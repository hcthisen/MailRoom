// Package inbound receives email (PRD §6.16, N-1..N-6). SES stores mail sent
// to INBOUND_DOMAIN in the inbound S3 bucket and notifies the event topic;
// the SNS webhook (events) hands the notification to Received, which
// records a pending message. The inbound-fetcher worker fetches the stored
// message, parses it (bodies, attachments, threading headers), links it to
// the contact with the sender's address and deletes the S3 copy. The API
// and the admin UI's Inbox list, read, archive and delete messages and
// send replies (message kind reply, queued through the outbox).
package inbound

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"mailroom/internal/audit"
	"mailroom/internal/config"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
)

// ObjectStore is the part of the S3 client the fetcher uses. *s3.Client
// satisfies it.
type ObjectStore interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Deps are the Service's dependencies.
type Deps struct {
	DB *db.DB // required
	// Store looks up the contact with the sender's address.
	Store *contactstore.Store
	// Settings supplies the sender of replies.
	Settings *settings.Service
	// Config supplies INBOUND_BUCKET and INBOUND_DOMAIN.
	Config config.Config
	// S3 fetches received mail. Nil disables the fetcher worker: messages
	// stay pending.
	S3     ObjectStore
	Clock  core.Clock
	Logger *slog.Logger
	// PollInterval overrides the fetcher's idle poll (DefaultPollInterval).
	PollInterval time.Duration
}

// Service implements receiving.
type Service struct {
	db       *db.DB
	contacts *contactstore.Store
	settings *settings.Service
	cfg      config.Config
	s3       ObjectStore
	clock    core.Clock
	log      *slog.Logger
	poll     time.Duration
}

// New returns the Service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.PollInterval <= 0 {
		d.PollInterval = DefaultPollInterval
	}
	return &Service{db: d.DB, contacts: d.Store, settings: d.Settings, cfg: d.Config, s3: d.S3,
		clock: d.Clock, log: d.Logger, poll: d.PollInterval}
}

// Enabled reports whether receiving is configured (INBOUND_BUCKET and
// INBOUND_DOMAIN set by provision ses).
func (s *Service) Enabled() bool { return s.cfg.Receiving() }

var _ core.InboundReceiver = (*Service)(nil)

func (s *Service) audit(ctx context.Context, tx *sql.Tx, op string, id int64, changes any) error {
	return audit.Write(ctx, tx, audit.Entry{Operation: op, TargetType: "inbound", TargetID: strconv.FormatInt(id, 10),
		Changes: changes, At: s.clock.Now()})
}
