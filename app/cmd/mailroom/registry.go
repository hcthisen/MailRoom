package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
)

// Exit codes (PRD §6.13).
const (
	ExitOK          = 0
	ExitError       = 1
	ExitCheckFailed = 2
)

// ExitCodeError makes a command exit with Code, printing Err if non-nil.
type ExitCodeError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *ExitCodeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *ExitCodeError) Unwrap() error { return e.Err }

// Env is what a command may touch of the process: standard streams and
// the environment. Tests substitute it. A nil Stdin reads as empty.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
}

// Command is one subcommand. Each file registers its commands with
// register in an init function, so later packages' commands are added by
// adding a file.
type Command struct {
	Name    string
	Summary string
	// Hidden commands work but are not listed in the usage text (e.g. the
	// container health check, which people don't run themselves).
	Hidden bool
	Run    func(ctx context.Context, env *Env, args []string) error
}

var (
	registryMu sync.Mutex
	registry   = map[string]Command{}
)

// register adds a command; registering a name twice panics at start-up.
func register(c Command) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[c.Name]; dup {
		panic("mailroom: command registered twice: " + c.Name)
	}
	registry[c.Name] = c
}

func lookup(name string) (Command, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	c, ok := registry[name]
	return c, ok
}

// commands returns the registered commands sorted by name.
func commands() []Command {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Command, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// newFlagSet returns a flag set for a command with the common --json flag.
func newFlagSet(env *Env, name string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet("mailroom "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	return fs, jsonOut
}

// printResult prints v as indented JSON when jsonOut is set, else text.
func printResult(env *Env, jsonOut bool, v any, text string) error {
	if jsonOut {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	_, err := fmt.Fprintln(env.Stdout, text)
	return err
}

// newLogger returns the JSON logger of long-running commands, writing to
// the command's standard output (the container log).
func newLogger(env *Env, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(env.Stdout, &slog.HandlerOptions{Level: level}))
}

// newStderrLogger returns a logger for commands whose standard output is
// their result (text or --json): log lines go to standard error.
func newStderrLogger(env *Env) *slog.Logger {
	return slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// parseInterspersed parses args with fs, allowing flags after positional
// arguments ("restore FILE --json"), and returns the positional arguments.
// A lone "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if len(args) > 0 && len(rest) < len(args) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}
