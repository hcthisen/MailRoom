package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"mailroom/internal/config"
)

func init() {
	register(Command{
		Name:    "healthcheck",
		Summary: "probe the local server's /healthz (container health check; exit 0 healthy, 1 unhealthy)",
		Hidden:  true,
		Run:     runHealthcheck,
	})
}

// DefaultHealthcheckTimeout bounds one health probe; Docker's own timeout
// should be longer.
const DefaultHealthcheckTimeout = 5 * time.Second

// healthcheckResult is the --json output of "healthcheck".
type healthcheckResult struct {
	Healthy bool   `json:"healthy"`
	URL     string `json:"url"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

// runHealthcheck GETs /healthz of the server in this container or host
// and exits 0 when it answers 200. The image has no shell or curl, so the
// compose health check runs this. It needs only HTTP_ONLY and HTTP_ADDR
// from the environment (config.HealthURL), or --url.
func runHealthcheck(ctx context.Context, env *Env, args []string) error {
	fs, jsonOut := newFlagSet(env, "healthcheck")
	url := fs.String("url", "", "probe this URL instead of the one derived from HTTP_ONLY/HTTP_ADDR")
	httpOnly := fs.Bool("http-only", false, "the server runs with --http-only (probe HTTP_ADDR)")
	timeout := fs.Duration("timeout", DefaultHealthcheckTimeout, "give up after this long")
	if err := fs.Parse(args); err != nil {
		return &ExitCodeError{Code: ExitError}
	}
	target := *url
	if target == "" {
		u, err := config.HealthURL(env.Getenv, *httpOnly)
		if err != nil {
			return &ExitCodeError{Code: ExitError, Err: err}
		}
		target = u
	}
	res := probeHealth(ctx, target, *timeout)
	text := "healthy: " + target
	if !res.Healthy {
		text = "unhealthy: " + target + ": " + res.Error
	}
	if err := printResult(env, *jsonOut, res, text); err != nil {
		return err
	}
	if !res.Healthy {
		return &ExitCodeError{Code: ExitError}
	}
	return nil
}

// probeHealth GETs target without following redirects: /healthz answers
// 200 itself, so anything else (a redirect to HTTPS included) is unhealthy.
func probeHealth(ctx context.Context, target string, timeout time.Duration) healthcheckResult {
	res := healthcheckResult{URL: target}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			res.Error = fmt.Sprintf("no answer within %s", timeout)
		}
		return res
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	res.Status = resp.StatusCode
	res.Healthy = resp.StatusCode == http.StatusOK
	if !res.Healthy {
		res.Error = "HTTP " + resp.Status
	}
	return res
}
