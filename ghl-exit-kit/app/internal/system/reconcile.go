package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/contacts"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// sendingStateKey is the settings key of the global sending pause
// (sending.StateKey; system can't import sending).
const sendingStateKey = "sending_state"

// exitUnsubscribedSequence is enrollments.exit_reason of a per-sequence
// opt-out (sequences.ExitUnsubscribed; system can't import sequences).
const exitUnsubscribedSequence = "unsubscribed_sequence"

// RestoredPauseReason starts the reason of the sending pause a restore
// sets. A manual pause: only a resume through the API or UI lifts it.
const RestoredPauseReason = "restored from a backup"

// ReconcileResult counts what Restore carried over from the replaced
// database (its safety backup) into the restored one, so that nothing a
// contact asked for after the backup was taken is undone.
type ReconcileResult struct {
	// Suppressions: manual and erased suppression rows added.
	Suppressions int `json:"suppressions" doc:"Suppressions (manual or erased) added from the replaced database"`
	// ErasedContacts: contacts the replaced database had erased (GDPR),
	// erased again.
	ErasedContacts int `json:"erased_contacts" doc:"Contacts erased again because the replaced database had erased them"`
	// Statuses: contacts moved to the unsubscribed, bounced, complained or
	// suppressed_manual status they had in the replaced database, or given
	// its unsubscribe request.
	Statuses int `json:"statuses" doc:"Contacts given the opt-out status (or unsubscribe request) they had in the replaced database"`
	// ContactsAdded: contacts created after the backup that had opted out
	// or bounced, added so they stay blocked.
	ContactsAdded int `json:"contacts_added" doc:"Opted-out, bounced or suppressed contacts created after the backup, added so they stay blocked"`
	// Messages: messages queued in the backup that the replaced database
	// shows as sent (or skipped, failed...), plus broadcast and sequence
	// messages sent after the backup, so none is sent twice.
	Messages int `json:"messages" doc:"Messages marked as already sent (or skipped) from the replaced database, so none is sent twice"`
	// Enrollments: sequence enrollments moved on to the step (or the end)
	// they had reached in the replaced database.
	Enrollments int `json:"enrollments" doc:"Sequence enrollments moved on to where the replaced database had them"`
	// TokensRevoked: API tokens that the replaced database had revoked
	// (or no longer had), revoked again, so a leaked token revoked after
	// the backup never works again.
	TokensRevoked int `json:"tokens_revoked" doc:"API tokens revoked again because the replaced database had revoked them or no longer had them"`
	// UsersRemoved: admin UI users the replaced database no longer had
	// (deleted after the backup), deleted again.
	UsersRemoved int `json:"users_removed" doc:"Admin UI users deleted again because the replaced database no longer had them"`
	// UsersUpdated: users given the password and two-factor settings they
	// had in the replaced database, so an old password stops working.
	UsersUpdated int `json:"users_updated" doc:"Admin UI users given the password and two-factor settings of the replaced database"`
}

// blockingStatuses are the statuses a restore carries over: they stop
// marketing email, and losing them would email someone who opted out,
// bounced or was blocked.
var blockingStatuses = []core.ContactStatus{
	core.StatusUnsubscribed, core.StatusBounced, core.StatusComplained, core.StatusSuppressedManual,
}

func isBlocking(st core.ContactStatus) bool {
	for _, b := range blockingStatuses {
		if st == b {
			return true
		}
	}
	return false
}

// pauseRestored sets the global sending pause (a manual pause) in the
// database at d, so a restored server sends nothing until an operator has
// checked it and resumes on purpose. An existing pause is replaced.
func pauseRestored(ctx context.Context, d *db.DB, backup string, now time.Time) error {
	st := map[string]any{
		"paused": true,
		"reason": fmt.Sprintf("%s (%s) at %s: check contacts, broadcasts and sequences, then resume sending",
			RestoredPauseReason, backup, now.UTC().Format(time.RFC3339)),
		"source": "manual",
		"since":  now.UTC(),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?1, ?2, ?3)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		sendingStateKey, string(raw), db.FormatTime(now)); err != nil {
		return fmt.Errorf("system: restore: pause sending: %w", err)
	}
	return nil
}

// reconcile carries into the restored database d what the replaced
// database (its safety backup at newerPath) knew about opt-outs and sends,
// in one transaction: nothing is changed when any step fails.
//
//   - The AUTOINCREMENT counters move up to the replaced database's, so
//     IDs in links already sent (contacts, messages) never name someone
//     else.
//   - Suppression rows (manual and erased) are added.
//   - Contacts whose address the replaced database had erased are erased
//     again (contacts.EraseRows).
//   - A contact whose status there was unsubscribed, bounced, complained
//     or suppressed_manual gets it when the status rules allow the move
//     (it only ever gets harder to email), with a status-change row of
//     source system; an unsubscribe request recorded there
//     (unsubscribe_requested_at) is kept too.
//   - Such contacts created after the backup are added.
//   - Messages queued in the backup that the replaced database has sent,
//     skipped or failed get that state, and broadcast and sequence
//     messages sent after the backup are added, so the broadcast snapshot
//     and the sequence scheduler don't send them again.
//   - Active enrollments move on to the step, or the end, they had there;
//     a per-sequence opt-out (exit reason unsubscribed_sequence) is kept
//     in any status, and added for enrollments created after the backup.
//   - Credentials only ever get revoked: API tokens revoked there (or
//     missing there) are revoked, users missing there are deleted, and
//     users there keep their password hash and TOTP settings from there.
//     (Restore also ends every UI session.)
//
// Changes the other way (a resubscribe, a lifted suppression, new
// contacts, tags, content, new users or tokens) are not carried over: the
// backup's state stays.
func reconcile(ctx context.Context, d *db.DB, newerPath string, now time.Time) (ReconcileResult, error) {
	var res ReconcileResult
	conn, err := d.Write.Conn(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS newer`, newerPath); err != nil {
		return res, fmt.Errorf("open the replaced database: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE newer`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()
	ts := db.FormatTime(now)
	steps := []func(context.Context, *sql.Tx, string, *ReconcileResult) error{
		reconcileSequences, reconcileSuppressions, reconcileErased, reconcileStatuses,
		reconcileNewContacts, reconcileMessages, reconcileEnrollments, reconcileCredentials,
	}
	for _, step := range steps {
		if err := step(ctx, tx, ts, &res); err != nil {
			return ReconcileResult{}, err
		}
	}
	actx := core.WithActor(ctx, core.Actor{Type: core.ActorCLI, Name: "mailroom restore"})
	if err := audit.Write(actx, tx, audit.Entry{
		Operation: "system.restore", TargetType: "database", Changes: map[string]any{"carried_over": res}, At: now,
	}); err != nil {
		return ReconcileResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReconcileResult{}, err
	}
	return res, nil
}

// affected adds the rows a statement changed to *n.
func affected(r sql.Result, n *int) error {
	c, err := r.RowsAffected()
	*n += int(c)
	return err
}

// reconcileSequences raises main's sqlite_sequence counters to the
// replaced database's, for the tables that are AUTOINCREMENT in main (an
// older backup may not have every such table yet; a counter row for a
// plain table would be left behind by a later rebuild).
func reconcileSequences(ctx context.Context, tx *sql.Tx, _ string, _ *ReconcileResult) error {
	var has int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM newer.sqlite_master WHERE name = 'sqlite_sequence'`).Scan(&has); err != nil || has == 0 {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT n.name, n.seq FROM newer.sqlite_sequence n
		WHERE n.name IN (SELECT name FROM main.sqlite_master WHERE type = 'table' AND sql LIKE '%AUTOINCREMENT%')`)
	if err != nil {
		return fmt.Errorf("read counters: %w", err)
	}
	type counter struct {
		name string
		seq  int64
	}
	var cs []counter
	for rows.Next() {
		var c counter
		if err := rows.Scan(&c.name, &c.seq); err != nil {
			rows.Close()
			return err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cs {
		r, err := tx.ExecContext(ctx, `UPDATE main.sqlite_sequence SET seq = max(seq, ?) WHERE name = ?`, c.seq, c.name)
		if err != nil {
			return fmt.Errorf("raise counter %s: %w", c.name, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO main.sqlite_sequence (name, seq) VALUES (?, ?)`, c.name, c.seq); err != nil {
				return fmt.Errorf("raise counter %s: %w", c.name, err)
			}
		}
	}
	return nil
}

func reconcileSuppressions(ctx context.Context, tx *sql.Tx, _ string, res *ReconcileResult) error {
	r, err := tx.ExecContext(ctx, `
		INSERT INTO main.suppressions (email_hash, reason, note, created_at)
		SELECT email_hash, reason, note, created_at FROM newer.suppressions WHERE true
		ON CONFLICT (email_hash) DO UPDATE SET reason = 'erased', note = ''
		WHERE excluded.reason = 'erased' AND suppressions.reason <> 'erased'`)
	if err != nil {
		return fmt.Errorf("carry over suppressions: %w", err)
	}
	return affected(r, &res.Suppressions)
}

func reconcileErased(ctx context.Context, tx *sql.Tx, now string, res *ReconcileResult) error {
	erased := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT email_hash FROM newer.suppressions WHERE reason = 'erased'`)
	if err != nil {
		return fmt.Errorf("read erased addresses: %w", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		erased[h] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(erased) == 0 {
		return err
	}
	type contact struct {
		id    int64
		email string
	}
	var victims []contact
	rows, err = tx.QueryContext(ctx, `SELECT id, email FROM main.contacts`)
	if err != nil {
		return fmt.Errorf("read contacts: %w", err)
	}
	for rows.Next() {
		var c contact
		if err := rows.Scan(&c.id, &c.email); err != nil {
			rows.Close()
			return err
		}
		if erased[core.EmailHash(c.email)] {
			victims = append(victims, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range victims {
		if err := contacts.EraseRows(ctx, tx, c.id, c.email, now); err != nil {
			return err
		}
		res.ErasedContacts++
	}
	return nil
}

// restoreNote is the note of the status changes a restore records.
const restoreNote = "carried over from the replaced database by mailroom restore"

func reconcileStatuses(ctx context.Context, tx *sql.Tx, now string, res *ReconcileResult) error {
	type row struct {
		id                   int64
		cur, newer           core.ContactStatus
		curChanged           string
		newerChanged         string
		curOptOut, newOptOut sql.NullString
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.status, m.status_changed_at, m.unsubscribe_requested_at,
		       n.status, n.status_changed_at, n.unsubscribe_requested_at
		FROM main.contacts m JOIN newer.contacts n ON n.email = m.email
		WHERE n.status IN ('unsubscribed', 'bounced', 'complained', 'suppressed_manual')
		   OR n.unsubscribe_requested_at IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("compare contacts: %w", err)
	}
	var todo []row
	for rows.Next() {
		var r row
		var cur, newer string
		if err := rows.Scan(&r.id, &cur, &r.curChanged, &r.curOptOut, &newer, &r.newerChanged, &r.newOptOut); err != nil {
			rows.Close()
			return err
		}
		r.cur, r.newer = core.ContactStatus(cur), core.ContactStatus(newer)
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range todo {
		changed := false
		status := r.cur
		if r.newer != r.cur && isBlocking(r.newer) &&
			contactstore.CanTransition(r.cur, r.newer, contactstore.SourceSystem, "") == nil {
			status = r.newer
			if _, err := tx.ExecContext(ctx, `
				UPDATE main.contacts SET status = ?1, status_changed_at = ?2, updated_at = ?3, version = version + 1,
				       unsubscribe_requested_at = CASE WHEN ?1 = 'unsubscribed' THEN NULL ELSE unsubscribe_requested_at END
				WHERE id = ?4`, string(status), r.newerChanged, now, r.id); err != nil {
				return fmt.Errorf("carry over the status of contact %d: %w", r.id, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO main.contact_status_changes (contact_id, from_status, to_status, source, actor, note, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				r.id, string(r.cur), string(status), contactstore.SourceSystem, core.ActorCLI, restoreNote, now); err != nil {
				return fmt.Errorf("record the status of contact %d: %w", r.id, err)
			}
			changed = true
		}
		// The contact keeps a status that can't become unsubscribed, but
		// asked to unsubscribe (there, or before it bounced here): keep
		// the request, so lifting the block lands on unsubscribed (U-3).
		if status == core.StatusBounced || status == core.StatusComplained || status == core.StatusSuppressedManual {
			marker := r.newOptOut
			switch {
			case r.newer == core.StatusUnsubscribed:
				marker = sql.NullString{String: r.newerChanged, Valid: true}
			case r.cur == core.StatusUnsubscribed && changed:
				marker = sql.NullString{String: r.curChanged, Valid: true}
			}
			if marker.Valid && !r.curOptOut.Valid {
				if _, err := tx.ExecContext(ctx, `
					UPDATE main.contacts SET unsubscribe_requested_at = ?, updated_at = ?, version = version + CASE WHEN ? THEN 0 ELSE 1 END
					WHERE id = ? AND unsubscribe_requested_at IS NULL`, marker.String, now, changed, r.id); err != nil {
					return fmt.Errorf("carry over the unsubscribe request of contact %d: %w", r.id, err)
				}
				changed = true
			}
		}
		if changed {
			res.Statuses++
		}
	}
	return nil
}

func reconcileNewContacts(ctx context.Context, tx *sql.Tx, now string, res *ReconcileResult) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT n.id, n.email, n.first_name, n.last_name, n.attributes, n.status, n.status_changed_at,
		       n.unsubscribe_requested_at, n.created_at
		FROM newer.contacts n
		WHERE n.status IN ('unsubscribed', 'bounced', 'complained', 'suppressed_manual')
		  AND NOT EXISTS (SELECT 1 FROM main.contacts m WHERE m.email = n.email)`)
	if err != nil {
		return fmt.Errorf("find contacts created after the backup: %w", err)
	}
	type contact struct {
		id                                int64
		email, first, last, attrs, status string
		changed, created                  string
		optOut                            sql.NullString
	}
	var add []contact
	for rows.Next() {
		var c contact
		if err := rows.Scan(&c.id, &c.email, &c.first, &c.last, &c.attrs, &c.status, &c.changed, &c.optOut, &c.created); err != nil {
			rows.Close()
			return err
		}
		add = append(add, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range add {
		// Keep the ID when it is free, so links already sent to the
		// contact keep working.
		var id int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO main.contacts (id, email, first_name, last_name, attributes, status, status_changed_at,
			                           unsubscribe_requested_at, created_at, updated_at)
			VALUES ((SELECT CASE WHEN EXISTS (SELECT 1 FROM main.contacts WHERE id = ?1) THEN NULL ELSE ?1 END),
			        ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)
			RETURNING id`,
			c.id, c.email, c.first, c.last, c.attrs, c.status, c.changed, c.optOut, c.created, now).Scan(&id); err != nil {
			return fmt.Errorf("add a contact created after the backup: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.contact_status_changes (contact_id, from_status, to_status, source, actor, note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, c.status, c.status, contactstore.SourceSystem, core.ActorCLI, restoreNote, now); err != nil {
			return fmt.Errorf("record an added contact: %w", err)
		}
		res.ContactsAdded++
	}
	return nil
}

func reconcileMessages(ctx context.Context, tx *sql.Tx, _ string, res *ReconcileResult) error {
	// Queued (or interrupted) in the backup, done in the replaced database.
	r, err := tx.ExecContext(ctx, `
		UPDATE main.messages AS m SET status = n.status, attempts = n.attempts, ses_message_id = n.ses_message_id,
		       error = n.error, subject = n.subject, sent_at = n.sent_at, delivered_at = n.delivered_at,
		       updated_at = n.updated_at
		FROM newer.messages AS n
		WHERE n.id = m.id AND n.kind = m.kind AND m.status IN ('queued', 'sending')
		  AND n.status NOT IN ('queued', 'sending')`)
	if err != nil {
		return fmt.Errorf("carry over sent messages: %w", err)
	}
	if err := affected(r, &res.Messages); err != nil {
		return err
	}
	// Broadcast and sequence messages created and sent after the backup:
	// the unique (broadcast, contact) and (enrollment, step) keys then
	// stop the broadcast snapshot and the scheduler from sending them
	// again. References the restored database lacks are left empty.
	r, err = tx.ExecContext(ctx, `
		INSERT INTO main.messages (id, kind, broadcast_id, enrollment_id, step_id, email_id, system_template, contact_id,
		                           to_email, data, subject, status, attempts, next_attempt_at, ses_message_id, error,
		                           queued_at, sent_at, delivered_at, updated_at)
		SELECT n.id, n.kind, n.broadcast_id,
		       CASE WHEN EXISTS (SELECT 1 FROM main.enrollments e WHERE e.id = n.enrollment_id AND e.contact_id = n.contact_id) THEN n.enrollment_id END,
		       CASE WHEN EXISTS (SELECT 1 FROM main.sequence_steps s WHERE s.id = n.step_id) THEN n.step_id END,
		       CASE WHEN EXISTS (SELECT 1 FROM main.emails e WHERE e.id = n.email_id) THEN n.email_id END,
		       n.system_template, n.contact_id, n.to_email, n.data, n.subject, n.status, n.attempts, n.next_attempt_at,
		       n.ses_message_id, n.error, n.queued_at, n.sent_at, n.delivered_at, n.updated_at
		FROM newer.messages n
		WHERE n.kind IN ('broadcast', 'sequence') AND n.status NOT IN ('queued', 'sending')
		  AND NOT EXISTS (SELECT 1 FROM main.messages m WHERE m.id = n.id)
		  AND EXISTS (SELECT 1 FROM main.contacts c WHERE c.id = n.contact_id AND c.email = n.to_email)
		  AND (n.broadcast_id IS NULL OR EXISTS (SELECT 1 FROM main.broadcasts b WHERE b.id = n.broadcast_id))
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return fmt.Errorf("carry over messages sent after the backup: %w", err)
	}
	return affected(r, &res.Messages)
}

func reconcileEnrollments(ctx context.Context, tx *sql.Tx, _ string, res *ReconcileResult) error {
	r, err := tx.ExecContext(ctx, `
		UPDATE main.enrollments AS e SET status = n.status, exit_reason = n.exit_reason, next_step_id = n.next_step_id,
		       next_run_at = n.next_run_at, finished_at = n.finished_at, updated_at = n.updated_at
		FROM newer.enrollments AS n
		WHERE n.id = e.id AND n.contact_id = e.contact_id AND n.sequence_id = e.sequence_id AND e.status = 'active'
		  AND (n.status <> 'active' OR n.next_step_id IS NOT e.next_step_id)
		  AND (n.next_step_id IS NULL OR EXISTS (
		        SELECT 1 FROM main.sequence_steps s WHERE s.id = n.next_step_id AND s.sequence_id = e.sequence_id))`)
	if err != nil {
		return fmt.Errorf("carry over enrollments: %w", err)
	}
	if err := affected(r, &res.Enrollments); err != nil {
		return err
	}
	// A per-sequence opt-out ("only stop these emails") made after the
	// backup, on an enrollment in any status there: the sequence must never
	// enrol the contact again (sequences.enrollBlocker).
	r, err = tx.ExecContext(ctx, `
		UPDATE main.enrollments AS e SET status = 'exited', exit_reason = n.exit_reason, next_run_at = NULL,
		       finished_at = n.finished_at, updated_at = n.updated_at
		FROM newer.enrollments AS n
		WHERE n.id = e.id AND n.contact_id = e.contact_id AND n.sequence_id = e.sequence_id
		  AND n.exit_reason = ?1 AND e.exit_reason <> ?1`, exitUnsubscribedSequence)
	if err != nil {
		return fmt.Errorf("carry over sequence opt-outs: %w", err)
	}
	if err := affected(r, &res.Enrollments); err != nil {
		return err
	}
	// Opted-out enrollments created after the backup, as exited rows, for
	// contacts and sequences the restored database has (matched by ID and
	// address, and ID and name: sequence IDs may be reused).
	r, err = tx.ExecContext(ctx, `
		INSERT INTO main.enrollments (sequence_id, contact_id, status, exit_reason, source, started_at, updated_at, finished_at)
		SELECT n.sequence_id, n.contact_id, 'exited', n.exit_reason, n.source, n.started_at, n.updated_at, n.finished_at
		FROM newer.enrollments n
		JOIN newer.contacts nc ON nc.id = n.contact_id
		JOIN newer.sequences ns ON ns.id = n.sequence_id
		WHERE n.exit_reason = ?1
		  AND NOT EXISTS (SELECT 1 FROM main.enrollments e WHERE e.id = n.id AND e.contact_id = n.contact_id AND e.sequence_id = n.sequence_id)
		  AND NOT EXISTS (SELECT 1 FROM main.enrollments e WHERE e.contact_id = n.contact_id AND e.sequence_id = n.sequence_id AND e.exit_reason = ?1)
		  AND EXISTS (SELECT 1 FROM main.contacts c WHERE c.id = n.contact_id AND c.email = nc.email)
		  AND EXISTS (SELECT 1 FROM main.sequences s WHERE s.id = n.sequence_id AND s.name = ns.name)`, exitUnsubscribedSequence)
	if err != nil {
		return fmt.Errorf("carry over sequence opt-outs made after the backup: %w", err)
	}
	return affected(r, &res.Enrollments)
}

// reconcileCredentials carries revocations over from the replaced
// database: a restore must never bring back access that was taken away
// after the backup (a revoked or deleted token, a deleted user, an old
// password or TOTP secret). Users are matched by ID and address (IDs may
// be reused), tokens by their hash. Tokens and users created after the
// backup are not carried over: they are lost with the restore.
func reconcileCredentials(ctx context.Context, tx *sql.Tx, ts string, res *ReconcileResult) error {
	r, err := tx.ExecContext(ctx, `
		UPDATE main.api_tokens AS t
		SET revoked_at = COALESCE((SELECT n.revoked_at FROM newer.api_tokens n WHERE n.token_hash = t.token_hash), ?)
		WHERE t.revoked_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM newer.api_tokens n WHERE n.token_hash = t.token_hash AND n.revoked_at IS NULL)`, ts)
	if err != nil {
		return fmt.Errorf("carry over token revocations: %w", err)
	}
	if err := affected(r, &res.TokensRevoked); err != nil {
		return err
	}
	r, err = tx.ExecContext(ctx, `
		DELETE FROM main.users
		WHERE NOT EXISTS (SELECT 1 FROM newer.users n WHERE n.id = main.users.id AND n.email = main.users.email COLLATE NOCASE)`)
	if err != nil {
		return fmt.Errorf("carry over deleted users: %w", err)
	}
	if err := affected(r, &res.UsersRemoved); err != nil {
		return err
	}
	r, err = tx.ExecContext(ctx, `
		UPDATE main.users AS u SET password_hash = n.password_hash, totp_secret = n.totp_secret,
		       totp_last_step = max(u.totp_last_step, n.totp_last_step), version = max(u.version, n.version) + 1, updated_at = ?
		FROM newer.users AS n
		WHERE n.id = u.id AND n.email = u.email COLLATE NOCASE
		  AND (n.password_hash <> u.password_hash OR n.totp_secret IS NOT u.totp_secret)`, ts)
	if err != nil {
		return fmt.Errorf("carry over passwords: %w", err)
	}
	return affected(r, &res.UsersUpdated)
}
