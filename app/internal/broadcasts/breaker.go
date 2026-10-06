package broadcasts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
)

// baselinePrefix is the settings-table key prefix of a broadcast's
// circuit-breaker baseline, written through settings.PutRaw when a
// tripped broadcast is resumed with confirm. It is never removed: only a
// broadcast that sent messages can trip, and Delete keeps those, so the
// document lives exactly as long as its broadcast. (A broadcasts column
// would be the better home; see the contract requests.)
const baselinePrefix = "broadcast_breaker:"

// BreakerCounts are the numbers the circuit breaker judges (B-6).
type BreakerCounts struct {
	Sent        int64 `json:"sent" doc:"Messages handed to the provider"`
	HardBounces int64 `json:"hard_bounces" doc:"Messages with a permanent bounce event"`
	Complaints  int64 `json:"complaints" doc:"Messages with a complaint (not-spam feedback reports excluded)"`
}

// breakerBaseline is the document stored under baselinePrefix+id: the
// breaker judges only messages sent at or after Since. Counting from a boundary
// (rather than subtracting totals) keeps a late bounce or complaint about
// a message sent before the resume from counting against the messages
// sent after it.
type breakerBaseline struct {
	Since time.Time `json:"since"`
}

// complaintExpr is true for a message with a real complaint: its status is
// complained, or it has a complaint event other than a "not-spam" feedback
// report (which events records but does not treat as a complaint).
const complaintExpr = `(m.status = 'complained' OR EXISTS (SELECT 1 FROM events e WHERE e.message_id = m.id
	AND e.type = 'complaint' AND e.subtype <> '` + complaintNotSpam + `'))`

// hardBounceExpr is true for a message with a permanent bounce event.
const hardBounceExpr = `EXISTS (SELECT 1 FROM events e WHERE e.message_id = m.id AND e.type = 'bounce' AND e.subtype = 'permanent')`

// complaintNotSpam is the events.subtype of an SES "not-spam" feedback
// report (events.ComplaintNotSpam).
const complaintNotSpam = "not-spam"

// breakerCounts reads a broadcast's sent, hard-bounced and complained
// message counts over the messages sent at or after since ("" = all of
// them). The boundary is inclusive: a paused broadcast sends nothing, so a
// message stamped with the resume instant itself was sent after it.
func (s *Service) breakerCounts(ctx context.Context, q db.Querier, id int64, since string) (BreakerCounts, error) {
	var c BreakerCounts
	err := q.QueryRowContext(ctx, `
		SELECT count(*), IFNULL(sum(`+hardBounceExpr+`), 0), IFNULL(sum(`+complaintExpr+`), 0)
		FROM messages m WHERE m.broadcast_id = ? AND m.sent_at IS NOT NULL AND m.sent_at >= ?`, id, since).
		Scan(&c.Sent, &c.HardBounces, &c.Complaints)
	if err != nil {
		return c, fmt.Errorf("broadcasts: breaker counts %d: %w", id, err)
	}
	return c, nil
}

// breakerSince returns the time after which the breaker judges a
// broadcast's messages, formatted for the database: the last confirmed
// resume after a trip, or "" (everything) when there is none.
func (s *Service) breakerSince(ctx context.Context, q db.Querier, id int64) (string, error) {
	if s.settings == nil {
		return "", nil
	}
	raw, err := s.settings.GetRaw(ctx, q, baselinePrefix+strconv.FormatInt(id, 10))
	if errors.Is(err, core.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var b breakerBaseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return "", fmt.Errorf("broadcasts: breaker baseline %d: %w", id, err)
	}
	if b.Since.IsZero() {
		return "", nil
	}
	return db.FormatTime(b.Since), nil
}

// putBaseline records a confirmed resume at since, so the breaker judges
// only messages sent afterwards.
func (s *Service) putBaseline(ctx context.Context, tx *sql.Tx, id int64, since time.Time) error {
	if s.settings == nil {
		return errors.New("broadcasts: settings are required to resume a tripped broadcast")
	}
	return s.settings.PutRaw(ctx, tx, baselinePrefix+strconv.FormatInt(id, 10), breakerBaseline{Since: since.UTC()})
}

// tripReason returns why the counts trip the breaker, "" when they don't.
// Rates are judged only after cb.MinMessages sent messages.
func tripReason(c BreakerCounts, cb settings.CircuitBreaker) string {
	if c.Sent <= 0 || c.Sent < int64(cb.MinMessages) {
		return ""
	}
	bounce := float64(c.HardBounces) / float64(c.Sent)
	complaint := float64(c.Complaints) / float64(c.Sent)
	switch {
	case bounce > cb.HardBounceRate:
		return fmt.Sprintf("hard-bounce rate %.2f%% (%d of %d sent) is above %.2f%%",
			bounce*100, c.HardBounces, c.Sent, cb.HardBounceRate*100)
	case complaint > cb.ComplaintRate:
		return fmt.Sprintf("complaint rate %.3f%% (%d of %d sent) is above %.3f%%",
			complaint*100, c.Complaints, c.Sent, cb.ComplaintRate*100)
	}
	return ""
}

// CheckCircuitBreakers judges every sending broadcast and pauses (reason
// circuit_breaker) those whose rates are too high, with an alert to the
// owner (key "circuit_breaker:<id>"). It returns the IDs it paused.
func (s *Service) CheckCircuitBreakers(ctx context.Context) ([]int64, error) {
	if s.settings == nil {
		return nil, nil
	}
	st, err := s.settings.Get(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := s.idsWhere(ctx, `status = 'sending'`)
	if err != nil {
		return nil, err
	}
	var paused []int64
	var errs []error
	for _, id := range ids {
		tripped, err := s.checkBreaker(ctx, id, st.CircuitBreaker)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if tripped {
			paused = append(paused, id)
		}
	}
	return paused, errors.Join(errs...)
}

// checkBreaker judges one broadcast and trips it if needed.
func (s *Service) checkBreaker(ctx context.Context, id int64, cb settings.CircuitBreaker) (bool, error) {
	since, err := s.breakerSince(ctx, nil, id)
	if err != nil {
		return false, err
	}
	delta, err := s.breakerCounts(ctx, s.db.Read, id, since)
	if err != nil {
		return false, err
	}
	reason := tripReason(delta, cb)
	if reason == "" {
		return false, nil
	}
	ctx = systemCtx(ctx)
	var tripped bool
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx, `
			UPDATE broadcasts SET status = 'paused', paused_reason = ?, version = version + 1, updated_at = ?
			WHERE id = ? AND status = 'sending' RETURNING name`, PauseCircuitBreaker, s.now(), id).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // paused, cancelled or completed meanwhile
		}
		if err != nil {
			return err
		}
		tripped = true
		if err := s.logAudit(ctx, tx, "broadcast.circuit_breaker", id, map[string]any{
			"status": map[string]any{"from": StatusSending, "to": StatusPaused}, "reason": reason, "counts": delta,
		}); err != nil {
			return err
		}
		return s.alert(ctx, tx, fmt.Sprintf("circuit_breaker:%d", id),
			fmt.Sprintf("Broadcast %q paused by the circuit breaker", name),
			fmt.Sprintf("Mailroom paused broadcast %d (%s) because its %s.\n\n"+
				"This protects the SES account. Check the broadcast's stats and the audience. "+
				"To continue anyway, resume it with {\"confirm\": true}; to stop it, cancel it.", id, name, reason))
	})
	if err != nil {
		return false, fmt.Errorf("broadcasts: trip circuit breaker %d: %w", id, err)
	}
	if tripped {
		s.log.Warn("broadcasts: circuit breaker tripped", "broadcast_id", id, "reason", reason)
	}
	return tripped, nil
}

// idsWhere returns the IDs of broadcasts matching a fixed condition.
func (s *Service) idsWhere(ctx context.Context, cond string, args ...any) ([]int64, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT id FROM broadcasts WHERE `+cond+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("broadcasts: select: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
