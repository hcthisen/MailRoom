package sending

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Pause sources.
const (
	// PauseManual is a pause set through the API or UI. It is lifted only
	// by a resume through the API or UI.
	PauseManual = "manual"
	// PauseSES is a pause set because SES reported sending paused, the
	// account suspended or sending disabled. It is lifted automatically when
	// the ses-account-poller sees sending enabled again. A manual pause
	// while an SES pause is in place replaces it.
	PauseSES = "ses"
)

// Alert keys raised by this package.
const (
	AlertSESPaused      = "ses_sending_paused"
	AlertSESResumed     = "ses_sending_resumed"
	AlertSESCredentials = "ses_credentials_rejected"
)

// State is the global pause state, stored under settings key StateKey.
// While Paused, no message of any kind is sent.
type State struct {
	Paused bool       `json:"paused" doc:"True while all sending is paused" example:"false"`
	Reason string     `json:"reason" doc:"Why sending is paused" example:""`
	Source string     `json:"source" enum:"manual,ses," doc:"Who paused it: manual (API/UI; lifted only by a resume) or ses (lifted automatically when SES sending is enabled again)" example:""`
	Since  *time.Time `json:"since" doc:"When sending was paused; null when not paused"`
}

// GetState returns the global pause state. A nil q reads from the read pool.
func (s *Service) GetState(ctx context.Context, q db.Querier) (State, error) {
	raw, err := s.settings.GetRaw(ctx, q, StateKey)
	if errors.Is(err, core.ErrNotFound) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("sending: decode %s: %w", StateKey, err)
	}
	return st, nil
}

// setPaused pauses sending in tx. It returns false (and changes nothing)
// when sending is already paused, so callers alert only on the transition.
func (s *Service) setPaused(ctx context.Context, tx *sql.Tx, source, reason string) (bool, error) {
	cur, err := s.GetState(ctx, tx)
	if err != nil || cur.Paused {
		return false, err
	}
	now := s.clock.Now()
	st := State{Paused: true, Reason: reason, Source: source, Since: &now}
	if err := s.settings.PutRaw(ctx, tx, StateKey, st); err != nil {
		return false, err
	}
	return true, nil
}

// setResumed lifts a pause in tx. With onlySource set, only a pause from
// that source is lifted. It reports whether a pause was lifted.
func (s *Service) setResumed(ctx context.Context, tx *sql.Tx, onlySource string) (bool, error) {
	cur, err := s.GetState(ctx, tx)
	if err != nil || !cur.Paused || (onlySource != "" && cur.Source != onlySource) {
		return false, err
	}
	if err := s.settings.PutRaw(ctx, tx, StateKey, State{}); err != nil {
		return false, err
	}
	return true, nil
}

// pauseForSES pauses all sending in tx because SES refused to send, and
// alerts the owner once (on the transition to paused).
func (s *Service) pauseForSES(ctx context.Context, tx *sql.Tx, reason string) error {
	changed, err := s.setPaused(ctx, tx, PauseSES, reason)
	if err != nil || !changed {
		return err
	}
	s.log.Warn("sending: all sending paused", "source", PauseSES, "reason", reason)
	return s.Alert(ctx, tx, AlertSESPaused, "Sending is paused: Amazon SES refused to send",
		"Mailroom has paused all sending because Amazon SES reported: "+reason+
			"\n\nQueued messages are kept and will be sent when sending resumes. Mailroom resumes automatically "+
			"when SES reports sending enabled again (checked every 5 minutes). Check the SES console for "+
			"account status, enforcement and any notice from AWS.")
}

// pauseForCredentials pauses all sending in tx because SES rejected the
// sending user's access key, and alerts the owner once. The pause has
// source PauseSES, so the ses-account-poller lifts it as soon as a poll
// succeeds with sending enabled, i.e. once the process runs with working
// keys. The alert is queued like any other and goes out after that.
func (s *Service) pauseForCredentials(ctx context.Context, tx *sql.Tx, reason string) error {
	changed, err := s.setPaused(ctx, tx, PauseSES, "SES rejected the access key: "+reason)
	if err != nil || !changed {
		return err
	}
	s.log.Warn("sending: all sending paused: SES rejected the access key", "source", PauseSES, "reason", reason)
	return s.Alert(ctx, tx, AlertSESCredentials, "Sending was paused: Amazon SES rejected Mailroom's access key",
		"Mailroom paused all sending because Amazon SES did not accept the sending user's access key ("+reason+").\n\n"+
			"This happens when the key in the server's .env was replaced (mailroom provision ses --rotate-keys) "+
			"but the container still ran with the old one. Recreate the container so it reads the env file again: "+
			"docker compose up -d (a plain restart keeps the old environment). Queued messages were kept and "+
			"are sent once sending resumes, which happens automatically when SES accepts the key again "+
			"(checked every 5 minutes).")
}

// Pause pauses all sending (manual pause) with an audit record. Pausing
// while a manual pause is in place changes nothing and returns the current
// state. Pausing while an SES pause is in place turns it into a manual
// pause (keeping Since), so the ses-account-poller no longer lifts it when
// SES enables sending again. With dryRun nothing is stored; the returned
// state is what would result.
func (s *Service) Pause(ctx context.Context, reason string, dryRun bool) (State, error) {
	if reason == "" {
		reason = "paused by " + core.ActorFromContext(ctx).String()
	}
	var out State
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		prev, err := s.GetState(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case prev.Paused && prev.Source == PauseManual:
			out = prev
			return nil
		case prev.Paused: // an SES pause: take it over
			out = State{Paused: true, Reason: reason, Source: PauseManual, Since: prev.Since}
			if err := s.settings.PutRaw(ctx, tx, StateKey, out); err != nil {
				return err
			}
		default:
			if _, err := s.setPaused(ctx, tx, PauseManual, reason); err != nil {
				return err
			}
			if out, err = s.GetState(ctx, tx); err != nil {
				return err
			}
		}
		return audit.Write(ctx, tx, audit.Entry{
			Operation: "sending.pause", TargetType: "sending", TargetID: StateKey,
			Changes: map[string]any{"reason": reason, "was_source": prev.Source}, At: s.clock.Now(),
		})
	})
	return out, err
}

// Resume lifts any pause (manual or SES) with an audit record. If SES
// still reports sending disabled, the next account poll pauses again.
// With dryRun nothing is stored; the returned state is what would result.
func (s *Service) Resume(ctx context.Context, dryRun bool) (State, error) {
	var (
		out     State
		changed bool
	)
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		prev, err := s.GetState(ctx, tx)
		if err != nil {
			return err
		}
		if changed, err = s.setResumed(ctx, tx, ""); err != nil || !changed {
			out = prev
			return err
		}
		out = State{}
		return audit.Write(ctx, tx, audit.Entry{
			Operation: "sending.resume", TargetType: "sending", TargetID: StateKey,
			Changes: map[string]any{"was_source": prev.Source, "was_reason": prev.Reason}, At: s.clock.Now(),
		})
	})
	if err == nil && changed && !dryRun {
		s.log.Info("sending: resumed")
	}
	return out, err
}
