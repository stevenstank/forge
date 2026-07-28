package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/stevenstank/forge/internal/namespace"
)

// initPayloadFD is the descriptor the init payload arrives on. Go's exec
// package maps ExtraFiles[0] onto file descriptor 3 in the child, after the
// three standard streams.
const initPayloadFD = 3

// InitExitCode is the status Forge exits with when the container could not be
// started at all — a missing binary, an unreadable payload, a namespace that
// could not be configured.
//
// 127 is the shell's "command not found" convention, which Docker also uses. It
// is deliberately distinct from the exit codes in SSOT §9 so a container that
// fails to start is not mistaken for one that ran and exited.
const InitExitCode = 127

// envInitGuard marks a process as a container init that Forge started. It is
// set on the init process only, and is never passed to the container itself —
// execve replaces the environment with the Spec's.
const envInitGuard = "FORGE_CONTAINER_INIT"

// ErrNoInitPayload reports that the init process was started without the
// configuration pipe, which almost always means it was run by hand.
var ErrNoInitPayload = errors.New("no init payload: " + InitCommandName + " is internal and cannot be run directly")

// ErrNestedInit reports that Run was called from inside a container init that
// never dispatched to Init.
//
// Run re-executes the current binary as InitCommandName, so any program using
// this package must route that command to Init (see IsInitCommand). A program
// that does not would re-enter its own main, start another container, and
// repeat — a fork bomb. This turns that into an immediate, explicit failure.
var ErrNestedInit = errors.New(
	"refusing to start a container from within a container init: " +
		"this binary re-executed itself but did not dispatch the " + InitCommandName +
		" command to runtime.Init")

// IsInitCommand reports whether an argument vector — normally os.Args — selects
// Forge's internal container init.
//
// Any binary that calls Run must consult this early in main and hand off to
// Init when it returns true:
//
//	if runtime.IsInitCommand(os.Args) {
//		if err := runtime.Init(); err != nil {
//			fmt.Fprintln(os.Stderr, err)
//			os.Exit(runtime.InitExitCode)
//		}
//	}
//
// Forge's own CLI does this by registering InitCommandName as a hidden command.
func IsInitCommand(args []string) bool {
	return len(args) > 1 && args[1] == InitCommandName
}

// initPayload is the configuration handed from the parent to the container's
// init process across the re-exec boundary.
type initPayload struct {
	Namespace namespace.Config `json:"namespace"`
	Command   []string         `json:"command"`
	Env       []string         `json:"env"`
}

// Init is the container's entry point, executed by the re-exec'd forge binary
// inside the new namespaces created by clone(2).
//
// It must only ever run in that child process: the whole point of the
// operations it performs is that they affect the caller's namespaces, so
// running it anywhere else would reconfigure the host.
//
// On success Init does not return — execve replaces this process with the
// container's binary, which inherits its PID. Inside a PID namespace that PID
// is 1.
func Init() error {
	payload, err := readInitPayload()
	if err != nil {
		return err
	}

	if err := namespace.Apply(payload.Namespace); err != nil {
		return err
	}

	if err := syscall.Exec(payload.Command[0], payload.Command, payload.Env); err != nil {
		return fmt.Errorf("executing %s: %w", payload.Command[0], err)
	}

	// Unreachable: a successful execve never returns.
	return errors.New("execve returned without an error")
}

// readInitPayload reads and decodes the configuration the parent wrote to the
// inherited pipe.
func readInitPayload() (initPayload, error) {
	f := os.NewFile(initPayloadFD, "forge-init-payload")
	if f == nil {
		return initPayload{}, ErrNoInitPayload
	}
	payload, readErr := decodeInitPayload(f)

	// Close before returning, and therefore before execve: descriptors survive
	// execve, and Forge's plumbing must not leak into the container.
	closeErr := f.Close()

	if readErr != nil {
		return initPayload{}, readErr
	}
	if closeErr != nil {
		return initPayload{}, fmt.Errorf("closing init payload descriptor: %w", closeErr)
	}

	return payload, nil
}

// decodeInitPayload reads and validates the configuration the parent sent. It
// takes an io.Reader rather than the descriptor so the wire format can be
// tested without a real pipe.
func decodeInitPayload(r io.Reader) (initPayload, error) {
	encoded, err := io.ReadAll(r)
	if err != nil {
		return initPayload{}, fmt.Errorf("reading init payload: %w", err)
	}
	if len(encoded) == 0 {
		return initPayload{}, ErrNoInitPayload
	}

	var payload initPayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return initPayload{}, fmt.Errorf("decoding init payload: %w", err)
	}
	if len(payload.Command) == 0 {
		return initPayload{}, ErrNoCommand
	}
	if payload.Command[0] == "" {
		return initPayload{}, ErrNoCommand
	}

	return payload, nil
}
