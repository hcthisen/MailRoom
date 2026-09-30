package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger(buf *bytes.Buffer) *slog.Logger {
	if buf == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:8080"
		cfg.HTTPOnly = true
	}
	if cfg.Logger == nil {
		cfg.Logger = quietLogger(nil)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRouting(t *testing.T) {
	s := newTestServer(t, Config{})
	s.PublicMux.HandleFunc("GET /u/{token}", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "unsub") })
	h := s.Handler()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/healthz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("/healthz = %d %s", rec.Code, rec.Body)
	}
	if rec := get("/api/v1/contacts"); rec.Code != 404 {
		t.Fatalf("/api before SetAPI = %d", rec.Code)
	}
	s.SetAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api:"+r.URL.Path) }))
	if rec := get("/api/v1/contacts"); rec.Body.String() != "api:/api/v1/contacts" {
		t.Fatalf("/api = %s", rec.Body)
	}
	if rec := get("/u/abc"); rec.Body.String() != "unsub" {
		t.Fatalf("/u = %s", rec.Body)
	}
	if rec := get("/nope"); rec.Code != 404 {
		t.Fatalf("/nope = %d", rec.Code)
	}
}

func TestHealthFailure(t *testing.T) {
	s := newTestServer(t, Config{Health: func(context.Context) error { return errors.New("db down") }})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	for _, tc := range []struct {
		base string
		http bool
		hsts bool
	}{
		{"http://localhost:8080", true, false},
		{"https://news.example.com", true, true}, // behind a TLS-terminating proxy
	} {
		s := newTestServer(t, Config{BaseURL: tc.base, HTTPOnly: tc.http})
		s.PublicMux.HandleFunc("/f/x", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "custom")
		})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("headers = %v", h)
		}
		if (h.Get("Strict-Transport-Security") != "") != tc.hsts {
			t.Fatalf("%s: HSTS = %q", tc.base, h.Get("Strict-Transport-Security"))
		}
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f/x", nil))
		if rec.Header().Get("Content-Security-Policy") != "custom" {
			t.Fatal("handlers must be able to override headers")
		}
	}
}

func TestRequestLogRedactsTokens(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServer(t, Config{Logger: quietLogger(&buf)})
	s.PublicMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	const secret = "eyJjIjo0MiwibSI6N30.c2lnbmF0dXJl"
	for _, p := range []string{"/u/" + secret, "/c/" + secret, "/o/" + secret + "/pixel.gif", "/f/abc/confirm/" + strings.Repeat("z", 40)} {
		s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, p+"?email=a@b.com", nil))
	}
	out := buf.String()
	if strings.Contains(out, secret) || strings.Contains(out, strings.Repeat("z", 40)) || strings.Contains(out, "a@b.com") {
		t.Fatalf("log leaks secrets: %s", out)
	}
	if !strings.Contains(out, `"status":204`) || !strings.Contains(out, `"/u/_"`) {
		t.Fatalf("log line missing fields: %s", out)
	}
}

func TestRedactPath(t *testing.T) {
	tests := map[string]string{
		"/u/tok":                        "/u/_",
		"/u/":                           "/u/",
		"/c/tok/extra":                  "/c/_/extra",
		"/f/ab12cd34ef":                 "/f/ab12cd34ef",
		"/api/v1/contacts/12":           "/api/v1/contacts/12",
		"/x/" + strings.Repeat("a", 32): "/x/_",
	}
	for in, want := range tests {
		if got := RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecoverer(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServer(t, Config{Logger: quietLogger(&buf)})
	s.PublicMux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Fatalf("panic response = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(buf.String(), "kaboom") {
		t.Fatal("panic not logged")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{BaseURL: "not a url"}); err == nil {
		t.Fatal("want error for bad base URL")
	}
	if _, err := New(Config{BaseURL: "http://x.example.com"}); err == nil {
		t.Fatal("TLS mode must require https")
	}
	if _, err := New(Config{BaseURL: "https://x.example.com"}); err == nil {
		t.Fatal("TLS mode must require an autocert dir")
	}
	s, err := New(Config{BaseURL: "https://x.example.com", AutocertDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := s.CertManager()
	if m == nil {
		t.Fatal("TLS mode needs a cert manager")
	}
	ctx := context.Background()
	if err := m.HostPolicy(ctx, "x.example.com"); err != nil {
		t.Fatalf("own host rejected: %v", err)
	}
	if err := m.HostPolicy(ctx, "evil.example.com"); err == nil {
		t.Fatal("other hosts must be rejected")
	}
}

func TestACMEHandler(t *testing.T) {
	s, err := New(Config{BaseURL: "https://x.example.com", AutocertDir: t.TempDir(), Logger: quietLogger(nil)})
	if err != nil {
		t.Fatal(err)
	}
	h := s.acmeHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x.example.com/f/abc", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://x.example.com/f/abc" {
		t.Fatalf("redirect = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("/healthz on :80 = %d", rec.Code)
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	s := newTestServer(t, Config{BaseURL: "http://localhost", HTTPOnly: true, HTTPAddr: "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	var addr string
	for i := 0; i < 200 && addr == ""; i++ {
		if a := s.Addrs(); len(a) > 0 {
			addr = a[0].String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("server did not start")
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunListenError(t *testing.T) {
	s := newTestServer(t, Config{BaseURL: "http://localhost", HTTPOnly: true, HTTPAddr: "256.0.0.1:1"})
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want listen error")
	}
}

// funcWorker adapts a function to Worker.
type funcWorker struct {
	name string
	run  func(ctx context.Context) error
}

func (w funcWorker) Name() string                  { return w.name }
func (w funcWorker) Run(ctx context.Context) error { return w.run(ctx) }

func TestSupervisorRestartsAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	var once atomic.Int32
	restarting := funcWorker{name: "flaky", run: func(ctx context.Context) error {
		switch calls.Add(1) {
		case 1:
			return errors.New("transient")
		case 2:
			panic("worker bug")
		default:
			<-ctx.Done()
			return ctx.Err()
		}
	}}
	finishing := funcWorker{name: "once", run: func(context.Context) error {
		once.Add(1)
		return nil
	}}
	sup := &Supervisor{Logger: quietLogger(nil), MinBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sup.Run(ctx, restarting, finishing) }()

	deadline := time.After(5 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("worker ran %d times, want 3", calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
	if once.Load() != 1 {
		t.Fatalf("finished worker ran %d times, want 1", once.Load())
	}
	if calls.Load() != 3 {
		t.Fatalf("flaky worker ran %d times after cancel", calls.Load())
	}
}

func TestSupervisorStopsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failing := funcWorker{name: "failing", run: func(context.Context) error { return errors.New("always") }}
	sup := &Supervisor{Logger: quietLogger(nil), MinBackoff: time.Hour}
	done := make(chan struct{})
	go func() { sup.Run(ctx, failing); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop during backoff")
	}
}

func TestSupervisorRunUntilBoundsShutdown(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	stubborn := funcWorker{name: "stubborn", run: func(context.Context) error { <-release; return nil }}
	polite := funcWorker{name: "polite", run: func(ctx context.Context) error { <-ctx.Done(); return nil }}
	tests := []struct {
		name    string
		workers []Worker
		want    []string
	}{
		{"all stop in time", []Worker{polite}, nil},
		{"a worker ignores its context", []Worker{polite, stubborn}, []string{"stubborn"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			sup := &Supervisor{Logger: quietLogger(nil)}
			got := make(chan []string, 1)
			go func() { got <- sup.RunUntil(ctx, 100*time.Millisecond, tt.workers...) }()
			time.Sleep(20 * time.Millisecond)
			cancel()
			select {
			case names := <-got:
				if fmt.Sprint(names) != fmt.Sprint(tt.want) {
					t.Fatalf("still running = %v, want %v", names, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("RunUntil did not give up")
			}
		})
	}
}
