package broadcasts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// audienceWhere returns the SQL condition over alias c selecting the
// audience's active contacts, with its arguments.
func (s *Service) audienceWhere(ctx context.Context, a BroadcastAudience) (string, []any, error) {
	const active = "c.status = 'active'"
	if a.Type != AudienceSegment {
		return active, nil, nil
	}
	if s.segments == nil {
		return "", nil, core.Invalid("audience.type", "segment audiences are not available")
	}
	expr, args, err := s.segments.WhereSQL(ctx, a.SegmentID)
	if err != nil {
		return "", nil, err
	}
	return active + " AND " + expr, args, nil
}

// suppressedHashes returns which of the given normalised addresses are in
// the suppressions table, keyed by address.
func suppressedHashes(ctx context.Context, q db.Querier, emails []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(emails) == 0 {
		return out, nil
	}
	byHash := make(map[string]string, len(emails))
	args := make([]any, 0, len(emails))
	for _, e := range emails {
		h := core.EmailHash(e)
		if _, dup := byHash[h]; !dup {
			byHash[h] = e
			args = append(args, h)
		}
	}
	rows, err := q.QueryContext(ctx,
		`SELECT email_hash FROM suppressions WHERE email_hash IN (?`+strings.Repeat(",?", len(args)-1)+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("broadcasts: check suppressions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[byHash[h]] = true
	}
	return out, rows.Err()
}

// countRecipients counts the active, unsuppressed contacts of the
// audience right now: the recipients a snapshot would queue.
func (s *Service) countRecipients(ctx context.Context, a BroadcastAudience) (int64, error) {
	where, args, err := s.audienceWhere(ctx, a)
	if err != nil {
		return 0, err
	}
	q := s.db.Read
	var suppressions int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM suppressions`).Scan(&suppressions); err != nil {
		return 0, fmt.Errorf("broadcasts: count suppressions: %w", err)
	}
	if suppressions == 0 {
		var n int64
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM contacts c WHERE `+where, args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("broadcasts: count recipients: %w", err)
		}
		return n, nil
	}
	// The table stores hashes, so hash each candidate address.
	hashes := make(map[string]struct{}, suppressions)
	rows, err := q.QueryContext(ctx, `SELECT email_hash FROM suppressions`)
	if err != nil {
		return 0, fmt.Errorf("broadcasts: read suppressions: %w", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return 0, err
		}
		hashes[h] = struct{}{}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("broadcasts: read suppressions: %w", err)
	}
	rows, err = q.QueryContext(ctx, `SELECT c.email FROM contacts c WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("broadcasts: count recipients: %w", err)
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return 0, err
		}
		if _, sup := hashes[core.EmailHash(email)]; !sup {
			n++
		}
	}
	return n, rows.Err()
}

// missingTags returns the tag names of a segment audience that no tag has
// (renamed, merged or deleted), when the resolver can tell.
func (s *Service) missingTags(ctx context.Context, a BroadcastAudience) ([]string, error) {
	out := []string{}
	if a.Type != AudienceSegment {
		return out, nil
	}
	sc, ok := s.segments.(SegmentCounter)
	if !ok {
		return out, nil
	}
	c, err := sc.CountSegment(ctx, a.SegmentID)
	if errors.Is(err, core.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if c.MissingTags != nil {
		out = c.MissingTags
	}
	return out, nil
}
