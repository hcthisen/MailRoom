package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(status)
		case "/moved":
			http.Redirect(w, r, "/healthz", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	env, out, errOut := testEnv(nil)
	if code := run(context.Background(), []string{"healthcheck", "--url", srv.URL + "/healthz", "--json"}, env); code != ExitOK {
		t.Fatalf("healthy exit = %d: %s %s", code, out, errOut)
	}
	var res healthcheckResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || !res.Healthy || res.Status != http.StatusOK {
		t.Fatalf("result = %+v (%v)", res, err)
	}

	status = http.StatusServiceUnavailable
	env, out, _ = testEnv(nil)
	if code := run(context.Background(), []string{"healthcheck", "--url", srv.URL + "/healthz"}, env); code != ExitError {
		t.Fatalf("unhealthy exit = %d", code)
	}
	if !strings.Contains(out.String(), "unhealthy") || !strings.Contains(out.String(), "503") {
		t.Fatalf("output = %q", out.String())
	}

	// A redirect (e.g. to HTTPS) is not a health answer.
	env, _, _ = testEnv(nil)
	if code := run(context.Background(), []string{"healthcheck", "--url", srv.URL + "/moved"}, env); code != ExitError {
		t.Fatalf("redirect exit = %d", code)
	}
}

func TestHealthcheckDerivesURLFromEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	env, out, errOut := testEnv(map[string]string{"HTTP_ONLY": "1", "HTTP_ADDR": addr})
	if code := run(context.Background(), []string{"healthcheck"}, env); code != ExitOK {
		t.Fatalf("exit = %d: %s %s", code, out, errOut)
	}
	// --http-only works like serve's flag.
	env, _, _ = testEnv(map[string]string{"HTTP_ADDR": addr})
	if code := run(context.Background(), []string{"healthcheck", "--http-only"}, env); code != ExitOK {
		t.Fatalf("--http-only exit = %d", code)
	}
	env, _, errOut = testEnv(map[string]string{"HTTP_ONLY": "sometimes"})
	if code := run(context.Background(), []string{"healthcheck"}, env); code != ExitError || !strings.Contains(errOut.String(), "HTTP_ONLY") {
		t.Fatalf("bad env exit = %d: %s", code, errOut)
	}
}

func TestHealthcheckIsHidden(t *testing.T) {
	if _, ok := lookup("healthcheck"); !ok {
		t.Fatal("healthcheck not registered")
	}
	env, out, _ := testEnv(nil)
	if code := run(context.Background(), []string{"help"}, env); code != ExitOK {
		t.Fatalf("help exit = %d", code)
	}
	if strings.Contains(out.String(), "healthcheck") {
		t.Fatalf("usage lists the hidden command: %q", out.String())
	}
}
