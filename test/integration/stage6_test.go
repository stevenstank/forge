//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/network"
	"github.com/stevenstank/forge/internal/runtime"
	"github.com/stevenstank/forge/test/integration/testutil"
)

// Stage 6, `forge exec`: running a second process inside a container that
// already exists.
//
// This is the stage's one genuinely new kernel mechanism. Everything else in
// Stage 6 reads and writes files — records, logs — and is tested without root
// in its own package. Joining namespaces cannot be: it needs CAP_SYS_ADMIN, a
// container that is actually running, and a kernel to disagree with.
//
// The interesting assertions are all comparisons of namespace identity. A
// namespace is named by the inode behind /proc/<pid>/ns/<kind>, so "the exec'd
// process is in the container's mount namespace" is a string equality against
// what the container itself reports — not an inference from the fact that a
// syscall returned zero.

// The namespaces exec joins, and the ones a Forge container actually gets.
//
// Forge creates four of the five: clone(2) is given CLONE_NEWPID, CLONE_NEWUTS,
// CLONE_NEWNS and CLONE_NEWNET, and no CLONE_NEWIPC. A container therefore
// shares the host's IPC namespace, and exec joining it is a no-op that is
// nonetheless correct — the requirement is that the command ends up where the
// container is, and for IPC that is the host. The distinction is kept explicit
// here so that if Forge later isolates IPC, this test starts asserting
// something and does not have to be rewritten to do it.
var (
	execNamespaces      = []string{"mnt", "pid", "net", "uts", "ipc"}
	isolatedNamespaces  = []string{"mnt", "pid", "net", "uts"}
	sharedWithHostNames = []string{"ipc"}
)

// stage6Helper dispatches the container-side and exec-side modes this stage
// needs.
func stage6Helper(mode string) (int, bool) {
	switch mode {
	case "print-namespaces":
		for _, kind := range execNamespaces {
			link, err := os.Readlink("/proc/self/ns/" + kind)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1, true
			}
			fmt.Printf("%s=%s\n", kind, link)
		}
		return 0, true

	case "print-exec-pid":
		fmt.Println(os.Getpid())
		return 0, true

	case "print-cgroup":
		data, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		fmt.Println(strings.TrimSpace(string(data)))
		return 0, true

	case "exec-exit":
		code, err := strconv.Atoi(os.Getenv(helperCodeEnv))
		if err != nil {
			return 253, true
		}
		return code, true

	case "exec-echo":
		fmt.Println(os.Getenv(helperDataEnv))
		return 0, true

	case "exec-sleep-forever":
		fmt.Println(readyMarker)
		select {}
	}

	return 0, false
}

// liveContainer starts a container that stays up, and returns the runner that
// owns it along with its ID.
//
// The ID comes from `forge ps` rather than from anywhere private: it is how a
// user finds a container, so it is how the tests find one too.
func liveContainer(ctx context.Context, t *testing.T) (*runtime.Runner, string, *testutil.Live) {
	t.Helper()

	runner := testutil.NewRunner(t)

	spec := helperSpec(t, "sleep-forever")

	// Overriding the harness default, which is host networking: every earlier
	// stage's container shares the host's network namespace, and a container
	// that shares it has nothing for exec to join. The assertions in this file
	// are equalities against the container's namespaces *and* inequalities
	// against the host's, and the second kind proves nothing for a namespace
	// the container never had.
	//
	// none rather than bridge, because what is needed here is CLONE_NEWNET and
	// nothing else: a private namespace with loopback in it, no bridge, no
	// address, no NAT. The exec'd helpers print namespace identities and read
	// /proc; none of them talks to anything.
	spec.Network = network.ModeNone
	live := testutil.StartLiveWithRunner(ctx, t, runner, spec)
	live.WaitForOutput(t, readyMarker, testutil.DefaultTimeout)

	var id string
	testutil.PollUntil(t, "the container to appear in forge ps", testutil.DefaultTimeout, func() bool {
		containers, errs := runner.List(false)
		if len(errs) != 0 {
			t.Fatalf("List() = %v", errs)
		}
		if len(containers) != 1 {
			return false
		}
		id = containers[0].ID
		return containers[0].Status == "running"
	})

	return runner, id, live
}

// execSpec returns an ExecSpec that runs this test binary in a helper mode
// inside the container.
//
// The container has no root filesystem of its own — these are Stage 1 style
// containers — so the test binary is at the same path inside it as outside,
// which is what makes it usable as the exec'd command.
func execSpec(t *testing.T, id, mode string, env ...string) runtime.ExecSpec {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	return runtime.ExecSpec{
		ID:      id,
		Command: []string{exe},
		Env:     append([]string{helperEnv + "=" + mode}, env...),
	}
}

// execOutput runs a command in the container and returns its stdout.
func execOutput(ctx context.Context, t *testing.T, runner *runtime.Runner, spec runtime.ExecSpec) (string, int) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	spec.Stdout = &stdout
	spec.Stderr = &stderr

	status, err := runner.Exec(ctx, spec)
	if err != nil {
		t.Fatalf("Exec() = %v\nstderr: %s", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), status.Code
}

// namespacesOf parses the helper's output into a kind → identity map.
func namespacesOf(t *testing.T, output string) map[string]string {
	t.Helper()

	found := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		kind, id, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			t.Fatalf("unparseable namespace line %q", line)
		}
		found[kind] = id
	}

	return found
}

// containerNamespaces reads a container's namespaces from the host, through
// the PID in its record.
func containerNamespaces(t *testing.T, runner *runtime.Runner, id string) map[string]string {
	t.Helper()

	c, err := runner.Inspect(id)
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if c.PID <= 0 {
		t.Fatalf("container %s has no recorded pid", id)
	}

	found := make(map[string]string)
	for _, kind := range execNamespaces {
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", c.PID, kind))
		if err != nil {
			t.Fatalf("reading the container's %s namespace: %v", kind, err)
		}
		found[kind] = link
	}

	return found
}

// TestExecFixtureContainerIsIsolated pins the premise every assertion below
// rests on: the container these tests exec into is in namespaces of its own.
//
// It is a test about the fixture rather than about exec, and it exists because
// the failure it guards against is silent. A container sharing one of the
// host's namespaces makes the corresponding "the exec joined the container"
// equality true for free — both sides read the host — so the suite goes on
// passing while proving nothing about the namespace in question. Asserting the
// separation directly is the only way that shows up as a failure, and it fails
// here rather than inside a test whose subject is something else.
func TestExecFixtureContainerIsIsolated(t *testing.T) {
	requireRoot(t)

	runner, id, _ := liveContainer(t.Context(), t)

	container := containerNamespaces(t, runner, id)
	for _, kind := range isolatedNamespaces {
		if host := hostNamespace(t, kind); container[kind] == host {
			t.Errorf("the container's %s namespace = %s, which is the host's: "+
				"exec has nothing to join and the assertions in this file prove nothing", kind, host)
		}
	}
}

// TestExecJoinsEveryNamespace is the requirement, stated as an equality: the
// command runs in the container's namespaces, not in the host's.
func TestExecJoinsEveryNamespace(t *testing.T) {
	requireRoot(t)

	runner, id, _ := liveContainer(t.Context(), t)

	container := containerNamespaces(t, runner, id)
	out, code := execOutput(t.Context(), t, runner, execSpec(t, id, "print-namespaces"))
	if code != 0 {
		t.Fatalf("the exec'd command exited %d", code)
	}
	got := namespacesOf(t, out)

	for _, kind := range execNamespaces {
		if got[kind] == "" {
			t.Errorf("the exec'd process reported no %s namespace", kind)
			continue
		}
		if got[kind] != container[kind] {
			t.Errorf("%s namespace = %s, want the container's %s", kind, got[kind], container[kind])
		}
	}

	// And the four Forge actually isolates are demonstrably not the host's,
	// so the equality above is proving something.
	for _, kind := range isolatedNamespaces {
		if host := hostNamespace(t, kind); got[kind] == host {
			t.Errorf("%s namespace = %s, which is the host's: the exec did not join the container", kind, host)
		}
	}

	// The one Forge does not isolate is the host's, in the container and in
	// the exec alike. Asserting it keeps the set honest.
	for _, kind := range sharedWithHostNames {
		if host := hostNamespace(t, kind); got[kind] != host {
			t.Errorf("%s namespace = %s, want the host's %s: forge creates no IPC namespace", kind, got[kind], host)
		}
	}
}

// TestExecRunsInTheContainersPIDNamespace checks the namespace that setns
// treats differently from the rest: joining it moves the caller's children
// rather than the caller, so the proof is what the child sees.
func TestExecRunsInTheContainersPIDNamespace(t *testing.T) {
	requireRoot(t)

	runner, id, _ := liveContainer(t.Context(), t)

	out, _ := execOutput(t.Context(), t, runner, execSpec(t, id, "print-exec-pid"))
	pid, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("the exec'd process printed %q, want a pid", out)
	}

	// A namespace-local PID, assigned from a namespace that has only the
	// container's init in it. On the host this process would have a PID in the
	// thousands; here it is the second or third process ever to exist.
	if pid <= 1 || pid > 100 {
		t.Errorf("the exec'd process sees itself as pid %d, want a small namespace-local pid", pid)
	}
	if pid == 1 {
		t.Error("the exec'd process is pid 1: it replaced the container's init rather than joining it")
	}
}

// TestExecJoinsTheContainerCgroup covers the isolation that is not a
// namespace: a process exec'd into a container counts against its limits.
func TestExecJoinsTheContainerCgroup(t *testing.T) {
	requireRoot(t)
	testutil.RequireCgroupV2(t)
	t.Cleanup(func() { testutil.RemoveForgeCgroupIfEmpty(t) })

	runner, id, _ := liveContainer(t.Context(), t)

	out, _ := execOutput(t.Context(), t, runner, execSpec(t, id, "print-cgroup"))
	want := "/forge/" + id
	if !strings.Contains(out, want) {
		t.Errorf("the exec'd process is in cgroup %q, want it to contain %q", out, want)
	}
}

// TestMultipleExecSessions runs several commands in one container, one after
// another. Each has to work as well as the first: the container is not
// consumed by being exec'd into.
func TestMultipleExecSessions(t *testing.T) {
	requireRoot(t)

	runner, id, live := liveContainer(t.Context(), t)

	for i := range 5 {
		want := fmt.Sprintf("session-%d", i)
		spec := execSpec(t, id, "exec-echo", helperDataEnv+"="+want)

		got, code := execOutput(t.Context(), t, runner, spec)
		if code != 0 {
			t.Fatalf("session %d exited %d", i, code)
		}
		if got != want {
			t.Errorf("session %d printed %q, want %q", i, got, want)
		}
	}

	// The container is untouched by all of it.
	assertStillRunning(t, runner, id, live)
}

// TestConcurrentExec runs several commands in the container at once.
//
// Each one locks an OS thread, unshares its filesystem context and joins five
// namespaces. Doing that concurrently is where a mistake in the thread
// discipline would show up as one exec landing in another's namespaces, or in
// the host's.
func TestConcurrentExec(t *testing.T) {
	requireRoot(t)

	runner, id, live := liveContainer(t.Context(), t)
	container := containerNamespaces(t, runner, id)

	const sessions = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make(map[int]map[string]string, sessions)
		failed  []error
	)

	for i := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var stdout, stderr bytes.Buffer
			spec := execSpec(t, id, "print-namespaces")
			spec.Stdout, spec.Stderr = &stdout, &stderr

			status, err := runner.Exec(t.Context(), spec)
			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				failed = append(failed, fmt.Errorf("session %d: %w (stderr: %s)", i, err, stderr.String()))
				return
			}
			if status.Code != 0 {
				failed = append(failed, fmt.Errorf("session %d exited %d", i, status.Code))
				return
			}
			results[i] = namespacesOf(t, stdout.String())
		}()
	}
	wg.Wait()

	for _, err := range failed {
		t.Error(err)
	}
	if len(results) != sessions {
		t.Fatalf("%d of %d sessions produced output", len(results), sessions)
	}

	// Every one of them landed in the same place: the container.
	for i, got := range results {
		for _, kind := range execNamespaces {
			if got[kind] != container[kind] {
				t.Errorf("session %d: %s namespace = %s, want the container's %s",
					i, kind, got[kind], container[kind])
			}
		}
	}

	assertStillRunning(t, runner, id, live)
}

// TestExecPropagatesTheExitCode covers what a script depends on: the command's
// own status, not a verdict on whether the exec worked.
func TestExecPropagatesTheExitCode(t *testing.T) {
	requireRoot(t)

	runner, id, live := liveContainer(t.Context(), t)

	for _, want := range []int{0, 1, 7, 42, 255} {
		t.Run(fmt.Sprintf("exit %d", want), func(t *testing.T) {
			spec := execSpec(t, id, "exec-exit", fmt.Sprintf("%s=%d", helperCodeEnv, want))

			var stdout, stderr bytes.Buffer
			spec.Stdout, spec.Stderr = &stdout, &stderr

			status, err := runner.Exec(t.Context(), spec)
			if err != nil {
				t.Fatalf("Exec() = %v\nstderr: %s", err, stderr.String())
			}
			if status.Code != want {
				t.Errorf("exit code = %d, want %d", status.Code, want)
			}
			if status.Signaled() {
				t.Errorf("status = %v, want an ordinary exit", status)
			}
		})
	}

	// A non-zero exit from the command is not a failure of the container.
	assertStillRunning(t, runner, id, live)
}

// TestExecAfterStopIsRefused is the requirement stated directly. It is also
// the safety property underneath it: a stopped container's recorded PID may
// by now belong to something else entirely, and exec must not join it.
func TestExecAfterStopIsRefused(t *testing.T) {
	requireRoot(t)

	runner, id, live := liveContainer(t.Context(), t)

	// It works before the stop, so the refusal afterwards is about the stop.
	if _, code := execOutput(t.Context(), t, runner, execSpec(t, id, "exec-echo")); code != 0 {
		t.Fatalf("exec before the stop exited %d", code)
	}

	if err := runner.Stop(t.Context(), id, runtime.StopOptions{Timeout: 2 * time.Second}); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	live.Wait(t, testutil.DefaultTimeout)

	var stdout, stderr bytes.Buffer
	spec := execSpec(t, id, "exec-echo")
	spec.Stdout, spec.Stderr = &stdout, &stderr

	_, err := runner.Exec(t.Context(), spec)
	if err == nil {
		t.Fatal("Exec() into a stopped container = nil, want an error")
	}
	if !isNotRunning(err) {
		t.Errorf("Exec() = %v, want a not-running error", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("the refused exec produced output: %q", stdout.String())
	}
}

// TestExecIntoARemovedContainerIsRefused covers the other end of the
// lifecycle: after `forge rm` there is no record, so there is nothing to join.
func TestExecIntoARemovedContainerIsRefused(t *testing.T) {
	requireRoot(t)

	runner, id, live := liveContainer(t.Context(), t)

	if err := runner.Stop(t.Context(), id, runtime.StopOptions{Timeout: 2 * time.Second, Remove: true}); err != nil {
		t.Fatalf("Stop(-rm) = %v", err)
	}
	live.Wait(t, testutil.DefaultTimeout)

	if _, err := runner.Exec(t.Context(), execSpec(t, id, "exec-echo")); err == nil {
		t.Fatal("Exec() into a removed container = nil, want an error")
	}
}

// TestExecProcessesDieWithTheContainer is the "no leaked processes"
// requirement, and it is the kernel's guarantee rather than Forge's: killing
// the init of a PID namespace makes the kernel SIGKILL every other process in
// it, and an exec'd command is in it.
func TestExecProcessesDieWithTheContainer(t *testing.T) {
	requireRoot(t)

	runner, id, live := liveContainer(t.Context(), t)

	// A command that would run forever if nothing killed it.
	var stdout testutil.SyncBuffer
	spec := execSpec(t, id, "exec-sleep-forever")
	spec.Stdout = &stdout

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		status, err := runner.Exec(t.Context(), spec)
		done <- result{code: status.Code, err: err}
	}()

	testutil.PollUntil(t, "the exec'd command to start", testutil.DefaultTimeout, func() bool {
		return strings.Contains(stdout.String(), readyMarker)
	})

	if err := runner.Stop(t.Context(), id, runtime.StopOptions{Timeout: 2 * time.Second}); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	live.Wait(t, testutil.DefaultTimeout)

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Exec() = %v", res.err)
		}
		// Killed rather than exited: 128+SIGKILL, the convention Forge uses
		// everywhere for a process a signal ended.
		if res.code != 137 {
			t.Errorf("the exec'd command exited %d, want 137 (killed by SIGKILL with the container)", res.code)
		}
	case <-time.After(testutil.DefaultTimeout):
		t.Fatal("the exec'd command outlived the container it was running in")
	}
}

// TestExecLeavesForgeInTheHostNamespaces is the "no namespace leaks"
// requirement, aimed at the process doing the exec rather than the one it
// starts.
//
// Joining a namespace is per-thread and permanent for that thread, so the
// discipline that keeps it contained — lock the thread, never unlock it, let
// the goroutine's exit destroy it — is the whole of what stops a `forge exec`
// from leaving parts of itself inside the container. If it were wrong, this
// process would be in the container's namespaces afterwards.
func TestExecLeavesForgeInTheHostNamespaces(t *testing.T) {
	requireRoot(t)

	before := make(map[string]string, len(execNamespaces))
	for _, kind := range execNamespaces {
		before[kind] = hostNamespace(t, kind)
	}

	runner, id, _ := liveContainer(t.Context(), t)

	for range 5 {
		if _, code := execOutput(t.Context(), t, runner, execSpec(t, id, "exec-echo")); code != 0 {
			t.Fatalf("exec exited %d", code)
		}
	}

	for _, kind := range execNamespaces {
		if got := hostNamespace(t, kind); got != before[kind] {
			t.Errorf("this process's %s namespace changed from %s to %s: exec leaked into forge",
				kind, before[kind], got)
		}
	}

	// The working directory is the other thing joining a mount namespace
	// replaces, and it is shared between threads unless the unshare worked.
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Errorf("this process's working directory is unusable after an exec: %v", err)
	}
}

// TestExecResolvesTheCommandInTheContainer covers PATH resolution happening on
// the far side of the mount-namespace join.
func TestExecResolvesTheCommandInTheContainer(t *testing.T) {
	requireRoot(t)

	runner, id, _ := liveContainer(t.Context(), t)

	// A bare name, resolved against the container's PATH. These containers
	// share the host's filesystem, so /bin/true is where it always was — what
	// is being checked is that a name rather than a path is accepted at all.
	spec := execSpec(t, id, "")
	spec.Command = []string{"true"}
	spec.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

	status, err := runner.Exec(t.Context(), spec)
	if err != nil {
		t.Fatalf("Exec() with a bare command name = %v", err)
	}
	if status.Code != 0 {
		t.Errorf("exit code = %d, want 0", status.Code)
	}

	// And a name that is nowhere on it fails as a command-not-found rather
	// than as something obscure.
	spec.Command = []string{"definitely-not-a-real-binary"}
	if _, err := runner.Exec(t.Context(), spec); err == nil {
		t.Error("Exec() of a command that does not exist = nil, want an error")
	}
}

// assertStillRunning checks that the container survived whatever was just done
// to it, which is the "preserve container lifecycle" requirement.
func assertStillRunning(t *testing.T, runner *runtime.Runner, id string, live *testutil.Live) {
	t.Helper()

	c, err := runner.Inspect(id)
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if c.Status != "running" {
		t.Errorf("container status = %q, want it still running", c.Status)
	}

	select {
	case <-live.Done():
		t.Error("the container exited while it was being exec'd into")
	default:
	}
}

// isNotRunning reports whether err is the refusal exec gives for a container
// that is not running. It matches on the message rather than the sentinel
// because the sentinels are internal/runtime's and this asserts on behaviour a
// user sees.
func isNotRunning(err error) bool {
	return strings.Contains(err.Error(), "not running") ||
		strings.Contains(err.Error(), "no such container")
}
