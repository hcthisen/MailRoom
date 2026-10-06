// Package app wires Mailroom together (ARCHITECTURE.md §2, [W3]): it opens
// and migrates the database, builds every service with its dependencies
// and the cross-package interfaces (content as core.Renderer, segments as
// core.SegmentResolver and core.TagReferrer, sequences as core.Enroller,
// sending as core.Alerter), registers every API operation on one api.API,
// every job handler before the jobs runner starts and every hook
// subscriber on one hooks.Bus, mounts the public endpoints and the admin
// UI (/admin/, with / redirecting there) on the server's public mux, and
// collects the background workers. cmd/mailroom serve runs
// the result; tests use Handler with httptest.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"mailroom/docs"
	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/auth"
	"mailroom/internal/broadcasts"
	"mailroom/internal/config"
	"mailroom/internal/contacts"
	"mailroom/internal/contactstore"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/events"
	"mailroom/internal/forms"
	"mailroom/internal/hooks"
	"mailroom/internal/imports"
	"mailroom/internal/jobs"
	"mailroom/internal/segments"
	"mailroom/internal/sending"
	"mailroom/internal/sequences"
	"mailroom/internal/server"
	"mailroom/internal/settings"
	"mailroom/internal/stats"
	"mailroom/internal/system"
	"mailroom/internal/tokens"
	"mailroom/internal/ui"
)

// Public endpoint limits that no owning package sets itself.
const (
	// ArchiveRate and ArchiveBurst limit the public web archive (B-10) per
	// client address.
	ArchiveRate  = 2.0
	ArchiveBurst = 30
	// MediaRate and MediaBurst limit the public media files (logos and
	// images in sent mail) per client address: generous, because a mail
	// client loads every image of an email at once.
	MediaRate  = 20.0
	MediaBurst = 300
	// WorkerDrainTimeout bounds how long shutdown waits for the background
	// workers after the HTTP server has stopped (ARCHITECTURE.md §8).
	WorkerDrainTimeout = 30 * time.Second
)

// Options tunes New. The zero value builds the production app from the
// configuration alone.
type Options struct {
	// Version is the build version (default "dev").
	Version string
	// Clock defaults to core.SystemClock (tests: testutil.Clock).
	Clock core.Clock
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// StartedAt is the process start (default: Clock.Now() in New).
	StartedAt time.Time

	// Provider replaces the SES email provider (tests:
	// testutil.FakeProvider). Without it, SES is used when the sending
	// user's credentials are configured; otherwise the sender worker is not
	// started and messages stay queued.
	Provider core.EmailProvider
	// AWS replaces the AWS clients built from the configuration.
	AWS *AWSClients
	// Resolver, CertChecker and DoctorHTTP replace the doctor's DNS
	// resolver, HTTPS certificate check and clock-skew HTTP client.
	Resolver    system.Resolver
	CertChecker system.CertChecker
	DoctorHTTP  system.HTTPDoer
	// WebhookHTTP replaces the client the SNS webhook uses to fetch
	// signing certificates and confirm subscriptions.
	WebhookHTTP events.HTTPDoer
	// Offline leaves every integration not set explicitly above unset
	// instead of building the real one, so nothing reaches the network:
	// no AWS clients, DNS, certificate or clock-skew checks (they report
	// skipped or failed) and no SES provider. For tests.
	Offline bool

	// SchedulerInterval overrides the sequences scheduler cadence (tests).
	SchedulerInterval time.Duration
}

// App is a wired Mailroom instance.
type App struct {
	Config config.Config
	DB     *db.DB
	Clock  core.Clock
	Log    *slog.Logger

	API    *api.API
	Server *server.Server
	Bus    *hooks.Bus
	Store  *contactstore.Store
	Signer *tokens.Signer

	Settings   *settings.Service
	Auth       *auth.Service
	Audit      *audit.Service
	Jobs       *jobs.Runner
	Contacts   *contacts.Service
	Segments   *segments.Service
	Content    *content.Service
	Sending    *sending.Service
	System     *system.Service
	Events     *events.Service
	Sequences  *sequences.Service
	Broadcasts *broadcasts.Service
	Forms      *forms.Service
	Imports    *imports.Service
	Stats      *stats.Service

	// UI is the admin UI, mounted under /admin/.
	UI *ui.UI
	// Version is the build version (Options.Version).
	Version string

	// Migrations is what opening the database applied.
	Migrations MigrateResult
	// SESConfigured reports whether a real (or injected) email provider
	// sends; false means the sender worker is not running.
	SESConfigured bool

	workers []server.Worker
}

// New opens and migrates the database, seeds the settings on first start
// and builds every service, route, job handler, hook and worker. Close
// releases the database.
func New(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = opts.Clock.Now()
	}
	opts = production(opts)
	d, mig, err := OpenDB(ctx, cfg.DBPath(), cfg.BackupsDir(), opts.Clock, opts.Logger)
	if err != nil {
		return nil, err
	}
	a := &App{Config: cfg, DB: d, Clock: opts.Clock, Log: opts.Logger, Migrations: mig, Version: opts.Version}
	if err := a.build(ctx, opts); err != nil {
		d.Close()
		return nil, err
	}
	return a, nil
}

// build creates the services on a.DB and wires them.
func (a *App) build(ctx context.Context, opts Options) error {
	cfg, d, clock, log := a.Config, a.DB, a.Clock, a.Log

	a.Settings = settings.New(settings.Deps{DB: d, Clock: clock, Domain: cfg.Domain})
	seeded, err := a.Settings.SeedFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	if seeded {
		log.Info("settings seeded from the environment")
	}
	if a.Signer, err = tokens.NewSigner(cfg.Secret, clock); err != nil {
		return fmt.Errorf("app: token signer: %w", err)
	}
	a.Bus = hooks.New()
	a.Store = &contactstore.Store{DB: d, Bus: a.Bus, Clock: clock}

	aws, err := a.awsClients(ctx, opts)
	if err != nil {
		return err
	}
	provider, account := a.emailProvider(opts, aws)

	a.Auth = auth.New(auth.Deps{DB: d, Clock: clock, Logger: log, Issuer: "Mailroom " + cfg.Domain})
	a.Audit = audit.New(d)
	a.Jobs = jobs.New(jobs.Deps{DB: d, Clock: clock, Logger: log})
	if a.API, err = api.New(api.Config{
		BaseURL: cfg.BaseURL, Version: opts.Version, Auth: a.Auth, DB: d, Clock: clock, Logger: log,
		TrustedProxies: cfg.TrustedProxies, Guide: docs.APIGuide(),
	}); err != nil {
		return err
	}

	a.Segments = segments.New(segments.Deps{DB: d, Clock: clock})
	a.Contacts = contacts.New(contacts.Deps{
		DB: d, Store: a.Store, Jobs: a.Jobs, Segments: a.Segments, TagRefs: a.Segments, Clock: clock, Logger: log,
	})
	a.Content = content.New(content.Deps{
		DB: d, Settings: a.Settings, Signer: a.Signer, Contacts: a.Store,
		BaseURL: cfg.BaseURL, DataDir: cfg.DataDir, Clock: clock, Logger: log,
	})
	a.Sending = sending.New(sending.Deps{
		DB: d, Clock: clock, Settings: a.Settings, Contacts: a.Store, Config: cfg,
		Renderer: a.Content, Provider: provider, Account: account, Logger: log,
	})
	a.System = system.New(system.Deps{
		DB: d, Clock: clock, Config: cfg, Settings: a.Settings, Jobs: a.Jobs, Alerter: a.Sending,
		SES: aws.SES, SNS: aws.SNSClient(), S3: aws.S3,
		Resolver: opts.Resolver, CertChecker: opts.CertChecker, HTTP: opts.DoctorHTTP,
		Version: opts.Version, StartedAt: opts.StartedAt, Logger: log,
	})
	a.Events = events.New(events.Deps{
		DB: d, Store: a.Store, Settings: a.Settings, Signer: a.Signer, Config: cfg,
		SNS: aws.SNS, HTTP: opts.WebhookHTTP, Alerter: a.Sending, Clock: clock, Logger: log,
	})
	a.Sequences = sequences.New(sequences.Deps{
		DB: d, Store: a.Store, Content: a.Content, Bus: a.Bus, Jobs: a.Jobs, Settings: a.Settings,
		Clock: clock, Logger: log, SchedulerInterval: opts.SchedulerInterval,
	})
	a.Broadcasts = broadcasts.New(broadcasts.Deps{
		DB: d, Content: a.Content, Segments: a.Segments, Jobs: a.Jobs, Alerter: a.Sending,
		Settings: a.Settings, BaseURL: cfg.BaseURL, Clock: clock, Logger: log,
	})
	a.Forms = forms.New(forms.Deps{
		DB: d, Store: a.Store, Enroller: a.Sequences, Settings: a.Settings, Signer: a.Signer, Alerter: a.Sending,
		Config: cfg, Clock: clock, Logger: log,
	})
	a.Imports = imports.New(imports.Deps{DB: d, Store: a.Store, Jobs: a.Jobs, Clock: clock, Logger: log, DataDir: cfg.DataDir})
	a.Stats = stats.New(stats.Deps{DB: d, Clock: clock, Settings: a.Settings})

	// Hook subscribers, all on one bus: the store publishes to it.
	a.Sequences.RegisterHooks(a.Bus)

	// Job handlers, before the runner starts (Run).
	a.Contacts.RegisterJobs(a.Jobs)
	a.Imports.RegisterJobs(a.Jobs)
	a.System.RegisterJobs()
	a.Sequences.RegisterJobs()
	a.Broadcasts.RegisterJobs()

	// API operations; api.Register panics on a duplicate operation ID and
	// the mux on a duplicate route, so each is registered exactly once.
	for _, r := range []interface{ RegisterAPI(*api.API) }{
		a.Settings, a.Auth, a.Audit, a.Jobs, a.Contacts, a.Segments, a.Content, a.Sending,
		a.System, a.Sequences, a.Broadcasts, a.Forms, a.Imports, a.Stats,
	} {
		r.RegisterAPI(a.API)
	}

	if a.Server, err = server.New(server.Config{
		BaseURL: cfg.BaseURL, HTTPOnly: cfg.HTTPOnly, HTTPAddr: cfg.HTTPAddr,
		AutocertDir: cfg.AutocertDir(), ACMEEmail: cfg.OwnerEmail,
		Health: a.System.Liveness, Logger: log,
	}); err != nil {
		return err
	}
	a.Server.SetAPI(a.API.Handler())
	a.mountPublic(a.Server.PublicMux)
	a.UI = ui.New(ui.Deps{
		Config: cfg, Version: opts.Version, Clock: clock, Logger: log, DB: d,
		Auth: a.Auth, Settings: a.Settings, Audit: a.Audit, Jobs: a.Jobs, Contacts: a.Contacts,
		Segments: a.Segments, Content: a.Content, Sending: a.Sending, System: a.System,
		Sequences: a.Sequences, Broadcasts: a.Broadcasts, Forms: a.Forms, Imports: a.Imports, Stats: a.Stats,
	})
	a.Server.PublicMux.Handle(ui.Prefix+"/", a.UI.Handler())
	a.Server.PublicMux.Handle("GET /{$}", ui.RootRedirect())

	a.workers = []server.Worker{a.Jobs, a.API.IdempotencyCleaner(0)}
	for _, w := range a.Sending.Workers() {
		if w.Name() == "sender" && !a.SESConfigured {
			continue
		}
		a.workers = append(a.workers, w)
	}
	a.workers = append(a.workers, a.System.Workers()...)
	a.workers = append(a.workers,
		a.Sequences.SchedulerWorker(),
		a.Broadcasts.StarterWorker(),
		a.Forms.ExpiryWorker(),
		a.Events.SubscriberWorker(),
		a.UI.DoctorWorker(),
	)
	return nil
}

// awsClients returns the injected clients, or builds them from the
// configuration unless opts.Offline.
func (a *App) awsClients(ctx context.Context, opts Options) (AWSClients, error) {
	switch {
	case opts.AWS != nil:
		return *opts.AWS, nil
	case opts.Offline:
		return AWSClients{}, nil
	}
	return NewAWSClients(ctx, a.Config)
}

// emailProvider picks the email provider and the SES account API of the
// ses-account-poller, and sets a.SESConfigured.
func (a *App) emailProvider(opts Options, aws AWSClients) (core.EmailProvider, sending.AccountAPI) {
	if opts.Provider != nil {
		a.SESConfigured = true
		return opts.Provider, aws.Account
	}
	if !opts.Offline && SESConfigured(a.Config) {
		p, err := sending.NewSESProvider(a.Config)
		if err == nil {
			a.SESConfigured = true
			account := aws.Account
			if account == nil {
				account = p.AccountAPI()
			}
			return p, account
		}
		a.Log.Error("app: SES provider", "err", err)
	}
	a.Log.Warn("SES is not configured: nothing is sent and messages stay queued until the sending user's keys are set " +
		"(run `mailroom provision ses`, then recreate the container with `docker compose up -d`; a plain restart keeps the old environment)")
	return unconfiguredProvider{}, nil
}

// mountPublic registers every public endpoint (outside /api) on mux: forms
// (/f), unsubscribe, click and open tracking and the SNS webhook (/u, /c,
// /o, /hooks/ses), media (/media) and the web archive (/archive).
func (a *App) mountPublic(mux *http.ServeMux) {
	a.Forms.Mount(mux)
	a.Events.Mount(mux)
	trusted := a.Config.TrustedProxies
	clientKey := func(r *http.Request) string { return api.IPKey(api.ClientIP(r, trusted)) }
	media := api.NewLimiter(MediaRate, MediaBurst, a.Clock)
	mux.Handle("GET /media/{name}", media.Middleware(clientKey, a.Content.MediaHandler()))
	lim := api.NewLimiter(ArchiveRate, ArchiveBurst, a.Clock)
	mux.Handle(broadcasts.ArchivePattern, lim.Middleware(clientKey, a.Broadcasts.ArchiveHandler()))
}

// production fills in the real network integrations that opts leaves
// unset (unless Offline): the doctor's resolver, certificate check and
// clock-skew client.
func production(opts Options) Options {
	if opts.Offline {
		return opts
	}
	if opts.Resolver == nil {
		opts.Resolver = system.NewDefaultResolver()
	}
	if opts.CertChecker == nil {
		opts.CertChecker = system.NewTLSChecker(opts.Clock)
	}
	if opts.DoctorHTTP == nil {
		opts.DoctorHTTP = system.NewDefaultHTTPClient()
	}
	return opts
}

// Workers returns the background workers Run supervises.
func (a *App) Workers() []server.Worker { return append([]server.Worker(nil), a.workers...) }

// Handler returns the root HTTP handler (/healthz, /api/, the admin UI
// under /admin/, public endpoints) with the server middleware, for tests
// with httptest.
func (a *App) Handler() http.Handler { return a.Server.Handler() }

// Run serves HTTP and runs the workers until ctx is cancelled, then shuts
// down in order: the HTTP server first (up to its shutdown timeout), then
// the workers get their context cancelled and WorkerDrainTimeout to
// return. Workers still running after that are logged and abandoned (jobs
// cut short are recovered at the next start).
func (a *App) Run(ctx context.Context) error {
	workerCtx, stopWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWorkers()
	sup := &server.Supervisor{Logger: a.Log, Clock: a.Clock}
	stuck := make(chan []string, 1)
	go func() { stuck <- sup.RunUntil(workerCtx, WorkerDrainTimeout, a.workers...) }()

	err := a.Server.Run(ctx)
	stopWorkers()
	if names := <-stuck; len(names) > 0 {
		a.Log.Warn("workers still running after the shutdown timeout; exiting anyway", "workers", names)
	}
	return err
}

// Close closes the database.
func (a *App) Close() error {
	if a.DB == nil {
		return nil
	}
	err := a.DB.Close()
	a.DB = nil
	return err
}
