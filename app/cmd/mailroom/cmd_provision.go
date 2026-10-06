package main

import (
	"context"

	"mailroom/internal/provision"
)

func init() {
	register(Command{Name: "provision", Summary: "provision SES/SNS/IAM (provision ses [--request-production]) or DNS (provision dns --cloudflare)", Run: runProvision})
	register(Command{Name: "dns", Summary: "print the DNS records to create by hand (dns records)", Run: runDNS})
}

// provisionCLI runs the provisioning commands; tests replace its AWS and
// Cloudflare clients. The commands read only the environment: they need
// no database and don't touch the data directory.
var provisionCLI = &provision.CLI{}

func runProvision(ctx context.Context, env *Env, args []string) error {
	return exitCode(provisionCLI.Provision(ctx, args, env.Stdout, env.Stderr, env.Getenv))
}

func runDNS(ctx context.Context, env *Env, args []string) error {
	return exitCode(provisionCLI.DNS(ctx, args, env.Stdout, env.Stderr, env.Getenv))
}

// exitCode turns the exit code of a command that reports its own errors
// (on stderr, or in its --json document) into a silent ExitCodeError.
func exitCode(code int) error {
	if code == ExitOK {
		return nil
	}
	return &ExitCodeError{Code: code}
}
