// Package server runs Mailroom's HTTP server: HTTPS with automatic Let's
// Encrypt certificates (autocert) on :80/:443, or plain HTTP behind a
// reverse proxy (HTTP_ONLY), plus the background worker lifecycle and
// graceful shutdown.
//
// Routing: /healthz is built in, /api/ goes to the API handler (set with
// SetAPI) and everything else goes to PublicMux, where the owning packages
// register public endpoints (/f, /u, /c, /o, /hooks/ses) and the UI.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// Config configures a Server.
type Config struct {
	// BaseURL is the public URL of the install (config.Config.BaseURL).
	// In TLS mode its host is the only name autocert will request.
	BaseURL string
	// HTTPOnly serves plain HTTP on HTTPAddr (behind a reverse proxy).
	HTTPOnly bool
	// HTTPAddr is the listen address in HTTP-only mode (default ":8080").
	HTTPAddr string
	// TLSAddr and ACMEAddr are the HTTPS and HTTP-01/redirect listen
	// addresses in TLS mode (defaults ":443" and ":80").
	TLSAddr  string
	ACMEAddr string
	// AutocertDir caches ACME account keys and certificates (<data>/autocert).
	AutocertDir string
	// ACMEEmail is the optional Let's Encrypt contact address.
	ACMEEmail string
	// Health, when set, is called by /healthz; an error answers 503.
	Health func(ctx context.Context) error
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// ShutdownTimeout bounds graceful shutdown (default 20s).
	ShutdownTimeout time.Duration
}

// Server is Mailroom's HTTP server.
type Server struct {
	// PublicMux receives every request that isn't /healthz or /api/.
	PublicMux *http.ServeMux

	cfg     Config
	log     *slog.Logger
	api     atomic.Pointer[http.Handler]
	manager *autocert.Manager
	secure  bool // BaseURL is https: send HSTS

	mu    sync.Mutex
	addrs []net.Addr
}

// New validates cfg and returns a server. It doesn't listen yet; see Run.
func New(cfg Config) (*Server, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("server: invalid base URL %q", cfg.BaseURL)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if cfg.TLSAddr == "" {
		cfg.TLSAddr = ":443"
	}
	if cfg.ACMEAddr == "" {
		cfg.ACMEAddr = ":80"
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 20 * time.Second
	}
	s := &Server{
		PublicMux: http.NewServeMux(),
		cfg:       cfg,
		log:       cfg.Logger,
		secure:    u.Scheme == "https",
	}
	if !cfg.HTTPOnly {
		if u.Scheme != "https" {
			return nil, errors.New("server: TLS mode needs an https base URL (or set HTTP_ONLY=1)")
		}
		if cfg.AutocertDir == "" {
			return nil, errors.New("server: TLS mode needs an autocert cache directory")
		}
		s.manager = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(cfg.AutocertDir),
			HostPolicy: autocert.HostWhitelist(u.Hostname()),
			Email:      cfg.ACMEEmail,
		}
	}
	return s, nil
}

// SetAPI mounts h under /api/. It may be called before or after Run.
func (s *Server) SetAPI(h http.Handler) { s.api.Store(&h) }

// CertManager returns the autocert manager, or nil in HTTP-only mode.
func (s *Server) CertManager() *autocert.Manager { return s.manager }

// Addrs returns the addresses the server is listening on once Run has
// started (useful with ":0" in tests).
func (s *Server) Addrs() []net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]net.Addr(nil), s.addrs...)
}

// Handler returns the root handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if h := s.api.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.Handle("/", s.PublicMux)
	return recoverer(s.log, requestLogger(s.log, securityHeaders(s.secure, mux)))
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg.Health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.cfg.Health(ctx); err != nil {
			s.log.Warn("health check failed", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
	}
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// newHTTPServer applies Mailroom's timeouts to h.
func (s *Server) newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}

// Run listens and serves until ctx is cancelled, then shuts down
// gracefully. It returns an error if a listener can't be opened or fails.
func (s *Server) Run(ctx context.Context) error {
	type unit struct {
		srv *http.Server
		ln  net.Listener
		tls bool
	}
	var units []unit
	listen := func(addr string, srv *http.Server, tls bool) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("server: listen %s: %w", addr, err)
		}
		units = append(units, unit{srv: srv, ln: ln, tls: tls})
		return nil
	}
	closeAll := func() {
		for _, u := range units {
			_ = u.ln.Close()
		}
	}

	root := s.Handler()
	if s.cfg.HTTPOnly {
		if err := listen(s.cfg.HTTPAddr, s.newHTTPServer(root), false); err != nil {
			return err
		}
	} else {
		tlsSrv := s.newHTTPServer(root)
		tlsSrv.TLSConfig = s.manager.TLSConfig()
		if err := listen(s.cfg.TLSAddr, tlsSrv, true); err != nil {
			return err
		}
		if err := listen(s.cfg.ACMEAddr, s.newHTTPServer(s.acmeHandler()), false); err != nil {
			closeAll()
			return err
		}
	}

	s.mu.Lock()
	for _, u := range units {
		s.addrs = append(s.addrs, u.ln.Addr())
	}
	s.mu.Unlock()

	errc := make(chan error, len(units))
	for _, u := range units {
		s.log.Info("http server listening", "addr", u.ln.Addr().String(), "tls", u.tls)
		go func() {
			var err error
			if u.tls {
				err = u.srv.ServeTLS(u.ln, "", "")
			} else {
				err = u.srv.Serve(u.ln)
			}
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errc <- err
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		if runErr == nil {
			runErr = errors.New("server: listener stopped unexpectedly")
		}
	}

	s.log.Info("http server shutting down")
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	for _, u := range units {
		if err := u.srv.Shutdown(shutCtx); err != nil {
			_ = u.srv.Close()
			runErr = errors.Join(runErr, fmt.Errorf("server: shutdown: %w", err))
		}
	}
	return runErr
}

// acmeHandler serves HTTP-01 challenges and /healthz on port 80 and
// redirects everything else to https.
func (s *Server) acmeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("/", s.manager.HTTPHandler(nil))
	return recoverer(s.log, mux)
}
