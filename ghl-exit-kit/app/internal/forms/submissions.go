package forms

import (
	"context"
	"database/sql"
	"fmt"

	"mailroom/internal/db"
)

// Submissions returns up to limit submissions of form formID, newest
// first, with ID below beforeID (0 = from the newest). An unknown form
// wraps core.ErrNotFound.
func (s *Service) Submissions(ctx context.Context, formID, beforeID int64, limit int) ([]Submission, error) {
	if _, err := s.getRecord(ctx, s.db.Read, `id = ?`, formID); err != nil {
		return nil, err
	}
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT fs.id, fs.outcome, fs.contact_id, COALESCE(c.email, ''), fs.created_at
		FROM form_submissions fs LEFT JOIN contacts c ON c.id = fs.contact_id
		WHERE fs.form_id = ? AND fs.id < ?
		ORDER BY fs.id DESC LIMIT ?`, formID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list form submissions: %w", err)
	}
	defer rows.Close()
	out := []Submission{}
	for rows.Next() {
		var (
			sub     Submission
			contact sql.NullInt64
			created string
		)
		if err := rows.Scan(&sub.ID, &sub.Outcome, &contact, &sub.Email, &created); err != nil {
			return nil, fmt.Errorf("scan form submission: %w", err)
		}
		if contact.Valid {
			sub.ContactID = &contact.Int64
		}
		if sub.CreatedAt, err = db.ParseTime(created); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// recordSubmission logs one submission outcome in q.
func (s *Service) recordSubmission(ctx context.Context, q db.Querier, formID, contactID int64, outcome string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO form_submissions (form_id, contact_id, outcome, created_at) VALUES (?, ?, ?, ?)`,
		formID, db.NullID(contactID), outcome, db.FormatTime(s.clock.Now()))
	if err != nil {
		return fmt.Errorf("record form submission: %w", err)
	}
	return nil
}
