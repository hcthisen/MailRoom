package segments_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

// TestCountPerformance counts a 50k-contact database with tags, attributes
// and clicks against the PRD §12 target: a 50k segment count in under 1 s.
func TestCountPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: builds a 50k-contact database")
	}
	const contacts = 50_000
	f := newFixture(t)
	ctx := context.Background()
	tagIDs := make([]int64, 8)
	for i := range tagIDs {
		tagIDs[i] = testutil.InsertTag(t, f.db, fmt.Sprintf("tag%d", i))
	}
	link := insertLink(t, f.db, "https://example.com/perf")
	msg := insertMessage(t, f.db, f.b2, f.ids["ann"])
	plans := []string{"free", "basic", "pro", "enterprise"}
	statuses := []string{"active", "active", "active", "unsubscribed", "bounced"}
	err := f.db.WithTx(ctx, func(tx *sql.Tx) error {
		insC, err := tx.PrepareContext(ctx, `INSERT INTO contacts (email, first_name, attributes, status, status_changed_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`)
		if err != nil {
			return err
		}
		insT, err := tx.PrepareContext(ctx, `INSERT INTO contact_tags (contact_id, tag_id, added_at) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		insK, err := tx.PrepareContext(ctx, `INSERT INTO clicks (link_id, message_id, contact_id, created_at) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		for i := range contacts {
			ts := db.FormatTime(testutil.Epoch.Add(-time.Duration(i%400) * day))
			attrs := fmt.Sprintf(`{"plan":%q,"seats":%d,"city":"City %d","vip":%t}`, plans[i%len(plans)], i%50, i%97, i%11 == 0)
			var id int64
			if err := insC.QueryRowContext(ctx, fmt.Sprintf("perf%d@example.com", i), fmt.Sprintf("Name%d", i%300),
				attrs, statuses[i%len(statuses)], ts, ts, ts).Scan(&id); err != nil {
				return err
			}
			for j := range 1 + i%3 {
				if _, err := insT.ExecContext(ctx, id, tagIDs[(i+j*3)%len(tagIDs)], ts); err != nil {
					return err
				}
			}
			if i%7 == 0 {
				if _, err := insK.ExecContext(ctx, link, msg, id, ts); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	filter := parse(t, `{"match":"all","rules":[
		{"field":"tags","op":"has_any","values":["tag1","tag2","tag5"]},
		{"field":"tag","op":"not_has","value":"tag7"},
		{"field":"created_at","op":"within_days","days":300},
		{"match":"any","rules":[
			{"field":"attr","key":"plan","op":"eq","value":"pro"},
			{"field":"attr","key":"seats","op":"eq","value":7},
			{"field":"attr","key":"city","op":"contains","value":"ity 4"},
			{"field":"clicked","op":"any_link","within_days":365},
			{"match":"all","rules":[{"field":"email","op":"ends_with","value":"@example.com"},{"field":"first_name","op":"contains","value":"e1"}]}]}]}`)
	// Warm up once (page cache), then time the count.
	if _, err := f.svc.Count(ctx, filter); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	c, err := f.svc.Count(ctx, filter)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("counted %d contacts: total %d, active %d in %s", contacts, c.Total, c.Active, elapsed)
	if c.Total == 0 || c.Active == 0 || c.Total >= contacts {
		t.Fatalf("implausible counts %+v", c)
	}
	if elapsed > time.Second {
		t.Fatalf("count of %d contacts took %s, want under 1s (PRD §12)", contacts, elapsed)
	}
}
