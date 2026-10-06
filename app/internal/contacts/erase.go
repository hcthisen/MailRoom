package contacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// EraseResult reports what a GDPR erase removed or anonymised (or, in a
// dry run, would).
type EraseResult struct {
	ContactID  int64          `json:"contact_id" example:"42"`
	EmailHash  string         `json:"email_hash" doc:"SHA-256 of the normalised address, kept in suppressions so it can't be re-added" example:"ff8d9819fc0e12bf0d24892e45987e249a28dce836a85cad60e28eaaa8c6d976"`
	DryRun     bool           `json:"dry_run"`
	Deleted    EraseDeleted   `json:"deleted"`
	Anonymised EraseAnonymous `json:"anonymised"`
}

// EraseDeleted counts the rows deleted with the contact.
type EraseDeleted struct {
	Contact         int `json:"contact" example:"1"`
	Tags            int `json:"tags" doc:"Tag memberships" example:"3"`
	ConsentEvents   int `json:"consent_events" example:"1"`
	StatusChanges   int `json:"status_changes" example:"2"`
	Enrollments     int `json:"enrollments" example:"1"`
	PendingOptins   int `json:"pending_optins" example:"0"`
	StoredResponses int `json:"stored_responses" doc:"Stored Idempotency-Key responses (kept up to 24 h) that contained the address; the keys stay recorded, so a retry answers 409" example:"1"`
}

// EraseAnonymous counts the rows kept for statistics but stripped of the
// contact: messages (its own, those to its address, and test sends that
// used it as the example contact) lose their address, data, personalised
// subject and error text (queued ones are skipped), events their raw detail and the
// recipient in their provider ID, clicks, opens and form submissions their
// contact link, and finished jobs that mention the address their
// parameters and result.
type EraseAnonymous struct {
	Messages        int `json:"messages" example:"12"`
	QueuedSkipped   int `json:"queued_skipped" doc:"Queued messages marked skipped so they are never sent" example:"0"`
	Events          int `json:"events" example:"11"`
	Clicks          int `json:"clicks" example:"4"`
	Opens           int `json:"opens" example:"0"`
	FormSubmissions int `json:"form_submissions" example:"1"`
	JobPayloads     int `json:"job_payloads" doc:"Finished background jobs whose parameters or result mentioned the address; both are emptied" example:"0"`
}

const (
	// erasedError is the error of messages that were queued when their
	// contact was erased.
	erasedError = "contact erased"
	// redactedError replaces the error text of an erased contact's
	// messages: provider errors can quote the recipient.
	redactedError = "redacted: contact erased"
)

// SQL fragments shared by the counting and anonymising statements of Erase.
// Their parameters: ?1 the contact ID, ?2 the address's email hash, ?3 the
// address.
const (
	// messagesOf selects the messages that hold the contact's personal
	// data: its own (contact_id), every message sent to its address
	// whatever the kind (test sends and alerts carry no contact), and test
	// sends rendered with its data as the example contact
	// (data.contact_id, content.SendTestAs).
	messagesOf = `(contact_id = ?1 OR to_email = ?3 OR (contact_id IS NULL AND json_extract(data, '$.contact_id') = ?1))`
	// eventsOf selects the events of those messages, plus every SES event
	// of the address whose email hash is ?2: the events package keys them
	// "<SNS MessageId>:<hash>", also when they could not be linked to a
	// message (unknown, test or alert messages).
	eventsOf = `(message_id IN (SELECT id FROM messages WHERE ` + messagesOf + `) OR provider_event_id LIKE '%:' || ?2)`
	// jobsMentioning matches finished jobs whose parameters or result
	// contain the address (?1 raw, ?2 JSON-escaped). No job type stores
	// addresses today; this keeps erase complete if one ever does. Queued
	// and running jobs are left alone so their handlers don't break.
	jobsMentioning = `status IN ('succeeded', 'failed') AND (instr(params, ?1) > 0 OR instr(params, ?2) > 0 OR instr(result, ?1) > 0 OR instr(result, ?2) > 0)`
)

// Erase deletes a contact and all its personal data (C-6) in one
// transaction: tags, consent log, status history, enrollments and pending
// opt-ins go with the contact row. Its messages (its own, those sent to its
// address such as test sends, and test sends that used it as the example
// contact) keep their counts but lose the address, the data, the rendered
// subject (replaced by the unrendered template subject, which holds merge
// fields rather than values) and the error text, and queued ones are
// marked skipped. Their events lose the raw detail and
// the recipient part of the provider event ID ("<SNS MessageId>:<recipient>"
// becomes "<SNS MessageId>:<hash>"); SES events of the address that are
// not linked to its messages lose their raw detail too. Clicks, opens and
// form submissions lose the contact link, and finished jobs that mention
// the address lose their parameters and result. Stored Idempotency-Key
// responses that contain the address are dropped (api.PurgeStoredResponses),
// so a replay can't return the erased record. The
// address's SHA-256 is added to suppressions (reason erased), so an import
// or form can't re-add it. The audit entry holds the hash, never the
// address. With dryRun nothing changes and the counts say what would.
func (s *Service) Erase(ctx context.Context, id, ifVersion int64, dryRun bool) (EraseResult, error) {
	var res EraseResult
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		c, err := s.store.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, c.Version); err != nil {
			return err
		}
		res = EraseResult{ContactID: id, EmailHash: core.EmailHash(c.Email), DryRun: dryRun}
		addr := []any{c.Email, jsonEscaped(c.Email)}
		if err := countErase(ctx, tx, id, res.EmailHash, c.Email, addr, &res); err != nil {
			return err
		}
		if err := EraseRows(ctx, tx, id, c.Email, s.now()); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "contact.erase", "contact", id, map[string]any{
			"email_hash": res.EmailHash, "deleted": res.Deleted, "anonymised": res.Anonymised,
		})
	})
	if err != nil {
		return EraseResult{}, err
	}
	return res, nil
}

// EraseRows does the writes of Erase for the contact with this id and
// normalised address inside tx, at time now (db.FormatTime): it anonymises
// the messages, events and jobs holding the address, adds the erased
// suppression, deletes the contact row (and with it tags, consent log,
// status history, enrollments and pending opt-ins) and drops stored
// Idempotency-Key responses containing the address. It writes no audit
// entry and counts nothing. mailroom restore uses it to erase again the
// contacts a restored backup brings back.
func EraseRows(ctx context.Context, tx *sql.Tx, id int64, email, now string) error {
	hash := core.EmailHash(email)
	addr := []any{email, jsonEscaped(email)}
	// of are the arguments of messagesOf and eventsOf; the statements'
	// own arguments follow as ?4 and up.
	of := []any{id, hash, email}
	with := func(more ...any) []any { return append(append([]any{}, of...), more...) }
	stmts := []struct {
		query string
		args  []any
	}{
		{`UPDATE messages SET status = 'skipped', error = ?4, updated_at = ?5 WHERE ` + messagesOf + ` AND status = 'queued'`,
			with(erasedError, now)},
		{`UPDATE events SET detail = '{}',
			provider_event_id = CASE WHEN instr(provider_event_id, ':') > 0
				THEN substr(provider_event_id, 1, instr(provider_event_id, ':')) || ?2
				ELSE provider_event_id END
			WHERE ` + eventsOf, of},
		{`UPDATE jobs SET params = '{}', result = '{}' WHERE ` + jobsMentioning, addr},
		{`UPDATE messages SET to_email = NULL, data = '{}',
			subject = COALESCE((SELECT e.subject FROM emails e WHERE e.id = messages.email_id), ''),
			error = CASE WHEN error IN ('', ?4) THEN error ELSE ?5 END,
			updated_at = ?6
			WHERE ` + messagesOf, with(erasedError, redactedError, now)},
		{`INSERT INTO suppressions (email_hash, reason, note, created_at) VALUES (?1, 'erased', '', ?2)
			ON CONFLICT (email_hash) DO UPDATE SET reason = 'erased', note = ''`, []any{hash, now}},
		{`DELETE FROM contacts WHERE id = ?1`, []any{id}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.query, st.args...); err != nil {
			return fmt.Errorf("erase contact %d: %w", id, err)
		}
	}
	if _, err := api.PurgeStoredResponses(ctx, tx, email); err != nil {
		return fmt.Errorf("erase contact %d: %w", id, err)
	}
	return nil
}

// jsonEscaped is s as it appears inside a JSON string (without quotes),
// e.g. with & as &.
func jsonEscaped(s string) string {
	b, _ := json.Marshal(s) // marshalling a string can't fail
	return strings.Trim(string(b), `"`)
}

// countErase fills the counts of res before anything is changed. hash is
// the address's email hash and email the address; addr is the address and
// its JSON-escaped form.
func countErase(ctx context.Context, tx *sql.Tx, id int64, hash, email string, addr []any, res *EraseResult) error {
	byID := []any{id}
	of := []any{id, hash, email}
	counts := []struct {
		dst   *int
		query string
		args  []any
	}{
		{&res.Deleted.Tags, `SELECT count(*) FROM contact_tags WHERE contact_id = ?1`, byID},
		{&res.Deleted.ConsentEvents, `SELECT count(*) FROM consent_events WHERE contact_id = ?1`, byID},
		{&res.Deleted.StatusChanges, `SELECT count(*) FROM contact_status_changes WHERE contact_id = ?1`, byID},
		{&res.Deleted.Enrollments, `SELECT count(*) FROM enrollments WHERE contact_id = ?1`, byID},
		{&res.Deleted.PendingOptins, `SELECT count(*) FROM pending_optins WHERE contact_id = ?1`, byID},
		{&res.Anonymised.Messages, `SELECT count(*) FROM messages WHERE ` + messagesOf, of},
		{&res.Anonymised.QueuedSkipped, `SELECT count(*) FROM messages WHERE ` + messagesOf + ` AND status = 'queued'`, of},
		{&res.Anonymised.Events, `SELECT count(*) FROM events WHERE ` + eventsOf, of},
		{&res.Anonymised.Clicks, `SELECT count(*) FROM clicks WHERE contact_id = ?1`, byID},
		{&res.Anonymised.Opens, `SELECT count(*) FROM opens WHERE contact_id = ?1`, byID},
		{&res.Anonymised.FormSubmissions, `SELECT count(*) FROM form_submissions WHERE contact_id = ?1`, byID},
		{&res.Anonymised.JobPayloads, `SELECT count(*) FROM jobs WHERE ` + jobsMentioning, addr},
	}
	for _, c := range counts {
		if err := tx.QueryRowContext(ctx, c.query, c.args...).Scan(c.dst); err != nil {
			return fmt.Errorf("erase contact %d: count: %w", id, err)
		}
	}
	stored, err := api.CountStoredResponses(ctx, tx, addr[0].(string))
	if err != nil {
		return fmt.Errorf("erase contact %d: %w", id, err)
	}
	res.Deleted.StoredResponses = int(stored)
	res.Deleted.Contact = 1
	return nil
}
