//go:build integration

package integration

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/cgroup"
	"github.com/stevenstank/forge/internal/runtime"
	"github.com/stevenstank/forge/test/integration/testutil"
)

// Stage 3 asks the kernel to enforce something, and the only way to find out
// whether it did is to make a container misbehave and watch what happens. The
// unit tests already cover which bytes are written to which file; nothing here
// re-checks that. These tests are about consequences: a container that
// allocates too much is killed, one that forks too often is refused, one that
// spins is throttled, and none of them leaves a cgroup behind.
//
// A cgroup's counters die with the cgroup, so most of these tests hold the
// container open — see testutil.Live — read the kernel's own accounting while
// it is still there, and only then let it exit.

// The helper modes Stage 3 adds. Each is this test binary re-executed inside
// the container; see harness_test.go.
const (
	stage3ModeHold          = "stage3-hold"
	stage3ModeAllocate      = "stage3-allocate"
	stage3ModeAllocateChild = "stage3-allocate-child"
	stage3ModeAllocateNow   = "stage3-allocate-now"
	stage3ModeFork          = "stage3-fork"
	stage3ModeBlock         = "stage3-block"
	stage3ModeSpin          = "stage3-spin"
	stage3ModeIgnoreSigterm = "stage3-ignore-sigterm"
)

// goMarker is what a test sends once it has finished preparing the container's
// cgroup and the container may start misbehaving.
const goMarker = "go"

// maxForkAttempts bounds the fork bomb. If pids.max were not being enforced —
// which is precisely what this suite is here to detect — an unbounded loop
// would be a real fork bomb on the developer's machine.
const maxForkAttempts = 200

// stage3Helper runs the container side of the Stage 3 tests.
func stage3Helper(mode string) (int, bool) {
	switch mode {
	case stage3ModeHold:
		announceReady()
		waitForEOF()
		return 0, true

	case stage3ModeAllocate:
		announceReady()
		if !waitForGo() {
			return 0, true
		}
		allocate(helperDataInt())
		fmt.Println("allocated")
		waitForEOF()
		return 0, true

	case stage3ModeAllocateNow:
		// No protocol: this mode exists only to be the process the kernel
		// picks as the OOM victim.
		allocate(helperDataInt())
		return 0, true

	case stage3ModeAllocateChild:
		announceReady()
		if !waitForGo() {
			return 0, true
		}
		fmt.Println(describeChildStatus(runChild(stage3ModeAllocateNow, os.Getenv(helperDataEnv))))
		waitForEOF()
		return 0, true

	case stage3ModeFork:
		announceReady()
		if !waitForGo() {
			return 0, true
		}
		forkUntilRefused()
		waitForEOF()
		return 0, true

	case stage3ModeBlock:
		// A forked child that stays alive, so it keeps counting against
		// pids.max, and dies with the container's PID namespace.
		waitForEOF()
		return 0, true

	case stage3ModeSpin:
		announceReady()
		if !waitForGo() {
			return 0, true
		}
		for range 2 {
			go func() {
				for {
					// A tight loop the compiler cannot elide.
					_ = strconv.Itoa(os.Getpid())
				}
			}()
		}
		fmt.Println("spinning")
		waitForEOF()
		return 0, true

	case stage3ModeIgnoreSigterm:
		// Runs on the host, not in a container: the survivor internal/cgroup
		// has to evict before it can remove a leaf. It refuses to die politely.
		signal.Ignore(syscall.SIGTERM)
		announceReady()
		waitForEOF()
		return 0, true
	}

	return 0, false
}

// containerStdin is the reader the helper protocol runs over. It is a single
// buffered reader because waitForGo and waitForEOF must not lose bytes to each
// other's buffering.
var containerStdin = bufio.NewReader(os.Stdin)

func announceReady() { fmt.Println(readyMarker) }

// waitForGo blocks until the test says the cgroup is ready to be tested. It
// reports false if the test gave up and closed stdin instead.
func waitForGo() bool {
	line, err := containerStdin.ReadString('\n')
	return err == nil && strings.TrimSpace(line) == goMarker
}

// waitForEOF blocks until the test closes the container's stdin, which is how
// every long-running helper is told to exit.
func waitForEOF() { _, _ = io.Copy(io.Discard, containerStdin) }

// helperDataInt reads the numeric argument a test passed through the
// environment.
func helperDataInt() int {
	value, err := strconv.Atoi(os.Getenv(helperDataEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: bad %s: %v\n", helperDataEnv, err)
		os.Exit(unknownHelperExitCode)
	}
	return value
}

// allocate touches bytes of anonymous memory and keeps it referenced, so the
// kernel must actually back it with pages.
func allocate(bytes int) {
	const chunk = 1 << 20

	held := make([][]byte, 0, bytes/chunk+1)
	for allocated := 0; allocated < bytes; allocated += chunk {
		block := make([]byte, chunk)
		// Writing one byte per page is what turns the reservation into
		// resident memory the memory controller charges for.
		for i := 0; i < len(block); i += os.Getpagesize() {
			block[i] = 1
		}
		held = append(held, block)
	}

	// Keep the allocation alive past any optimisation.
	fmt.Fprintf(io.Discard, "%d", len(held))
}

// runChild re-executes this binary in another helper mode and waits for it.
func runChild(mode, data string) error {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}

	cmd := exec.Command(exe)
	cmd.Env = []string{helperEnv + "=" + mode, helperDataEnv + "=" + data, "GOMAXPROCS=2"}
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// describeChildStatus renders how a child died, in a line the test can parse.
func describeChildStatus(err error) string {
	if err == nil {
		return "child exit=0 signal=none"
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return fmt.Sprintf("child exit=%d signal=%d", 128+int(status.Signal()), int(status.Signal()))
		}
		return fmt.Sprintf("child exit=%d signal=none", exitErr.ExitCode())
	}

	return "child error=" + err.Error()
}

// forkUntilRefused starts children until the kernel refuses, then reports what
// happened in a line the test can parse.
func forkUntilRefused() {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}

	// Every child blocks reading this pipe, which is never written to, so they
	// stay alive and keep counting against pids.max without sleeping.
	reader, writer, err := os.Pipe()
	if err != nil {
		fmt.Printf("forked=0 eagain=false error=%v\n", err)
		return
	}
	defer func() { _ = writer.Close() }()

	started := 0
	for range maxForkAttempts {
		cmd := exec.Command(exe)
		cmd.Env = []string{helperEnv + "=" + stage3ModeBlock, "GOMAXPROCS=2"}
		cmd.Stdin = reader

		if err := cmd.Start(); err != nil {
			fmt.Printf("forked=%d eagain=%v error=%v\n", started, errors.Is(err, syscall.EAGAIN), err)
			return
		}
		started++
	}

	fmt.Printf("forked=%d eagain=false error=none\n", started)
}

// --- tests ----------------------------------------------------------------

// stage3Spec returns a Spec that runs this binary in mode, limited by limits.
//
// GOMAXPROCS is pinned because the pids controller counts threads, not
// processes: a Go program on a 64-core host would otherwise spend a pids budget
// of 16 on its own scheduler before forking anything.
func stage3Spec(t *testing.T, mode string, limits cgroup.Limits, env ...string) runtime.Spec {
	t.Helper()

	spec := helperSpec(t, mode, append([]string{"GOMAXPROCS=2"}, env...)...)
	spec.Limits = limits

	return spec
}

// startLimited starts a container, waits for it to report ready, and returns it
// with the path of the cgroup Forge created for it.
func startLimited(t *testing.T, spec runtime.Spec) (*testutil.Live, string) {
	t.Helper()

	before := testutil.Leaves(t)
	live := testutil.StartLive(t.Context(), t, spec)

	leaf := testutil.WaitForNewLeaf(t, before, testutil.DefaultTimeout)
	live.WaitForOutput(t, readyMarker, testutil.DefaultTimeout)

	return live, leaf
}

// TestMemoryLimitIsEnforced covers FR-3.2 against the kernel: a container that
// allocates past memory.max is OOM-killed, and the kernel records it.
//
// The two cases split the two assertions because they cannot both be made about
// the same process. A container whose init is killed cannot be inspected — its
// cgroup goes with it — so the second case puts the allocation in a child and
// keeps init alive to be read.
func TestMemoryLimitIsEnforced(t *testing.T) {
	requireRoot(t)
	testutil.RequireCgroupV2(t)
	t.Cleanup(func() { testutil.RemoveForgeCgroupIfEmpty(t) })

	const (
		limit    = 32 << 20  // memory.max
		allocate = 128 << 20 // four times the limit
	)

	tests := []struct {
		name   string
		mode   string
		assert func(t *testing.T, live *testutil.Live, leaf string)
	}{
		{
			name: "the container is killed when its init allocates past the limit",
			mode: stage3ModeAllocate,
			assert: func(t *testing.T, live *testutil.Live, _ string) {
				status := live.Wait(t, testutil.DefaultTimeout)

				if !status.Signaled() || status.Signal != syscall.SIGKILL {
					t.Errorf("container exited %v, want it killed by SIGKILL", status)
				}
				if status.Code != 137 {
					t.Errorf("exit code = %d, want 137 (128+SIGKILL)", status.Code)
				}
			},
		},
		{
			name: "the kernel records the kill in memory.events",
			mode: stage3ModeAllocateChild,
			assert: func(t *testing.T, live *testutil.Live, leaf string) {
				// The container's init survives, so the cgroup is still here to
				// be read after the allocation was killed.
				live.WaitForOutput(t, "child ", testutil.DefaultTimeout)

				events := filepath.Join(leaf, "memory.events")
				if got := testutil.KeyedValue(t, events, "oom_kill"); got < 1 {
					t.Errorf("%s: oom_kill = %d, want at least 1", events, got)
				}
				if got := testutil.KeyedValue(t, events, "max"); got < 1 {
					t.Errorf("%s: max = %d, want the limit to have been hit", events, got)
				}

				if got := live.Stdout.String(); !strings.Contains(got, "signal=9") {
					t.Errorf("the allocating process reported %q, want it killed by signal 9", strings.TrimSpace(got))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := cgroup.Bytes(limit)
			spec := stage3Spec(t, tt.mode, cgroup.Limits{MemoryMax: &memory},
				helperDataEnv+"="+strconv.Itoa(allocate))

			live, leaf := startLimited(t, spec)

			// FR-3.2 reached the kernel, not just the file.
			if got := testutil.ReadInt64(t, filepath.Join(leaf, "memory.max")); got != limit {
				t.Fatalf("memory.max = %d, want %d", got, limit)
			}

			// Swap is what would otherwise let the allocation succeed by
			// paging out instead of dying, so a memory limit carries a swap
			// limit. This assertion is the one that makes the OOM below
			// deterministic rather than dependent on whether the host has swap.
			swap := filepath.Join(leaf, "memory.swap.max")
			if _, err := os.Stat(swap); err == nil {
				if got := testutil.ReadInt64(t, swap); got != 0 {
					t.Fatalf("memory.swap.max = %d, want 0 alongside memory.max", got)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reading memory.swap.max: %v", err)
			} else {
				// A kernel built without swap accounting has no such file, and
				// no way for anyone to cap swap per cgroup.
				t.Log("this kernel has no memory.swap.max; the OOM below depends on the host having no swap")
			}

			live.Send(t, goMarker)
			tt.assert(t, live, leaf)
		})
	}
}

// TestPIDsLimitIsEnforced covers FR-3.4: a container that forks past pids.max
// is refused with EAGAIN, and the count never gets past the limit.
func TestPIDsLimitIsEnforced(t *testing.T) {
	requireRoot(t)
	testutil.RequireCgroupV2(t)
	t.Cleanup(func() { testutil.RemoveForgeCgroupIfEmpty(t) })

	const limit = 16

	procs := int64(limit)
	live, leaf := startLimited(t, stage3Spec(t, stage3ModeFork, cgroup.Limits{PIDsMax: &procs}))

	if got := testutil.ReadInt64(t, filepath.Join(leaf, "pids.max")); got != limit {
		t.Fatalf("pids.max = %d, want %d", got, limit)
	}

	// Sample the count throughout the fork loop rather than only at the end:
	// "never exceeds" is a claim about every moment, and the interesting
	// moments are the ones while the container is still forking.
	stop := make(chan struct{})
	stopSampling := sync.OnceFunc(func() { close(stop) })
	sampling := testutil.SamplePeak(filepath.Join(leaf, "pids.current"), stop)
	// Also on the failure paths below, so the sampler never outlives the test.
	defer stopSampling()

	live.Send(t, goMarker)
	live.WaitForOutput(t, "forked=", testutil.DefaultTimeout)

	stopSampling()
	highest := <-sampling
	if highest > limit {
		t.Errorf("pids.current peaked at %d, want no more than pids.max (%d)", highest, limit)
	}
	if highest == 0 {
		t.Error("pids.current never read above 0, so nothing was measured")
	}

	report := strings.TrimSpace(live.Stdout.String())
	if !strings.Contains(report, "eagain=true") {
		t.Errorf("fork loop reported %q, want it to have been refused with EAGAIN", report)
	}

	// The kernel's own record of the refusal.
	if got := testutil.KeyedValue(t, filepath.Join(leaf, "pids.events"), "max"); got < 1 {
		t.Errorf("pids.events: max = %d, want at least one refusal", got)
	}
	if got := testutil.ReadInt64(t, filepath.Join(leaf, "pids.current")); got > limit {
		t.Errorf("pids.current = %d, want no more than %d", got, limit)
	}
}

// TestCPUQuotaIsEnforced covers FR-3.3: a container that wants more CPU than
// cpu.max allows is throttled, and the kernel counts the throttling.
func TestCPUQuotaIsEnforced(t *testing.T) {
	requireRoot(t)
	testutil.RequireCgroupV2(t)
	t.Cleanup(func() { testutil.RemoveForgeCgroupIfEmpty(t) })

	// Half a core: two spinning goroutines cannot help but exceed it.
	quota := cgroup.CPUQuota{Quota: 50_000, Period: 100_000}
	live, leaf := startLimited(t, stage3Spec(t, stage3ModeSpin, cgroup.Limits{CPU: &quota}))

	cpuMax, err := os.ReadFile(filepath.Join(leaf, "cpu.max"))
	if err != nil {
		t.Fatalf("reading cpu.max: %v", err)
	}
	if got := strings.TrimSpace(string(cpuMax)); got != "50000 100000" {
		t.Fatalf("cpu.max = %q, want %q", got, "50000 100000")
	}

	stat := filepath.Join(leaf, "cpu.stat")
	before := testutil.KeyedValue(t, stat, "nr_throttled")

	live.Send(t, goMarker)
	live.WaitForOutput(t, "spinning", testutil.DefaultTimeout)

	// Throttling begins within one period (100ms) of the container saturating
	// its quota, but the deadline is generous: a loaded CI box may take longer
	// to schedule the spinners in the first place.
	testutil.PollUntil(t, "the container to be throttled", testutil.DefaultTimeout, func() bool {
		return testutil.KeyedValue(t, stat, "nr_throttled") > before
	})

	throttled := testutil.KeyedValue(t, stat, "nr_throttled")
	t.Logf("cpu.stat: nr_throttled %d → %d, throttled_usec %d, usage_usec %d",
		before, throttled,
		testutil.KeyedValue(t, stat, "throttled_usec"),
		testutil.KeyedValue(t, stat, "usage_usec"))

	if got := testutil.KeyedValue(t, stat, "throttled_usec"); got <= 0 {
		t.Errorf("throttled_usec = %d, want the container to have lost time to the quota", got)
	}
	if got := testutil.KeyedValue(t, stat, "usage_usec"); got <= 0 {
		t.Errorf("usage_usec = %d, want the container to have used some CPU", got)
	}

	// The quota is a ceiling, not a floor: the container must still be running
	// after being throttled, not killed by it.
	testutil.PollUntil(t, "the throttling to continue", testutil.DefaultTimeout, func() bool {
		return testutil.KeyedValue(t, stat, "nr_throttled") > throttled
	})
}

// TestContainerCgroupIsRemovedOnExit covers FR-3.1 and FR-3.5 end to end: the
// leaf exists while the container does, and is gone the moment Run returns.
func TestContainerCgroupIsRemovedOnExit(t *testing.T) {
	requireRoot(t)
	testutil.RequireCgroupV2(t)
	t.Cleanup(func() { testutil.RemoveForgeCgroupIfEmpty(t) })

	before := testutil.Leaves(t)

	memory := cgroup.Bytes(64 << 20)
	live, leaf := startLimited(t, stage3Spec(t, stage3ModeHold, cgroup.Limits{MemoryMax: &memory}))

	// FR-3.1: a leaf of its own, holding the container's init.
	if got := testutil.ReadInt64(t, filepath.Join(leaf, "pids.current")); got < 1 {
		t.Errorf("pids.current = %d, want the container's own processes", got)
	}

	live.CloseStdin(t)
	if status := live.Wait(t, testutil.DefaultTimeout); status.Code != 0 {
		t.Fatalf("container exited %v, want 0", status)
	}

	// FR-3.5: Run does not return until its cleanup has run, so this needs no
	// polling. The cgroup is gone by the time the caller sees the exit status.
	if _, err := os.Stat(leaf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists after the container exited: %v", leaf, err)
	}
	if got := testutil.Leaves(t); len(got) != len(before) {
		t.Errorf("cgroups under %s went from %v to %v", testutil.ForgeCgroup, before, got)
	}
}

// TestDestroyEvictsSurvivors covers the case FR-3.5 cannot assume away: a
// process still in the cgroup when Forge comes to remove it.
//
// rmdir(2) refuses a cgroup with members, so a Destroy that only removed the
// directory would leak a leaf — and the process in it — for good. The survivor
// here ignores SIGTERM, so only the eviction path can clear it.
func TestDestroyEvictsSurvivors(t *testing.T) {
	requireRoot(t)
	testutil.RequireCgroupV2(t)
	t.Cleanup(func() { testutil.RemoveForgeCgroupIfEmpty(t) })

	manager := cgroup.New(testutil.CgroupRoot)
	const id = "forge-integration-survivor"

	if err := manager.Create(id, cgroup.Limits{}); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	t.Cleanup(func() {
		// Idempotent, so running it after the test's own Destroy costs
		// nothing — and it is what keeps a failed test from leaking the leaf.
		if err := manager.Destroy(id); err != nil {
			t.Errorf("cleanup Destroy() = %v", err)
		}
	})

	leaf := filepath.Join(testutil.ForgeCgroup, id)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	stdin, stdinWriter := io.Pipe()
	survivor := exec.Command(exe)
	survivor.Env = []string{helperEnv + "=" + stage3ModeIgnoreSigterm, "GOMAXPROCS=2"}
	survivor.Stdin = stdin
	stdout := &testutil.SyncBuffer{}
	survivor.Stdout = stdout

	if err := survivor.Start(); err != nil {
		t.Fatalf("starting the survivor: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- survivor.Wait() }()
	t.Cleanup(func() {
		_ = stdinWriter.Close()
		_ = survivor.Process.Kill()
		<-waited
	})

	testutil.PollUntil(t, "the survivor to start", testutil.DefaultTimeout, func() bool {
		return strings.Contains(stdout.String(), readyMarker)
	})

	if err := manager.Add(id, survivor.Process.Pid); err != nil {
		t.Fatalf("Add() = %v", err)
	}
	if got := testutil.ReadInt64(t, filepath.Join(leaf, "pids.current")); got < 1 {
		t.Fatalf("pids.current = %d, want the survivor to have joined", got)
	}

	// It ignores the polite signal, which is what makes this a test of
	// eviction rather than of cooperation.
	if err := survivor.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signalling the survivor: %v", err)
	}
	select {
	case err := <-waited:
		t.Fatalf("the survivor exited on SIGTERM (%v); it was supposed to ignore it", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := manager.Destroy(id); err != nil {
		t.Fatalf("Destroy() with a survivor = %v", err)
	}

	// The survivor was killed, not merely detached.
	select {
	case err := <-waited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("the survivor exited with %v, want it killed", err)
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
			t.Errorf("the survivor exited %v, want SIGKILL", exitErr)
		}
	case <-time.After(testutil.DefaultTimeout):
		t.Fatal("the survivor outlived Destroy")
	}

	if _, err := os.Stat(leaf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists after Destroy: %v", leaf, err)
	}
}
