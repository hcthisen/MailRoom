package broadcasts

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
)

// transitionFn performs a status change inside the transaction and returns
// the changes to audit.
type transitionFn func(tx *sql.Tx, cur Broadcast) (map[string]any, error)

// transition runs one B-2 state change: it loads the broadcast, checks
// If-Match and that the current status is one of from, calls fn and
// writes the audit record "broadcast.<action>". With dryRun it is all
// rolled back.
func (s *Service) transition(ctx context.Context, id, ifVersion int64, action string, from []string, dryRun bool, fn transitionFn) (Broadcast, error) {
	var out Broadcast
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		cur, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		if !slices.Contains(from, cur.Status) {
			return fmt.Errorf("broadcast %d is %s; %s needs a %s broadcast: %w",
				id, cur.Status, action, strings.Join(from, " or "), core.ErrConflict)
		}
		changes, err := fn(tx, cur)
		if err != nil {
			return err
		}
		if changes == nil {
			changes = map[string]any{}
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		changes["status"] = map[string]any{"from": cur.Status, "to": out.Status}
		return s.logAudit(ctx, tx, "broadcast."+action, id, changes)
	})
	return out, err
}

// Schedule moves a draft to scheduled; the starter starts it at at, which
// must be in the future.
func (s *Service) Schedule(ctx context.Context, id int64, at time.Time, ifVersion int64) (Broadcast, error) {
	if at.IsZero() {
		return Broadcast{}, core.Invalid("at", "is required")
	}
	if !at.After(s.clock.Now()) {
		return Broadcast{}, core.Invalid("at", "must be in the future")
	}
	if err := s.requireSender(ctx); err != nil {
		return Broadcast{}, err
	}
	return s.transition(ctx, id, ifVersion, "schedule", []string{StatusDraft}, false, func(tx *sql.Tx, cur Broadcast) (map[string]any, error) {
		if _, err := s.resolveAudience(ctx, cur.Audience); err != nil {
			return nil, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE broadcasts SET status = 'scheduled', scheduled_at = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			db.FormatTime(at), s.now(), id)
		return map[string]any{"scheduled_at": at.UTC()}, err
	})
}

// Unschedule moves a scheduled broadcast back to draft.
func (s *Service) Unschedule(ctx context.Context, id int64, ifVersion int64) (Broadcast, error) {
	return s.transition(ctx, id, ifVersion, "unschedule", []string{StatusScheduled}, false, func(tx *sql.Tx, cur Broadcast) (map[string]any, error) {
		_, err := tx.ExecContext(ctx, `UPDATE broadcasts SET status = 'draft', scheduled_at = NULL, version = version + 1, updated_at = ? WHERE id = ?`,
			s.now(), id)
		return map[string]any{"scheduled_at": cur.ScheduledAt}, err
	})
}

// BroadcastSendResult is the outcome of Send.
type BroadcastSendResult struct {
	DryRun      bool      `json:"dry_run" doc:"true when nothing was changed" example:"false"`
	Recipients  int64     `json:"recipients" doc:"Active, unsuppressed contacts in the audience right now: the messages the snapshot will queue" example:"1180"`
	MissingTags []string  `json:"missing_tags" doc:"Tag names the audience segment refers to that no tag has (renamed, merged or deleted); check the segment when this is not empty" example:"[]"`
	Broadcast   Broadcast `json:"broadcast" doc:"The broadcast. For a dry run it is unchanged, as stored (status, version and snapshot_job_id as before): the transition is checked but not kept"`
	Job         *jobs.Job `json:"job,omitempty" doc:"The broadcast.snapshot job queueing the recipients (omitted for a dry run)"`
}

// Send starts a draft or scheduled broadcast now: status sending and the
// snapshot job, in one transaction. With dryRun nothing changes and the
// result reports how many recipients the audience has.
func (s *Service) Send(ctx context.Context, id int64, ifVersion int64, dryRun bool) (BroadcastSendResult, error) {
	cur, err := s.get(ctx, nil, id)
	if err != nil {
		return BroadcastSendResult{}, err
	}
	if _, err := s.resolveAudience(ctx, cur.Audience); err != nil {
		return BroadcastSendResult{}, err
	}
	if err := s.requireSender(ctx); err != nil {
		return BroadcastSendResult{}, err
	}
	res := BroadcastSendResult{DryRun: dryRun}
	if res.Recipients, err = s.countRecipients(ctx, cur.Audience); err != nil {
		return BroadcastSendResult{}, err
	}
	if res.MissingTags, err = s.missingTags(ctx, cur.Audience); err != nil {
		return BroadcastSendResult{}, err
	}
	res.Broadcast, err = s.transition(ctx, id, ifVersion, "send", []string{StatusDraft, StatusScheduled}, dryRun,
		func(tx *sql.Tx, cur Broadcast) (map[string]any, error) {
			job, err := s.start(ctx, tx, cur.ID)
			if err != nil {
				return nil, err
			}
			if !dryRun {
				res.Job = &job
			}
			return map[string]any{"snapshot_job_id": job.ID, "recipients_estimate": res.Recipients}, nil
		})
	if err != nil {
		return BroadcastSendResult{}, err
	}
	if dryRun {
		// The rolled-back transition would show a sending broadcast whose
		// snapshot_job_id names a job that doesn't exist (and whose ID a
		// later job may get): report the broadcast as it is.
		res.Broadcast = cur
	}
	return res, nil
}

// Pause pauses a sending broadcast (reason manual). Queued messages stay
// queued and are sent after Resume.
func (s *Service) Pause(ctx context.Context, id int64, ifVersion int64) (Broadcast, error) {
	return s.transition(ctx, id, ifVersion, "pause", []string{StatusSending}, false, func(tx *sql.Tx, _ Broadcast) (map[string]any, error) {
		_, err := tx.ExecContext(ctx, `UPDATE broadcasts SET status = 'paused', paused_reason = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			PauseManual, s.now(), id)
		return map[string]any{"paused_reason": PauseManual}, err
	})
}

// Resume continues a paused broadcast. A broadcast paused by the circuit
// breaker needs confirm: the breaker then judges only the messages sent
// after this resume. A snapshot that didn't finish is submitted again,
// with the recipient bound of the original start and a fresh attempt
// count.
func (s *Service) Resume(ctx context.Context, id int64, confirm bool, ifVersion int64) (Broadcast, error) {
	return s.transition(ctx, id, ifVersion, "resume", []string{StatusPaused}, false, func(tx *sql.Tx, cur Broadcast) (map[string]any, error) {
		changes := map[string]any{"paused_reason": cur.PausedReason}
		if cur.PausedReason == PauseCircuitBreaker {
			if !confirm {
				return nil, core.Invalid("confirm", `this broadcast was paused by the circuit breaker (bounce or complaint rate too high); check its stats and resend with {"confirm": true} to continue`)
			}
			since := s.clock.Now()
			if err := s.putBaseline(ctx, tx, id, since); err != nil {
				return nil, err
			}
			changes["confirmed"] = true
			changes["breaker_since"] = since.UTC()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE broadcasts SET status = 'sending', paused_reason = '', version = version + 1, updated_at = ? WHERE id = ?`,
			s.now(), id); err != nil {
			return nil, err
		}
		if cur.SnapshotDoneAt == nil {
			prev, err := s.snapshotJob(ctx, cur.SnapshotJobID)
			if err != nil {
				return nil, err
			}
			if snapshotStalled(prev) {
				p, err := s.nextSnapshotParams(ctx, tx, id, prev)
				if err != nil {
					return nil, err
				}
				p.Attempt = 1 // the owner asked for another try
				job, err := s.submitSnapshot(ctx, tx, p)
				if err != nil {
					return nil, err
				}
				changes["snapshot_job_id"] = job.ID
			}
		}
		return changes, nil
	})
}

// Cancel cancels a draft, scheduled or paused broadcast. Its queued
// messages are skipped with error "cancelled".
func (s *Service) Cancel(ctx context.Context, id int64, ifVersion int64) (Broadcast, error) {
	return s.transition(ctx, id, ifVersion, "cancel", []string{StatusDraft, StatusScheduled, StatusPaused}, false, func(tx *sql.Tx, _ Broadcast) (map[string]any, error) {
		now := s.now()
		res, err := tx.ExecContext(ctx, `UPDATE messages SET status = 'skipped', error = ?, updated_at = ? WHERE broadcast_id = ? AND status = 'queued'`,
			SkipCancelled, now, id)
		if err != nil {
			return nil, fmt.Errorf("broadcasts: skip queued messages of %d: %w", id, err)
		}
		skipped, _ := res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `UPDATE broadcasts SET status = 'cancelled', paused_reason = '', finished_at = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			now, now, id); err != nil {
			return nil, err
		}
		return map[string]any{"skipped_messages": skipped}, nil
	})
}

// BroadcastTestResult lists the queued test messages.
type BroadcastTestResult struct {
	MessageIDs []int64  `json:"message_ids" doc:"The queued test messages, one per address" example:"[901]"`
	To         []string `json:"to" doc:"The normalised addresses, in the same order" example:"[\"owner@example.org\"]"`
}

// SendTest queues a test of the broadcast's email to 1-5 addresses (B-7)
// through the content service; contactID > 0 fills the merge fields with
// that contact's data. Test messages don't count in the broadcast's stats.
// Tests use the broadcast's from_name and reply_to overrides where it has
// them (content.SendTestAs), else the settings' sender.
func (s *Service) SendTest(ctx context.Context, id int64, to []string, contactID int64) (BroadcastTestResult, error) {
	if len(to) == 0 {
		return BroadcastTestResult{}, core.Invalid("to", "at least one address is required")
	}
	var out BroadcastTestResult
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		b, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		out.MessageIDs, out.To, err = s.content.SendTestAs(ctx, tx, b.Email.ID, to, contactID,
			content.SenderOverride{FromName: b.FromName, ReplyTo: b.ReplyTo})
		if err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "broadcast.test_send", id, map[string]any{"recipients": len(out.To), "message_ids": out.MessageIDs})
	})
	return out, err
}
