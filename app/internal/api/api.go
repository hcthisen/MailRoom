// Package api is Mailroom's REST API foundation (ARCHITECTURE.md §7): the
// Huma v2 setup with the generated OpenAPI 3.1 spec, the single error
// format, authentication (API tokens and UI sessions with CSRF) and scopes,
// per-actor rate limiting, Idempotency-Key handling, ETag/If-Match,
// cursor pagination, dry runs and the route registry used by the UI/API
// parity test.
//
// Every domain package registers its operations with Register from its
// RegisterAPI(a *api.API) method. The auth package supplies the
// Authenticator, so this package does not depend on it.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/server"
)

// Prefix is the base path of every API operation.
const Prefix = "/api/v1"

// Config configures the API.
type Config struct {
	// BaseURL is the public URL of the install (config.Config.BaseURL); the
	// OpenAPI server URL is BaseURL + Prefix.
	BaseURL string
	// Version is the build version shown in the spec.
	Version string
	// Auth validates tokens and sessions. When nil every protected request
	// is refused with 401.
	Auth Authenticator
	// DB stores idempotency keys. Required.
	DB *db.DB
	// Clock defaults to core.SystemClock.
	Clock core.Clock
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Rate and Burst configure the per-actor limiter (defaults DefaultRate
	// and DefaultBurst).
	Rate  float64
	Burst int
	// TrustedProxies are the reverse proxies whose X-Forwarded-For is
	// believed (config TRUSTED_PROXIES); see ClientIP. Empty = none.
	TrustedProxies []netip.Prefix
	// Guide is the agent guide (Markdown) served at Prefix+"/docs" (G-3).
	// When empty, a short pointer to the OpenAPI spec is served.
	Guide []byte
}

// API is the Mailroom REST API. Create it with New, register operations
// with Register and mount Handler() under /api/ (server.SetAPI).
type API struct {
	cfg     Config
	mux     *http.ServeMux
	huma    huma.API
	limiter *Limiter
	idem    *idempotency

	mu     sync.Mutex
	routes []RouteInfo
	opIDs  map[string]struct{}
}

// New creates the API. The OpenAPI spec is served (without credentials)
// at Prefix+"/openapi.json" and ".yaml"; Huma's CDN docs page is disabled.
func New(cfg Config) (*API, error) {
	if cfg.DB == nil {
		return nil, errors.New("api: DB is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = core.SystemClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	a := &API{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		limiter: NewLimiter(cfg.Rate, cfg.Burst, cfg.Clock),
		idem:    newIdempotency(cfg.DB, cfg.Clock, cfg.Logger),
		opIDs:   map[string]struct{}{},
	}
	a.huma = humago.NewWithPrefix(a.mux, Prefix, humaConfig(cfg))
	a.huma.UseMiddleware(a.requireScope)
	a.mux.HandleFunc("GET "+Prefix+"/docs", a.serveGuide)
	a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		NewError(http.StatusNotFound, CodeNotFound, "no such API endpoint: "+r.Method+" "+server.RedactPath(r.URL.Path), "").Write(w)
	})
	return a, nil
}

func humaConfig(cfg Config) huma.Config {
	c := huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info: &huma.Info{
				Title:   "Mailroom API",
				Version: cfg.Version,
				Description: "The complete REST API of Mailroom: every record, setting and action is here (PRD G-1).\n\n" +
					"Authenticate with `Authorization: Bearer mr_...`. Errors always look like " +
					"`{\"error\": {\"code\": \"...\", \"message\": \"...\", \"field\": \"...\"}}`. " +
					"Send an `Idempotency-Key` header on every POST, `If-Match` with the ETag on updates, " +
					"and use `?dry_run=true` where offered. A guide with recipes is at `" + Prefix + "/docs`.",
				License: &huma.License{Name: "MIT"},
			},
			Servers: []*huma.Server{{URL: strings.TrimSuffix(cfg.BaseURL, "/") + Prefix}},
			Components: &huma.Components{
				Schemas: huma.NewMapRegistry("#/components/schemas/", newSchemaNamer()),
				SecuritySchemes: map[string]*huma.SecurityScheme{
					securityScheme: {
						Type:         "http",
						Scheme:       "bearer",
						BearerFormat: "mr_...",
						Description:  "An API token created with `mailroom token create` or POST /tokens. Scope admin may call everything, scope read only read operations.",
					},
				},
			},
		},
		OpenAPIPath:   "/openapi",
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	}
	return c
}

// Huma returns the underlying Huma API, e.g. for spec generation. Register
// operations with Register, not huma.Register, so they are scoped and
// recorded.
func (a *API) Huma() huma.API { return a.huma }

// OpenAPI returns the generated spec.
func (a *API) OpenAPI() *huma.OpenAPI { return a.huma.OpenAPI() }

// Limiter returns the per-actor rate limiter.
func (a *API) Limiter() *Limiter { return a.limiter }

// Handler returns the API handler with authentication, rate limiting and
// idempotency applied. It expects full paths (Prefix included).
func (a *API) Handler() http.Handler {
	return a.authenticate(a.idem.wrap(a.mux))
}

// serveGuide serves the agent guide (G-3).
func (a *API) serveGuide(w http.ResponseWriter, _ *http.Request) {
	guide := a.cfg.Guide
	if len(guide) == 0 {
		guide = []byte("# Mailroom API\n\nThe OpenAPI 3.1 spec is at " + Prefix + "/openapi.json (and .yaml).\n" +
			"Authenticate with `Authorization: Bearer mr_...`.\n")
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write(guide)
}

// CleanupIdempotency deletes Idempotency-Key records older than
// IdempotencyTTL and returns how many were removed.
func (a *API) CleanupIdempotency(ctx context.Context) (int64, error) {
	return a.idem.cleanup(ctx)
}

// IdempotencyCleaner returns the worker that runs CleanupIdempotency at
// start and then every interval (default one hour).
func (a *API) IdempotencyCleaner(interval time.Duration) server.Worker {
	if interval <= 0 {
		interval = time.Hour
	}
	return &cleaner{a: a, interval: interval}
}

type cleaner struct {
	a        *API
	interval time.Duration
}

func (c *cleaner) Name() string { return "idempotency-cleanup" }

func (c *cleaner) Run(ctx context.Context) error {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		n, err := c.a.CleanupIdempotency(ctx)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if n > 0 {
			c.a.cfg.Logger.Debug("api: expired idempotency keys deleted", "count", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
