package events

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sns"

	"mailroom/internal/api"
	"mailroom/internal/config"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
	"mailroom/internal/tokens"
)

// SNSAPI is the subset of the SNS client the subscriber worker uses.
// *sns.Client satisfies it.
type SNSAPI interface {
	Subscribe(ctx context.Context, in *sns.SubscribeInput, optFns ...func(*sns.Options)) (*sns.SubscribeOutput, error)
	ListSubscriptionsByTopic(ctx context.Context, in *sns.ListSubscriptionsByTopicInput, optFns ...func(*sns.Options)) (*sns.ListSubscriptionsByTopicOutput, error)
}

// HTTPDoer performs outgoing HTTP requests: fetching SNS signing
// certificates and confirming subscriptions. *http.Client satisfies it.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Defaults for optional Deps.
const (
	// DefaultHTTPTimeout bounds each certificate fetch and subscription
	// confirmation.
	DefaultHTTPTimeout = 10 * time.Second
	// DefaultTrackingRate and DefaultTrackingBurst are the per-IP limits
	// for recording clicks and opens. Redirects and pixels are always
	// served; only the recording is skipped beyond the limit.
	DefaultTrackingRate  = 5
	DefaultTrackingBurst = 60
	// DefaultPageRate and DefaultPageBurst limit the unsubscribe page
	// (GET /u/{token}) per client IP. The POST that unsubscribes is never
	// limited: mail providers send one-click unsubscribes for many people
	// from a few shared addresses, and none may be refused (U-3).
	DefaultPageRate  = 1
	DefaultPageBurst = 60
	// DefaultSubscribeMinBackoff and DefaultSubscribeMaxBackoff bound the
	// sns-subscriber retry delay, which doubles after each failure.
	DefaultSubscribeMinBackoff = 5 * time.Second
	DefaultSubscribeMaxBackoff = 5 * time.Minute
)

// Deps are the Service's dependencies.
type Deps struct {
	DB *db.DB // required
	// Store applies contact status changes (and publishes StatusChanged,
	// which exits sequences).
	Store *contactstore.Store
	// Settings supplies the company name for the unsubscribe page and the
	// tracking switches. Nil means settings.Defaults().
	Settings *settings.Service
	// Signer verifies the unsubscribe, click and open tokens. Required.
	Signer *tokens.Signer
	// Config supplies BaseURL, SNSTopicARN, AWSRegion and TrustedProxies.
	Config config.Config
	// SNS is used by the sns-subscriber worker. Nil disables the worker.
	SNS SNSAPI
	// HTTP fetches signing certificates and SubscribeURLs. Nil means an
	// http.Client with DefaultHTTPTimeout that follows no redirects.
	HTTP HTTPDoer
	// Alerter raises the alert for an UnsubscribeConfirmation. Optional.
	Alerter core.Alerter
	Clock   core.Clock
	Logger  *slog.Logger
	// Limiter rate-limits click and open recording per client IP. Nil
	// means api.NewLimiter(DefaultTrackingRate, DefaultTrackingBurst).
	Limiter *api.Limiter
	// PageLimiter rate-limits the unsubscribe page per client IP. Nil
	// means api.NewLimiter(DefaultPageRate, DefaultPageBurst).
	PageLimiter *api.Limiter
	// TrustedProxies overrides Config.TrustedProxies when non-nil.
	TrustedProxies []netip.Prefix
	// SubscribeMinBackoff and SubscribeMaxBackoff tune the sns-subscriber
	// retries (defaults DefaultSubscribeMinBackoff/MaxBackoff).
	SubscribeMinBackoff time.Duration
	SubscribeMaxBackoff time.Duration
}

// Service implements the public event endpoints and the SNS subscriber.
type Service struct {
	db       *db.DB
	store    *contactstore.Store
	settings *settings.Service
	signer   *tokens.Signer
	cfg      config.Config
	sns      SNSAPI
	http     HTTPDoer
	alerter  core.Alerter
	clock    core.Clock
	log      *slog.Logger
	limiter  *api.Limiter
	pages    *api.Limiter
	trusted  []netip.Prefix
	certs    *certCache
	deferred *deferrals
	minWait  time.Duration
	maxWait  time.Duration
}

// New returns an events service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{
			Timeout: DefaultHTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if d.Limiter == nil {
		d.Limiter = api.NewLimiter(DefaultTrackingRate, DefaultTrackingBurst, d.Clock)
	}
	if d.PageLimiter == nil {
		d.PageLimiter = api.NewLimiter(DefaultPageRate, DefaultPageBurst, d.Clock)
	}
	if d.TrustedProxies == nil {
		d.TrustedProxies = d.Config.TrustedProxies
	}
	if d.SubscribeMinBackoff <= 0 {
		d.SubscribeMinBackoff = DefaultSubscribeMinBackoff
	}
	if d.SubscribeMaxBackoff < d.SubscribeMinBackoff {
		d.SubscribeMaxBackoff = max(DefaultSubscribeMaxBackoff, d.SubscribeMinBackoff)
	}
	store := d.Store
	if store == nil {
		store = &contactstore.Store{DB: d.DB, Clock: d.Clock}
	}
	return &Service{
		db:       d.DB,
		store:    store,
		settings: d.Settings,
		signer:   d.Signer,
		cfg:      d.Config,
		sns:      d.SNS,
		http:     d.HTTP,
		alerter:  d.Alerter,
		clock:    d.Clock,
		log:      d.Logger,
		limiter:  d.Limiter,
		pages:    d.PageLimiter,
		trusted:  d.TrustedProxies,
		certs:    newCertCache(maxCachedCerts),
		deferred: newDeferrals(),
		minWait:  d.SubscribeMinBackoff,
		maxWait:  d.SubscribeMaxBackoff,
	}
}

// Public paths served by the Service.
const (
	PathSESHook     = "/hooks/ses"
	PathUnsubscribe = "/u/"
	PathClick       = "/c/"
	PathOpen        = "/o/"
)

// Mount registers the public endpoints on mux (server.PublicMux):
// POST /hooks/ses, GET and POST /u/{token}, GET /c/{token} and
// GET /o/{token}. GET patterns also answer HEAD.
//
// Rate limits: the unsubscribe page (GET) is limited per client IP
// (PageLimiter); click and open recording too (Limiter, the redirect and
// pixel are always served). The unsubscribe POST and the SNS webhook are
// not: one-click unsubscribes arrive from mail providers' shared addresses
// and must never be refused, and SNS posts every SES event from a few AWS
// addresses and would drop bounces and complaints on a 429. Both check an
// HMAC token or the SNS signature before touching the database.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathSESHook, s.handleSNS)
	mux.Handle("GET "+PathUnsubscribe+"{token}", s.pages.Middleware(s.clientKey, http.HandlerFunc(s.handleUnsubscribePage)))
	mux.HandleFunc("POST "+PathUnsubscribe+"{token}", s.handleUnsubscribe)
	mux.HandleFunc("GET "+PathClick+"{token}", s.handleClick)
	mux.HandleFunc("GET "+PathOpen+"{token}", s.handleOpen)
}

// Handler returns a mux with only the Service's endpoints, for tests and
// for callers that mount it under a prefix-free router.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Mount(mux)
	return mux
}

// appSettings returns the current settings, or the defaults when no
// settings service is wired or it fails (public pages must still work).
func (s *Service) appSettings(ctx context.Context) settings.Settings {
	if s.settings == nil {
		return settings.Defaults()
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		s.log.Warn("events: read settings", "err", err)
		return settings.Defaults()
	}
	return st
}

// clientKey is the per-IP rate-limit key of r.
func (s *Service) clientKey(r *http.Request) string {
	return api.IPKey(api.ClientIP(r, s.trusted))
}

// now returns the current time formatted for the database.
func (s *Service) now() string { return db.FormatTime(s.clock.Now()) }
