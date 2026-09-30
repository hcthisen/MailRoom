// Command mailroom is the single Mailroom binary: the server plus the
// bootstrap, provisioning and offline maintenance commands (PRD §6.13).
//
// Exit codes: 0 ok, 1 error, 2 check failed.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], &Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv})
	stop()
	os.Exit(code)
}

// run dispatches args to a registered command and returns the exit code.
func run(ctx context.Context, args []string, env *Env) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(env.Stdout)
		if len(args) == 0 {
			return ExitError
		}
		return ExitOK
	}
	cmd, ok := lookup(args[0])
	if !ok {
		fmt.Fprintf(env.Stderr, "mailroom: unknown command %q\n\n", args[0])
		usage(env.Stderr)
		return ExitError
	}
	err := cmd.Run(ctx, env, args[1:])
	if err == nil {
		return ExitOK
	}
	var ee *ExitCodeError
	if errors.As(err, &ee) {
		if ee.Err != nil {
			fmt.Fprintf(env.Stderr, "mailroom %s: %v\n", cmd.Name, ee.Err)
		}
		return ee.Code
	}
	fmt.Fprintf(env.Stderr, "mailroom %s: %v\n", cmd.Name, err)
	return ExitError
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: mailroom <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands() {
		if c.Hidden {
			continue
		}
		fmt.Fprintf(w, "  %-10s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'mailroom <command> -h' for command flags. Every command accepts --json.")
}
