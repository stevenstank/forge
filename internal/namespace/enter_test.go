package namespace_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/stevenstank/forge/internal/namespace"
)

// Namespace entry, tested without root.
//
// Joining a namespace needs CAP_SYS_ADMIN in the user namespace that owns it,
// which normally means these tests would be integration tests and this file
// would not exist. There is a way round it: an *unprivileged* user namespace
// makes its creator root inside it, and a process created in one owns the
// namespaces it creates alongside — so a test can build a small target to join
// and have the rights to join it.
//
// That is worth the scaffolding below, because the mechanism this exercises is
// the one most likely to be wrong and the hardest to reason about from
// reading: a multithreaded Go process moving into another process's mount
// namespace, which is widely and almost correctly believed to be impossible.
// The "almost" is unshare(CLONE_FS), and this is what proves it.
//
// The test skips where unprivileged user namespaces are unavailable — some
// distributions disable them — so a host that cannot run it says so rather
// than failing.

const (
	helperEnv    = "FORGE_NAMESPACE_TEST_HELPER"
	targetPIDEnv = "FORGE_NAMESPACE_TEST_TARGET_PID"
	wantEnvBase  = "FORGE_NAMESPACE_TEST_WANT_"
)

// joinable is the set the helper exercises. The PID namespace is deliberately
// absent: setns with CLONE_NEWPID moves the caller's *children* rather than
// the caller, so a process cannot demonstrate the join by looking at itself.
// It is covered by test/integration/stage6_test.go, which has a real container
// and a real child to look at.
var joinable = []namespace.Kind{
	namespace.KindIPC,
	namespace.KindUTS,
	namespace.KindNetwork,
	namespace.KindMount,
}

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "target":
		os.Exit(runTarget())
	case "joiner":
		os.Exit(runJoiner())
	case "naive-target":
		os.Exit(runNaiveTarget())
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

// nsID returns the identity of one of the calling *thread's* namespaces.
//
// /proc/thread-self rather than /proc/self, and the difference is the whole
// point: setns moves one thread, so the process's view — which is the thread
// group leader's — would still show the old namespace and the test would fail
// while the code was right.
func nsID(kind namespace.Kind) string {
	link, err := os.Readlink("/proc/thread-self/ns/" + string(kind))
	if err != nil {
		return "unreadable:" + err.Error()
	}
	return link
}

// runTarget plays the container: it holds the namespaces to be joined, and
// starts the joiner in different ones.
func runTarget() int {
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(),
		helperEnv+"=joiner",
		targetPIDEnv+"="+strconv.Itoa(os.Getpid()),
	)
	for _, kind := range joinable {
		cmd.Env = append(cmd.Env, wantEnvBase+strings.ToUpper(string(kind))+"="+nsID(kind))
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

	// The joiner starts in namespaces of its own, so ending up in the
	// target's proves setns put it there rather than inheritance.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC,
	}

	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "joiner:", err)
		return 1
	}

	return 0
}

// runJoiner is the process under test: multithreaded, in its own namespaces,
// asked to move into the target's.
func runJoiner() int {
	pid, err := strconv.Atoi(os.Getenv(targetPIDEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "no target pid:", err)
		return 1
	}

	for _, kind := range joinable {
		if nsID(kind) == os.Getenv(wantEnvBase+strings.ToUpper(string(kind))) {
			fmt.Fprintf(os.Stderr, "joiner already shares the target's %s namespace\n", kind)
			return 1
		}
	}

	handles, err := namespace.Open(pid, joinable...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Open:", err)
		return 1
	}
	defer func() { _ = namespace.CloseAll(handles) }()

	// Make the process unmistakably multithreaded. The Go runtime is already,
	// but a test that depends on that implicitly would stop testing anything
	// if it ever changed.
	for range 4 {
		go func() { select {} }()
	}

	result := make(chan int, 1)
	go func() {
		goruntime.LockOSThread()

		if err := namespace.Enter(handles); err != nil {
			fmt.Fprintln(os.Stderr, "Enter:", err)
			result <- 1
			return
		}

		code := 0
		for _, kind := range joinable {
			want := os.Getenv(wantEnvBase + strings.ToUpper(string(kind)))
			if got := nsID(kind); got != want {
				fmt.Fprintf(os.Stderr, "%s namespace = %s, want %s\n", kind, got, want)
				code = 1
			}
		}
		result <- code
	}()

	return <-result
}

// runNaiveTarget attempts the setns Enter is careful not to attempt: a mount
// namespace joined without first unsharing the thread's filesystem context.
//
// It succeeds (exit 0) when the kernel refuses with EINVAL, which is the
// behaviour Enter is built around.
func runNaiveTarget() int {
	f, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "opening own mount namespace:", err)
		return 1
	}
	defer func() { _ = f.Close() }()

	for range 4 {
		go func() { select {} }()
	}

	result := make(chan int, 1)
	go func() {
		goruntime.LockOSThread()

		err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNS)
		if errors.Is(err, unix.EINVAL) {
			result <- 0
			return
		}
		fmt.Fprintf(os.Stderr,
			"setns(CLONE_NEWNS) without unshare(CLONE_FS) returned %v, want EINVAL\n", err)
		result <- 1
	}()

	return <-result
}

// requireUserNamespaces skips when the host will not let an unprivileged
// process create a user namespace, which is what these tests stand on.
func requireUserNamespaces(t *testing.T) {
	t.Helper()

	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), helperEnv+"=unknown")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Skipf("unprivileged user namespaces are unavailable: %v", err)
	}
}

// TestEnterJoinsAnotherProcessNamespaces is the claim that makes `forge exec`
// possible in a runtime written in Go: a multithreaded process can join a
// mount namespace, provided the thread doing it first unshares its filesystem
// context.
//
// Without the unshare, the setns below fails with EINVAL — the kernel's
// mntns_install refuses when the caller's fs_struct is shared, and every
// thread the Go runtime creates shares one. TestEnterFailsWithoutUnsharingFS
// pins that half.
func TestEnterJoinsAnotherProcessNamespaces(t *testing.T) {
	requireUserNamespaces(t)

	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), helperEnv+"=target")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// The user namespace is what supplies the capabilities; the other
		// four are the namespaces the joiner will be asked to move into.
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS |
			syscall.CLONE_NEWUTS | syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	if err := cmd.Run(); err != nil {
		t.Fatalf("joining the target's namespaces failed: %v", err)
	}
}

// TestEnterFailsWithoutUnsharingFS documents the constraint Enter works
// around, by reproducing it. If a future kernel drops the fs_struct check this
// test starts failing, which is the moment to find out — not when the unshare
// is quietly removed as unnecessary.
func TestEnterFailsWithoutUnsharingFS(t *testing.T) {
	requireUserNamespaces(t)

	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), helperEnv+"=naive-target")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	if err := cmd.Run(); err != nil {
		t.Fatalf("the naive setns did not fail the way it was expected to: %v", err)
	}
}

func TestKindPathAndValidity(t *testing.T) {
	if got := namespace.KindMount.Path(4242); got != "/proc/4242/ns/mnt" {
		t.Errorf("Path() = %q, want /proc/4242/ns/mnt", got)
	}

	for _, kind := range namespace.EntryOrder {
		if !kind.Valid() {
			t.Errorf("%q is in EntryOrder but is not valid", kind)
		}
	}
	if namespace.Kind("sideways").Valid() {
		t.Error("an unknown namespace reports as valid")
	}

	// The mount namespace is joined last, because joining it replaces the
	// thread's root and every path after it resolves inside the container.
	if last := namespace.EntryOrder[len(namespace.EntryOrder)-1]; last != namespace.KindMount {
		t.Errorf("EntryOrder ends with %q, want the mount namespace last", last)
	}
}

func TestOpenRejectsBadInput(t *testing.T) {
	if _, err := namespace.Open(0, namespace.KindMount); err == nil {
		t.Error("Open(0) = nil, want an error")
	}
	if _, err := namespace.Open(os.Getpid(), namespace.Kind("sideways")); err == nil {
		t.Error("Open(unknown kind) = nil, want an error")
	}

	// A PID no process can have: nothing to open, and nothing left open.
	if _, err := namespace.Open(1<<30, namespace.KindMount); !errors.Is(err, namespace.ErrNoSuchNamespace) {
		t.Errorf("Open(missing pid) = %v, want ErrNoSuchNamespace", err)
	}
}

func TestOpenAndCloseAll(t *testing.T) {
	handles, err := namespace.Open(os.Getpid(), namespace.EntryOrder...)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if len(handles) != len(namespace.EntryOrder) {
		t.Fatalf("Open() returned %d handles, want %d", len(handles), len(namespace.EntryOrder))
	}

	// Idempotent, so a caller can defer it and still close on an error path.
	for i := range 2 {
		if err := namespace.CloseAll(handles); err != nil {
			t.Fatalf("CloseAll() call %d = %v", i+1, err)
		}
	}
}
