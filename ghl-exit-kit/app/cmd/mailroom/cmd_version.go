package main

import (
	"context"
	"runtime"
)

func init() {
	register(Command{Name: "version", Summary: "print the Mailroom version", Run: runVersion})
}

func runVersion(_ context.Context, env *Env, args []string) error {
	fs, jsonOut := newFlagSet(env, "version")
	if err := fs.Parse(args); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	return printResult(env, *jsonOut,
		map[string]string{"version": version, "go": runtime.Version()},
		"mailroom "+version)
}
