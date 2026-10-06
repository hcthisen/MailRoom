package main

import (
	"context"
	"fmt"

	"mailroom/internal/app"
	"mailroom/internal/config"
	"mailroom/internal/settings"
	"mailroom/internal/system"
)

func init() {
	register(Command{Name: "doctor", Summary: "check DNS, SES, SNS, HTTPS, backups, clock and disk (exit 2 when a check fails)", Run: runDoctor})
}

// integrations are the network clients of the doctor and backup commands.
type integrations struct {
	AWS      app.AWSClients
	Resolver system.Resolver
	Certs    system.CertChecker
	HTTP     system.HTTPDoer
}

// newIntegrations builds the real clients; tests replace it with fakes.
var newIntegrations = func(ctx context.Context, cfg config.Config) (integrations, error) {
	aws, err := app.NewAWSClients(ctx, cfg)
	if err != nil {
		return integrations{}, err
	}
	return integrations{
		AWS: aws, Resolver: system.NewDefaultResolver(),
		Certs: system.NewTLSChecker(nil), HTTP: system.NewDefaultHTTPClient(),
	}, nil
}

// runDoctor runs the same checks as GET /api/v1/system/doctor (L-4). It
// also works before the first start: a new database is created and the
// settings seeded from the environment, as the server would. It never
// migrates an existing database (see openBootstrap).
func runDoctor(ctx context.Context, env *Env, args []string) error {
	fs, jsonOut := newFlagSet(env, "doctor")
	if err := fs.Parse(args); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	cfg, d, err := openBootstrap(ctx, env)
	if err != nil {
		return err
	}
	defer d.Close()
	st := settings.New(settings.Deps{DB: d, Domain: cfg.Domain})
	if _, err := st.SeedFromConfig(cliCtx(ctx), cfg); err != nil {
		return err
	}
	in, err := newIntegrations(ctx, cfg)
	if err != nil {
		return err
	}
	sys := system.New(system.Deps{
		DB: d, Config: cfg, Settings: st,
		SES: in.AWS.SES, SNS: in.AWS.SNSClient(), S3: in.AWS.S3,
		Resolver: in.Resolver, CertChecker: in.Certs, HTTP: in.HTTP,
		Version: version, Logger: newStderrLogger(env),
	})
	rep := sys.Doctor(ctx)
	if *jsonOut {
		b, err := rep.JSON()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(env.Stdout, string(b)); err != nil {
			return err
		}
	} else if _, err := fmt.Fprint(env.Stdout, rep.Text()); err != nil {
		return err
	}
	return exitCode(rep.ExitCode())
}
