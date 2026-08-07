// Package cli implements Forge's command-line interface: flag parsing,
// subcommand dispatch, and user-facing output formatting.
//
// Per SSOT §13.6 this package contains no business logic. Commands parse their
// arguments into a typed request and hand it to internal/runtime; all behaviour
// worth testing lives below this layer.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/runtime"
)

// Process exit codes, per SSOT §9.
const (
	// ExitOK reports successful completion.
	ExitOK = 0
	// ExitUsage reports a user error: bad flags, unknown command, unknown
	// container.
	ExitUsage = 1
	// ExitInternal reports an unexpected failure inside Forge.
	ExitInternal = 2
)

// Default filesystem locations, per SSOT §9.
const (
	// DefaultStateDir is where container metadata is persisted.
	DefaultStateDir = "/var/lib/forge"
	// DefaultRoot is where per-container root filesystems are stored.
	DefaultRoot = "/var/lib/forge/containers"
	// DefaultImageRoot is where downloaded image layers are cached. It is a
	// sibling of DefaultRoot rather than a child so the two can be pointed at
	// different filesystems.
	DefaultImageRoot = runtime.DefaultImageRoot
)

// ErrUsage marks an error as caused by how the user invoked Forge rather than
// by a failure inside it. It selects ExitUsage over ExitInternal; wrap it with
// %w to report a user error from a command.
var ErrUsage = errors.New("invalid usage")

// ExitError reports an error that must map to a specific exit code, overriding
// the ExitUsage/ExitInternal classification.
//
// It exists because `forge run` propagates the container's own exit status
// (FR-1.4): a container that exits 3 must make forge exit 3. A nil Err carries
// a status with nothing to report, and prints no message.
type ExitError struct {
	// Code is the exit code forge terminates with.
	Code int
	// Err is the underlying cause, or nil when the code is simply a
	// container's exit status.
	Err error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *ExitError) Unwrap() error { return e.Err }

// Options holds the global settings parsed from Forge's root-level flags.
type Options struct {
	// LogLevel is the minimum severity written to stderr.
	LogLevel slog.Level
	// StateDir is the directory holding persisted container metadata.
	StateDir string
	// Root is the directory holding per-container root filesystems.
	Root string
	// ImageRoot is the directory holding the cache of downloaded image layers.
	ImageRoot string
}

// Env carries the dependencies a command needs to do its work. It exists so
// commands never reach for os.Stdout, os.Args, or a package-level logger,
// keeping them testable in-process (SSOT §4, §13.4).
type Env struct {
	// Opts holds the global flags this invocation was given.
	Opts Options
	// Logger writes diagnostics to stderr.
	Logger *slog.Logger
	// Stdin is the process's input, handed to a container that wants it.
	Stdin io.Reader
	// Stdout receives machine-readable output, such as container IDs.
	Stdout io.Writer
	// Stderr receives human-readable detail.
	Stderr io.Writer
}

// Command is a single forge subcommand.
type Command struct {
	// Name is the verb the user types, such as "run".
	Name string
	// Summary is the one-line description shown in help output.
	Summary string
	// Exec runs the command with its own arguments, which exclude the global
	// flags and the command name itself.
	Exec func(ctx context.Context, env *Env, args []string) error

	// Hidden keeps the command out of help output. It is reserved for
	// Forge's internal re-exec entry point, which users never invoke.
	Hidden bool
}

// commands returns the subcommands Forge currently implements.
//
// Stage 1 delivers "run"; the remaining verbs in SSOT §9 arrive with their own
// stages. No verb is registered before its stage implements it, so there are no
// stubs that report "not implemented".
func commands() []Command {
	return []Command{
		newRunCommand(),
		newPsCommand(),
		newStopCommand(),
		newExecCommand(),
		newLogsCommand(),
		newRemoveCommand(),
		newInitCommand(),
	}
}

// newRunner builds the Runner a command works through, from the global flags.
//
// It is shared by every verb so that they all agree on where Forge's state
// lives: a `forge ps` looking in a different directory from the `forge run`
// that created the container would report nothing and be very hard to explain.
func newRunner(env *Env) (*runtime.Runner, error) {
	return runtime.NewRunner(env.Logger, runtime.Config{
		Root:      env.Opts.Root,
		StateDir:  env.Opts.StateDir,
		ImageRoot: env.Opts.ImageRoot,
	})
}

// Run executes a single forge invocation and returns the process exit code.
// args are the arguments after the program name.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{commands: commands(), stdin: stdin, stdout: stdout, stderr: stderr}
	return a.run(ctx, args)
}

// app is the dispatcher behind Run. Holding the command set as a field rather
// than reaching for a package-level registry keeps dispatch testable while the
// real registry is still empty.
type app struct {
	commands []Command
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
}

// run maps the outcome of an invocation onto an exit code, and is the single
// place where errors are formatted for the terminal (SSOT §5).
func (a *app) run(ctx context.Context, args []string) int {
	err := a.exec(ctx, args)

	// A specific exit code wins over the general classification below, so a
	// container's own status survives (FR-1.4).
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		if exitErr.Err != nil {
			fmt.Fprintf(a.stderr, "forge: %v\n", exitErr.Err)
		}
		return exitErr.Code
	}

	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, flag.ErrHelp):
		a.writeUsage(a.stdout)
		return ExitOK
	case errors.Is(err, ErrUsage):
		fmt.Fprintf(a.stderr, "forge: %v\n\n", err)
		a.writeUsage(a.stderr)
		return ExitUsage
	default:
		fmt.Fprintf(a.stderr, "forge: %v\n", err)
		return ExitInternal
	}
}

// exec parses global flags, resolves the subcommand, and runs it.
func (a *app) exec(ctx context.Context, args []string) error {
	opts, rest, err := parseGlobals(args)
	if err != nil {
		return err
	}

	if len(rest) == 0 {
		return fmt.Errorf("%w: no command given", ErrUsage)
	}

	name := rest[0]
	cmd := a.lookup(name)
	if cmd == nil {
		return fmt.Errorf("%w: unknown command %q", ErrUsage, name)
	}

	env := &Env{
		Opts:   opts,
		Logger: logging.New(a.stderr, opts.LogLevel),
		Stdin:  a.stdin,
		Stdout: a.stdout,
		Stderr: a.stderr,
	}
	return cmd.Exec(ctx, env, rest[1:])
}

// lookup returns the command registered under name, or nil.
func (a *app) lookup(name string) *Command {
	for i := range a.commands {
		if a.commands[i].Name == name {
			return &a.commands[i]
		}
	}
	return nil
}

// globalFlags holds the destinations of Forge's root-level flags.
type globalFlags struct {
	logLevel  string
	stateDir  string
	root      string
	imageRoot string
}

// newGlobalFlagSet builds the root-level flag set. Errors and usage are
// silenced because run formats both itself.
func newGlobalFlagSet() (*flag.FlagSet, *globalFlags) {
	var g globalFlags

	fs := flag.NewFlagSet("forge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.StringVar(&g.logLevel, "log-level", "info", "minimum log severity: debug, info, warn, or error")
	fs.StringVar(&g.stateDir, "state-dir", DefaultStateDir, "directory holding persisted container state")
	fs.StringVar(&g.root, "root", DefaultRoot, "directory holding container root filesystems")
	fs.StringVar(&g.imageRoot, "image-root", DefaultImageRoot, "directory holding the cache of downloaded image layers")

	return fs, &g
}

// parseGlobals parses and validates the root-level flags, returning them
// alongside the remaining arguments.
func parseGlobals(args []string) (Options, []string, error) {
	fs, g := newGlobalFlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Options{}, nil, err
		}
		return Options{}, nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}

	level, err := logging.ParseLevel(g.logLevel)
	if err != nil {
		return Options{}, nil, fmt.Errorf("%w: -log-level: %w", ErrUsage, err)
	}

	// Both directories are resolved against the host root by every later
	// stage, so a relative path would silently depend on the caller's working
	// directory. Reject it here rather than surfacing it as a confusing mount
	// or state-store failure much later.
	if !filepath.IsAbs(g.stateDir) {
		return Options{}, nil, fmt.Errorf("%w: -state-dir must be an absolute path, got %q", ErrUsage, g.stateDir)
	}
	if !filepath.IsAbs(g.root) {
		return Options{}, nil, fmt.Errorf("%w: -root must be an absolute path, got %q", ErrUsage, g.root)
	}
	if !filepath.IsAbs(g.imageRoot) {
		return Options{}, nil, fmt.Errorf("%w: -image-root must be an absolute path, got %q", ErrUsage, g.imageRoot)
	}

	opts := Options{
		LogLevel:  level,
		StateDir:  filepath.Clean(g.stateDir),
		Root:      filepath.Clean(g.root),
		ImageRoot: filepath.Clean(g.imageRoot),
	}
	return opts, fs.Args(), nil
}

// writeUsage prints Forge's help text to w.
func (a *app) writeUsage(w io.Writer) {
	fmt.Fprint(w, "Forge — a Docker-inspired container runtime.\n\n")
	fmt.Fprint(w, "Usage:\n  forge [global flags] <command> [args...]\n\n")

	var visible []Command
	for _, c := range a.commands {
		if !c.Hidden {
			visible = append(visible, c)
		}
	}
	if len(visible) > 0 {
		fmt.Fprint(w, "Commands:\n")
		for _, c := range visible {
			fmt.Fprintf(w, "  %-10s %s\n", c.Name, c.Summary)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprint(w, "Global flags:\n")
	fs, _ := newGlobalFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()
}
