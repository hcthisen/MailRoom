package main

import (
	"context"
	"log/slog"

	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/config"
)

func init() {
	register(Command{Name: "serve", Summary: "run the Mailroom server (HTTPS via autocert, or --http-only / HTTP_ONLY=1)", Run: runServe})
}

// newApp builds the application; tests replace it to inject fakes.
var newApp = app.New

func runServe(ctx context.Context, env *Env, args []string) error {
	fs, _ := newFlagSet(env, "serve")
	httpOnly := fs.Bool("http-only", false, "serve plain HTTP on HTTP_ADDR behind a reverse proxy (same as HTTP_ONLY=1)")
	if err := fs.Parse(args); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	getenv := env.Getenv
	if *httpOnly {
		getenv = func(k string) string {
			if k == "HTTP_ONLY" {
				return "1"
			}
			return env.Getenv(k)
		}
	}
	cfg, err := config.LoadFrom(getenv)
	if err != nil {
		return err
	}
	log := newLogger(env, cfg.LogLevel)
	slog.SetDefault(log)
	log.Info("mailroom starting", "version", version, "config", cfg)

	a, err := newApp(ctx, cfg, app.Options{Version: version, Logger: log})
	if err != nil {
		return err
	}
	defer a.Close()
	if err := bootstrapFromEnv(ctx, a, env.Getenv, log); err != nil {
		return err
	}
	if err := a.Run(ctx); err != nil {
		return err
	}
	log.Info("mailroom stopped")
	return nil
}

// bootstrapFromEnv creates the first admin and an admin API token from
// MAILROOM_ADMIN_EMAIL, MAILROOM_ADMIN_PASSWORD and
// MAILROOM_BOOTSTRAP_TOKEN, for platforms where no one can run "admin
// create" or "token create" in the container (Coolify). Both are created
// only once; the values are never logged.
func bootstrapFromEnv(ctx context.Context, a *app.App, getenv func(string) string, log *slog.Logger) error {
	in := auth.EnvBootstrap{
		AdminEmail:    getenv("MAILROOM_ADMIN_EMAIL"),
		AdminPassword: getenv("MAILROOM_ADMIN_PASSWORD"),
		Token:         getenv("MAILROOM_BOOTSTRAP_TOKEN"),
	}
	if in == (auth.EnvBootstrap{}) {
		return nil
	}
	res, err := a.Auth.BootstrapFromEnv(ctx, in)
	if err != nil {
		return err
	}
	if res.AdminCreated {
		log.Info("admin user created from MAILROOM_ADMIN_EMAIL")
	}
	if res.TokenCreated {
		log.Info("API token \"bootstrap\" created from MAILROOM_BOOTSTRAP_TOKEN")
	}
	return nil
}
