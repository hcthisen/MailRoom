package main

import (
	"context"
	"log/slog"

	"mailroom/internal/app"
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
	if err := a.Run(ctx); err != nil {
		return err
	}
	log.Info("mailroom stopped")
	return nil
}
