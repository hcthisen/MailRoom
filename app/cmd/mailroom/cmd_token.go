package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/core"
)

func init() {
	register(Command{Name: "token", Summary: "bootstrap API tokens (token create --name X --scope admin|read)", Run: runToken})
}

// tokenCreateResult is the --json output of "token create".
type tokenCreateResult struct {
	Token  auth.Token `json:"token"`
	Secret string     `json:"secret"`
}

func runToken(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return &ExitCodeError{Code: ExitError, Err: errors.New("usage: mailroom token create --name NAME --scope admin|read [--expires-in 90d] [--json]")}
	}
	fs, jsonOut := newFlagSet(env, "token create")
	name := fs.String("name", "", "unique token name (required)")
	scope := fs.String("scope", "", "admin (everything) or read (GET only) (required)")
	expiresIn := fs.String("expires-in", "", "optional lifetime, e.g. 90d or 12h; default never")
	if err := fs.Parse(args[1:]); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	if *name == "" {
		return &ExitCodeError{Code: ExitError, Err: errors.New("--name is required")}
	}
	sc := api.Scope(*scope)
	if !sc.Valid() {
		return &ExitCodeError{Code: ExitError, Err: errors.New("--scope must be admin or read")}
	}
	in := auth.TokenInput{Name: *name, Scope: sc}
	if *expiresIn != "" {
		d, err := parseLifetime(*expiresIn)
		if err != nil {
			return &ExitCodeError{Code: ExitError, Err: err}
		}
		t := core.SystemClock{}.Now().Add(d)
		in.ExpiresAt = &t
	}
	_, d, err := openBootstrap(ctx, env)
	if err != nil {
		return err
	}
	defer d.Close()

	tok, secret, err := app.CreateToken(ctx, d, in)
	if err != nil {
		return err
	}
	if !*jsonOut {
		fmt.Fprintf(env.Stderr, "created %s token %q (id %d); it is shown only once:\n", tok.Scope, tok.Name, tok.ID)
	}
	return printResult(env, *jsonOut, tokenCreateResult{Token: tok, Secret: secret}, secret)
}

// parseLifetime parses a Go duration or a whole number of days ("90d").
func parseLifetime(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("--expires-in: %q is not a number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--expires-in: %q is not a positive duration such as 90d or 12h", s)
	}
	return d, nil
}
