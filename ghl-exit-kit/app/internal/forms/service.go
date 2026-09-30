// Package forms is Mailroom's sign-up forms part (PRD §6.3 F-1..F-8, K-4):
// forms CRUD with an embed snippet and the list of recent submissions
// (REST API, tag "Forms"), the public endpoints under /f/ (hosted form
// page, form-encoded or JSON submissions with CORS, honeypot and per-IP
// rate limit), double opt-in and forced resubscribe confirmation, and the
// hourly worker that expires unconfirmed sign-ups (F-6).
//
// Public endpoints are plain http.Handlers mounted on server.PublicMux with
// Mount. A submission never reveals whether an address is suppressed,
// bounced or complained (F-7): every accepted submission of a form gets
// the same response.
package forms

import (
	"context"
	"database/sql"
	"log/slog"
	"net/netip"
	"strconv"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/config"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
	"mailroom/internal/tokens"
)

// Public submission rate limit per client address (F-5): 10 per minute.
const (
	SubmitRate  = 10.0 / 60
	SubmitBurst = 10
)

// Deps are the Service's dependencies.
type Deps struct {
	DB    *db.DB
	Store *contactstore.Store
	// Enroller starts a form's sequence (implemented by sequences). May be
	// nil (tests): sign-ups then enrol nobody.
	Enroller core.Enroller
	// Settings supplies the sender name shown on public pages. May be nil.
	Settings *settings.Service
	// Alerter alerts the owner when a confirmation email cap is reached.
	// May be nil (the cap is then only logged).
	Alerter core.Alerter
	// Signer signs opt-in and resubscribe confirmation tokens. Required.
	Signer *tokens.Signer
	// Config supplies BaseURL (public URLs) and TrustedProxies (client
	// addresses behind a reverse proxy).
	Config config.Config
	// Limiter rate-limits submissions per client address; nil = a limiter
	// with SubmitRate and SubmitBurst.
	Limiter *api.Limiter
	Clock   core.Clock   // default core.SystemClock
	Logger  *slog.Logger // default slog.Default()
}

// Service implements forms. API handlers, public handlers and the admin UI
// call its methods.
type Service struct {
	db       *db.DB
	store    *contactstore.Store
	enroller core.Enroller
	settings *settings.Service
	alerter  core.Alerter
	signer   *tokens.Signer
	baseURL  string
	trusted  []netip.Prefix
	limiter  *api.Limiter
	// pageLimiter bounds the other public requests (pages, preflights,
	// confirmations) per client address.
	pageLimiter *api.Limiter
	// logLimiter bounds how many spam/rate_limited submissions are recorded
	// per client address, so a flood can't fill the table.
	logLimiter *api.Limiter
	clock      core.Clock
	log        *slog.Logger
}

// New returns a forms service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Limiter == nil {
		d.Limiter = api.NewLimiter(SubmitRate, SubmitBurst, d.Clock)
	}
	return &Service{
		db:          d.DB,
		store:       d.Store,
		enroller:    d.Enroller,
		settings:    d.Settings,
		alerter:     d.Alerter,
		signer:      d.Signer,
		baseURL:     d.Config.BaseURL,
		trusted:     d.Config.TrustedProxies,
		limiter:     d.Limiter,
		pageLimiter: api.NewLimiter(1, 60, d.Clock),
		logLimiter:  api.NewLimiter(0.1, 5, d.Clock),
		clock:       d.Clock,
		log:         d.Logger,
	}
}

// audit writes an audit record for a form in tx.
func (s *Service) audit(ctx context.Context, tx *sql.Tx, op string, id int64, changes any) error {
	return audit.Write(ctx, tx, audit.Entry{
		Operation: op, TargetType: "form", TargetID: strconv.FormatInt(id, 10), Changes: changes, At: s.clock.Now(),
	})
}

// HostedURL returns the public URL of a form's hosted page.
func (s *Service) HostedURL(publicID string) string { return s.baseURL + "/f/" + publicID }

// confirmURL returns the public URL of a confirmation token.
func (s *Service) confirmURL(token string) string { return s.baseURL + "/f/confirm/" + token }

// senderName returns the sender's display name for public pages, or "".
func (s *Service) senderName(ctx context.Context) string {
	if s.settings == nil {
		return ""
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "forms: read settings", "err", err)
		return ""
	}
	return st.Sender.FromName
}
