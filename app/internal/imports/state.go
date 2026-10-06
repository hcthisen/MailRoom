package imports

import (
	"context"
	"fmt"
	"strings"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// dbState is what the database knows about a batch of records' addresses.
type dbState struct {
	existing   map[string]*existingContact // by normalised email
	suppressed map[string]bool             // by normalised email
}

// loadState reads the contacts and suppressions for recs' addresses
// through q (the batch's transaction, or the read pool for a dry run).
func loadState(ctx context.Context, q db.Querier, recs []*record) (dbState, error) {
	st := dbState{existing: map[string]*existingContact{}, suppressed: map[string]bool{}}
	if len(recs) == 0 {
		return st, nil
	}
	emails := make([]any, len(recs))
	hashes := make([]any, len(recs))
	byHash := make(map[string]string, len(recs))
	for i, r := range recs {
		emails[i] = r.email
		h := core.EmailHash(r.email)
		hashes[i] = h
		byHash[h] = r.email
	}

	rows, err := q.QueryContext(ctx, `SELECT email_hash FROM suppressions WHERE email_hash IN (`+marks(len(hashes))+`)`, hashes...)
	if err != nil {
		return st, fmt.Errorf("imports: read suppressions: %w", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return st, err
		}
		st.suppressed[byHash[h]] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows, err = q.QueryContext(ctx, `SELECT id, email, first_name, last_name, attributes, status
		FROM contacts WHERE email IN (`+marks(len(emails))+`)`, emails...)
	if err != nil {
		return st, fmt.Errorf("imports: read contacts: %w", err)
	}
	byID := map[int64]*existingContact{}
	for rows.Next() {
		var (
			c            existingContact
			email, attrs string
			status       string
		)
		if err := rows.Scan(&c.id, &email, &c.firstName, &c.lastName, &attrs, &status); err != nil {
			rows.Close()
			return st, err
		}
		c.status = core.ContactStatus(status)
		c.tags = map[string]bool{}
		if err := db.UnmarshalJSON(attrs, &c.attrs); err != nil {
			rows.Close()
			return st, fmt.Errorf("imports: contact %d attributes: %w", c.id, err)
		}
		st.existing[email] = &c
		byID[c.id] = &c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	if len(byID) == 0 {
		return st, nil
	}

	ids := make([]any, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	rows, err = q.QueryContext(ctx, `SELECT ct.contact_id, t.name FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
		WHERE ct.contact_id IN (`+marks(len(ids))+`)`, ids...)
	if err != nil {
		return st, fmt.Errorf("imports: read contact tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return st, err
		}
		byID[id].tags[foldASCII(name)] = true
	}
	return st, rows.Err()
}

// marks returns n comma-separated "?" placeholders.
func marks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
