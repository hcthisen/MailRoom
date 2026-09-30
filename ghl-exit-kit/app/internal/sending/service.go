package sending

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mailroom/internal/config"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/server"
	"mailroom/internal/settings"
)

// Settings keys owned by this package (settings.PutRaw documents).
const (
	// StateKey holds the global pause state (State).
	StateKey = "sending_state"
	// AccountKey holds the last SES account snapshot (Account). The system
	// package reads it for the SES status API and doctor.
	AccountKey = "ses_account"
)

// Defaults.
const (
	DefaultWorkers      = 4
	DefaultPollInterval = time.Second
	// DefaultAccountPollInterval is how often the SES account is polled.
	DefaultAccountPollInterval = 5 * time.Minute
	// DefaultRate is the send rate (messages/s) until the SES quota is known.
	DefaultRate = 1.0
	// dispatchBatch is how many due messages one dispatcher round selects.
	dispatchBatch = 100
	// sendTimeout bounds one provider call.
	sendTimeout = 60 * time.Second
	// completeInterval is how often the sender completes finished
	// broadcasts while it is busy with other messages.
	completeInterval = 5 * time.Second
)

// Deps are the Service's dependencies. DB, Settings, Renderer and Provider
// are required.
type Deps struct {
	DB       *db.DB
	Clock    core.Clock
	Settings *settings.Service
	// Contacts reads contacts and the suppressions table. When nil a
	// store without hooks is created from DB and Clock (the sender never
	// writes contacts).
	Contacts *contactstore.Store
	// Config supplies the sending domain (Message-ID, Feedback-ID).
	Config   config.Config
	Renderer core.Renderer
	Provider core.EmailProvider
	// Account is the SES account API for the ses-account-poller. When nil
	// the poller is not started and the rate stays at the override or
	// DefaultRate.
	Account AccountAPI
	Logger  *slog.Logger
	// Workers is the number of concurrent senders (default DefaultWorkers).
	// They share one token bucket, so this bounds concurrency, not rate.
	Workers int
	// PollInterval is the idle poll interval of the sender (default 1 s).
	PollInterval time.Duration
	// AccountPollInterval is the SES poll interval (default 5 min).
	AccountPollInterval time.Duration
}

// Service is the sending service. Create it with New.
type Service struct {
	db       *db.DB
	clock    core.Clock
	settings *settings.Service
	contacts *contactstore.Store
	domain   string
	renderer core.Renderer
	provider core.EmailProvider
	account  AccountAPI
	log      *slog.Logger

	workers          int
	pollInterval     time.Duration
	acctInterval     time.Duration
	completeInterval time.Duration   // how often finished broadcasts are completed while busy
	outcomeBackoff   []time.Duration // retries of an outcome write, see persist

	bucket *bucket

	acctMu  sync.RWMutex
	acct    *Account // last known SES account snapshot; nil until known
	running sync.Mutex
}

// New returns a sending service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Workers <= 0 {
		d.Workers = DefaultWorkers
	}
	if d.PollInterval <= 0 {
		d.PollInterval = DefaultPollInterval
	}
	if d.AccountPollInterval <= 0 {
		d.AccountPollInterval = DefaultAccountPollInterval
	}
	if d.Contacts == nil {
		d.Contacts = &contactstore.Store{DB: d.DB, Clock: d.Clock}
	}
	domain := d.Config.Domain
	if domain == "" && d.Settings != nil {
		domain = d.Settings.Domain()
	}
	return &Service{
		db:           d.DB,
		clock:        d.Clock,
		settings:     d.Settings,
		contacts:     d.Contacts,
		domain:       domain,
		renderer:     d.Renderer,
		provider:     d.Provider,
		account:      d.Account,
		log:          d.Logger,
		workers:      d.Workers,
		pollInterval: d.PollInterval,
		acctInterval: d.AccountPollInterval,
		bucket:       newBucket(d.Clock, DefaultRate),

		completeInterval: completeInterval,
		outcomeBackoff:   defaultOutcomeBackoff,
	}
}

// Workers returns the background workers: the sender and, when an SES
// account API is configured, the ses-account-poller.
func (s *Service) Workers() []server.Worker {
	ws := []server.Worker{&senderWorker{s: s}}
	if s.account != nil {
		ws = append(ws, &accountPoller{s: s})
	}
	return ws
}

// wait blocks for d or until ctx is done or wake fires; it reports false
// when ctx is done.
func wait(ctx context.Context, d time.Duration, wake <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	case <-wake:
	}
	return true
}
