package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mailroom/internal/db"
)

// storedResponseContains matches idempotency_keys rows holding a stored
// response whose body contains ?1 (as is) or ?2 (JSON-escaped).
const storedResponseContains = `status_code > 0 AND (instr(CAST(response_body AS TEXT), ?1) > 0 OR instr(CAST(response_body AS TEXT), ?2) > 0)`

// PurgeStoredResponses drops the stored response bodies of Idempotency-Key
// records whose response contains s, as is or JSON-escaped, e.g. an email
// address that a GDPR erase removes. It runs on q, normally the caller's
// transaction, so the purge commits with the erase. The keys stay recorded
// as carried out without a response, so a retry with the same key is
// answered 409 conflict: it is neither replayed (the data is gone) nor run
// a second time (which could repeat a side effect such as a test send).
// It returns the number of records changed; an empty s changes nothing.
func PurgeStoredResponses(ctx context.Context, q db.Querier, s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	res, err := q.ExecContext(ctx,
		`UPDATE idempotency_keys SET status_code = ?3, response_body = x'', response_type = 'application/json'
		 WHERE `+storedResponseContains, s, jsonEscaped(s), keyCommitted)
	if err != nil {
		return 0, fmt.Errorf("purge stored responses: %w", err)
	}
	return res.RowsAffected()
}

// CountStoredResponses counts the records PurgeStoredResponses would
// change, without changing them (for dry runs).
func CountStoredResponses(ctx context.Context, q db.Querier, s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE `+storedResponseContains,
		s, jsonEscaped(s)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count stored responses: %w", err)
	}
	return n, nil
}

// jsonEscaped is s as encoding/json writes it inside a JSON string
// (without the quotes), e.g. with & as &.
func jsonEscaped(s string) string {
	b, _ := json.Marshal(s) // marshalling a string can't fail
	return strings.TrimSuffix(strings.TrimPrefix(string(b), `"`), `"`)
}
