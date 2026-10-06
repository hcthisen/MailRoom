package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Settings raw keys written by the sending package and read here.
const (
	SettingSESAccount   = "ses_account"
	SettingSendingState = "sending_state"
)

// Health is the answer of GET /system/health.
type Health struct {
	Status        string          `json:"status" enum:"ok,degraded,fail" doc:"fail: the database is unreachable; degraded: migrations are pending" example:"ok"`
	Database      string          `json:"database" doc:"\"ok\" or the database error" example:"ok"`
	SchemaVersion int             `json:"schema_version" doc:"Applied schema version" example:"1"`
	SchemaLatest  int             `json:"schema_latest" doc:"Newest schema version this binary knows" example:"1"`
	QueueDepth    int64           `json:"queue_depth" doc:"Messages waiting to be sent" example:"0"`
	QueueDue      int64           `json:"queue_due" doc:"Queued messages whose send time has come" example:"0"`
	JobsQueued    int64           `json:"jobs_queued" doc:"Background jobs waiting to run" example:"0"`
	JobsRunning   int64           `json:"jobs_running" doc:"Background jobs running" example:"0"`
	SendingState  json.RawMessage `json:"sending_state" doc:"The sender's state document (e.g. whether sending is paused), null when the sender hasn't written one"`
	Version       string          `json:"version" doc:"Build version" example:"1.0.0"`
	UptimeSeconds int64           `json:"uptime_seconds" doc:"Seconds since the process started" example:"3600"`
	CheckedAt     time.Time       `json:"checked_at"`
}

// Liveness pings the database; it is meant for server.Config.Health
// (/healthz).
func (s *Service) Liveness(ctx context.Context) error {
	if s.db == nil {
		return errNoDB
	}
	return s.db.Ping(ctx)
}

// Health reports database reachability, schema version, queue depth,
// jobs and the sending state. Database problems are reported in the
// result, not as an error.
func (s *Service) Health(ctx context.Context) Health {
	now := s.clock.Now()
	h := Health{Status: "ok", Database: "ok", Version: s.version, CheckedAt: now,
		UptimeSeconds: int64(now.Sub(s.started) / time.Second), SendingState: json.RawMessage("null")}
	h.SchemaLatest = latestSchema()
	if s.db == nil {
		h.Status, h.Database = "fail", errNoDB.Error()
		return h
	}
	if err := s.db.Ping(ctx); err != nil {
		h.Status, h.Database = "fail", err.Error()
		return h
	}
	q := s.db.Read
	var version sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if err == nil {
		err = q.QueryRowContext(ctx,
			`SELECT count(*), coalesce(sum(next_attempt_at <= ?), 0) FROM messages WHERE status = 'queued'`,
			db.FormatTime(now)).Scan(&h.QueueDepth, &h.QueueDue)
	}
	if err == nil {
		err = q.QueryRowContext(ctx,
			`SELECT coalesce(sum(status = 'queued'), 0), coalesce(sum(status = 'running'), 0) FROM jobs WHERE status IN ('queued', 'running')`).
			Scan(&h.JobsQueued, &h.JobsRunning)
	}
	if err != nil {
		h.Status, h.Database = "fail", err.Error()
		return h
	}
	h.SchemaVersion = int(version.Int64)
	if h.SchemaVersion < h.SchemaLatest {
		h.Status = "degraded"
	}
	if s.settings != nil {
		if raw, err := s.settings.GetRaw(ctx, nil, SettingSendingState); err == nil {
			h.SendingState = raw
		} else if !errors.Is(err, core.ErrNotFound) {
			s.log.Warn("system: read sending state", "err", err)
		}
	}
	return h
}

// latestSchema is the newest schema version this binary knows.
func latestSchema() int {
	all, err := db.Migrations()
	if err != nil || len(all) == 0 {
		return 0
	}
	return all[len(all)-1].Version
}

// VersionInfo is the answer of GET /system/version.
type VersionInfo struct {
	Version       string    `json:"version" doc:"Build version, compiled into the binary" example:"1.0.0"`
	GoVersion     string    `json:"go_version" example:"go1.27.0"`
	Platform      string    `json:"platform" example:"linux/amd64"`
	SchemaLatest  int       `json:"schema_latest" doc:"Newest schema version this binary knows" example:"1"`
	StartedAt     time.Time `json:"started_at" doc:"When the process started"`
	UptimeSeconds int64     `json:"uptime_seconds" example:"3600"`
}

// Version returns the build and process information.
func (s *Service) Version() VersionInfo {
	now := s.clock.Now()
	return VersionInfo{
		Version: s.version, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		SchemaLatest: latestSchema(), StartedAt: s.started, UptimeSeconds: int64(now.Sub(s.started) / time.Second),
	}
}

// SESAccount is the SES account status and quota. The sending package's
// account poller stores it (every 5 minutes) under the settings raw key
// "ses_account" in this JSON shape.
type SESAccount struct {
	ProductionAccessEnabled bool      `json:"production_access_enabled" doc:"false while the account is in the SES sandbox" example:"true"`
	SendingEnabled          bool      `json:"sending_enabled" doc:"false when AWS paused sending for the account" example:"true"`
	EnforcementStatus       string    `json:"enforcement_status" doc:"HEALTHY, PROBATION or SHUTDOWN" example:"HEALTHY"`
	ReviewStatus            string    `json:"review_status,omitempty" doc:"Status of the production access request: PENDING, FAILED, GRANTED or DENIED" example:"GRANTED"`
	Max24HourSend           float64   `json:"max_24_hour_send" doc:"Daily sending quota (-1 = unlimited)" example:"50000"`
	MaxSendRate             float64   `json:"max_send_rate" doc:"Maximum messages per second" example:"14"`
	SentLast24Hours         float64   `json:"sent_last_24_hours" doc:"Messages sent in the last 24 hours" example:"1200"`
	FetchedAt               time.Time `json:"fetched_at" doc:"When the status was read from SES"`
}

// SESStatus is the answer of GET /system/ses.
type SESStatus struct {
	Source    string     `json:"source" enum:"cache,live" doc:"cache: the sender's last poll; live: read from SES for this request" example:"cache"`
	Account   SESAccount `json:"account"`
	QuotaUsed float64    `json:"quota_used" doc:"Share of the daily quota used, 0 to 1 (0 when unlimited)" example:"0.024"`
}

// sesCacheMaxAge is how old the cached account status may be before GET
// /system/ses reads SES live instead (the poller runs every 5 minutes).
const sesCacheMaxAge = 15 * time.Minute

// SESStatus returns the SES account status: the sending poller's cached
// copy when it is fresh, else (or with refresh) a live GetAccount call.
// Without AWS configuration and cache it wraps core.ErrConflict.
func (s *Service) SESStatus(ctx context.Context, refresh bool) (SESStatus, error) {
	now := s.clock.Now()
	var cached *SESAccount
	if s.settings != nil && !refresh {
		if raw, err := s.settings.GetRaw(ctx, nil, SettingSESAccount); err == nil {
			var a SESAccount
			if json.Unmarshal(raw, &a) == nil && !a.FetchedAt.IsZero() {
				cached = &a
			}
		}
	}
	if cached != nil && (now.Sub(cached.FetchedAt) <= sesCacheMaxAge || s.ses == nil) {
		return newSESStatus("cache", *cached), nil
	}
	if s.ses == nil {
		return SESStatus{}, fmt.Errorf("SES is not configured (AWS_REGION and the sending user's keys are missing); run `mailroom provision ses`: %w", core.ErrConflict)
	}
	out, err := s.ses.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil {
		if cached != nil {
			s.log.Warn("system: SES GetAccount failed; serving the cached status", "err", err)
			return newSESStatus("cache", *cached), nil
		}
		return SESStatus{}, fmt.Errorf("system: SES GetAccount: %w", err)
	}
	return newSESStatus("live", AccountFromSES(out, now)), nil
}

func newSESStatus(source string, a SESAccount) SESStatus {
	st := SESStatus{Source: source, Account: a}
	if a.Max24HourSend > 0 {
		st.QuotaUsed = a.SentLast24Hours / a.Max24HourSend
	}
	return st
}

// AccountFromSES converts a GetAccount response, fetched at t.
func AccountFromSES(out *sesv2.GetAccountOutput, t time.Time) SESAccount {
	a := SESAccount{ProductionAccessEnabled: out.ProductionAccessEnabled, SendingEnabled: out.SendingEnabled, FetchedAt: t.UTC()}
	if out.EnforcementStatus != nil {
		a.EnforcementStatus = *out.EnforcementStatus
	}
	if q := out.SendQuota; q != nil {
		a.Max24HourSend, a.MaxSendRate, a.SentLast24Hours = q.Max24HourSend, q.MaxSendRate, q.SentLast24Hours
	}
	if out.Details != nil && out.Details.ReviewDetails != nil {
		a.ReviewStatus = string(out.Details.ReviewDetails.Status)
	}
	return a
}
