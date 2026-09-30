package broadcasts_test

import (
	"fmt"
	"math"
	"net/http"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

func TestStats(t *testing.T) {
	hs := newHarness(t)
	b := hs.create("B", "")
	hs.setStatus(b.ID, broadcasts.StatusSending, "")
	ids := hs.sentMessages(b.ID, b.Email.ID, "m", 10) // 10 sent
	now := db.FormatTime(hs.clock.Now())
	// 6 delivered, of which 1 later complained; 2 hard-bounced; 1 soft bounce only.
	for _, id := range ids[:6] {
		testutil.Exec(t, hs.d, `UPDATE messages SET status = 'delivered', delivered_at = ? WHERE id = ?`, now, id)
	}
	testutil.Exec(t, hs.d, `UPDATE messages SET status = 'complained' WHERE id = ?`, ids[5])
	hs.event(ids[5], "complaint", "")
	for _, id := range ids[6:8] {
		testutil.Exec(t, hs.d, `UPDATE messages SET status = 'bounced' WHERE id = ?`, id)
		hs.event(id, "bounce", "permanent")
	}
	hs.event(ids[8], "bounce", "transient")
	// Unsubscribes: one by link event, one recorded as a status change.
	hs.event(ids[0], "unsubscribe", "one_click")
	var c1 int64
	if err := hs.d.Read.QueryRow(`SELECT contact_id FROM messages WHERE id = ?`, ids[1]).Scan(&c1); err != nil {
		t.Fatal(err)
	}
	testutil.Exec(t, hs.d, `INSERT INTO contact_status_changes (contact_id, from_status, to_status, source, message_id, created_at) VALUES (?, 'active', 'unsubscribed', 'link', ?, ?)`, c1, ids[1], now)
	// Not yet sent: 2 queued, 1 failed, 1 skipped.
	extra := hs.contacts("q", 4, core.StatusActive, "")
	for i, st := range []string{"queued", "queued", "failed", "skipped"} {
		testutil.Exec(t, hs.d, `INSERT INTO messages (kind, broadcast_id, email_id, contact_id, to_email, status, next_attempt_at, queued_at, updated_at)
			VALUES ('broadcast', ?, ?, ?, ?, ?, ?, ?, ?)`, b.ID, b.Email.ID, extra[i], fmt.Sprintf("q%d@example.net", i), st, now, now, now)
	}
	// Clicks: offer 3 clicks by 2 recipients, blog 1 click.
	click := func(msg int64, url string) {
		testutil.Exec(t, hs.d, `INSERT INTO links (url, created_at) VALUES (?, ?) ON CONFLICT DO NOTHING`, url, now)
		testutil.Exec(t, hs.d, `INSERT INTO clicks (link_id, message_id, created_at) VALUES ((SELECT id FROM links WHERE url = ?), ?, ?)`, url, msg, now)
	}
	click(ids[0], "https://example.com/offer")
	click(ids[0], "https://example.com/offer")
	click(ids[1], "https://example.com/offer")
	click(ids[1], "https://example.com/blog")
	// Messages of other broadcasts and tests don't count.
	other := hs.create("Other", "")
	hs.sentMessages(other.ID, other.Email.ID, "o", 3)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/test", b.ID), `{"to":["owner@example.org"]}`).wantStatus(t, http.StatusAccepted)

	var st broadcasts.BroadcastStats
	hs.doAs(readToken, "GET", fmt.Sprintf("/broadcasts/%d/stats", b.ID), "").wantStatus(t, 200).decode(t, &st)
	want := broadcasts.BroadcastStats{
		BroadcastID: b.ID, Status: "sending", Recipients: 14, Queued: 2, Sent: 10, Delivered: 6, Bounced: 2, Complained: 1,
		Failed: 1, Skipped: 1, Unsubscribed: 2, UniqueClicks: 2, Clicks: 4,
	}
	gotCounts := st
	gotCounts.ClicksPerLink, gotCounts.Rates, gotCounts.MissingTags = nil, broadcasts.BroadcastRates{}, nil
	if fmt.Sprintf("%+v", gotCounts) != fmt.Sprintf("%+v", want) {
		t.Fatalf("stats =\n%+v\nwant\n%+v", gotCounts, want)
	}
	if len(st.ClicksPerLink) != 2 || st.ClicksPerLink[0] != (broadcasts.BroadcastLinkClicks{URL: "https://example.com/offer", Clicks: 3, Unique: 2}) ||
		st.ClicksPerLink[1] != (broadcasts.BroadcastLinkClicks{URL: "https://example.com/blog", Clicks: 1, Unique: 1}) {
		t.Fatalf("clicks per link = %+v", st.ClicksPerLink)
	}
	for name, pair := range map[string][2]float64{
		"delivery": {st.Rates.Delivery, 0.6}, "bounce": {st.Rates.Bounce, 0.2}, "complaint": {st.Rates.Complaint, 0.1},
		"unsubscribe": {st.Rates.Unsubscribe, 0.2}, "click": {st.Rates.Click, 0.2},
	} {
		if math.Abs(pair[0]-pair[1]) > 1e-9 {
			t.Errorf("%s rate = %v, want %v", name, pair[0], pair[1])
		}
	}
	if st.MissingTags == nil {
		t.Fatal("missing_tags must be [] not null")
	}
	// Once the snapshot is done, recipients is its total.
	testutil.Exec(t, hs.d, `UPDATE broadcasts SET snapshot_done_at = ?, recipients_total = 20 WHERE id = ?`, now, b.ID)
	hs.do("GET", fmt.Sprintf("/broadcasts/%d/stats", b.ID), "").wantStatus(t, 200).decode(t, &st)
	if st.Recipients != 20 {
		t.Fatalf("recipients = %d", st.Recipients)
	}
	hs.do("GET", "/broadcasts/999/stats", "").wantError(t, http.StatusNotFound, "not_found", "")
}

func TestRecipients(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("c", 7, core.StatusActive, "")
	b := hs.create("B", "")
	hs.sendAndSnapshot(b.ID)
	testutil.Exec(t, hs.d, `UPDATE messages SET status = 'failed', error = 'MessageRejected' WHERE id IN (SELECT id FROM messages WHERE broadcast_id = ? ORDER BY id LIMIT 2)`, b.ID)
	// An erased recipient has no contact or address.
	testutil.Exec(t, hs.d, `UPDATE messages SET contact_id = NULL, to_email = NULL WHERE id = (SELECT max(id) FROM messages WHERE broadcast_id = ?)`, b.ID)

	var all []broadcasts.BroadcastRecipient
	cursor := ""
	for {
		var p api.ListBody[broadcasts.BroadcastRecipient]
		hs.doAs(readToken, "GET", fmt.Sprintf("/broadcasts/%d/recipients?limit=3&cursor=%s", b.ID, cursor), "").wantStatus(t, 200).decode(t, &p)
		all = append(all, p.Items...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(all) != 7 {
		t.Fatalf("recipients = %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].MessageID <= all[i-1].MessageID {
			t.Fatal("not in message order")
		}
	}
	if last := all[6]; last.ContactID != nil || last.Email != nil {
		t.Fatalf("erased recipient = %+v", last)
	}
	if first := all[0]; first.Status != "failed" || first.Error != "MessageRejected" || first.Email == nil || *first.Email != "c0@example.net" {
		t.Fatalf("first = %+v", first)
	}
	var failed api.ListBody[broadcasts.BroadcastRecipient]
	hs.do("GET", fmt.Sprintf("/broadcasts/%d/recipients?status=failed", b.ID), "").wantStatus(t, 200).decode(t, &failed)
	if len(failed.Items) != 2 || failed.NextCursor != "" {
		t.Fatalf("failed = %+v", failed)
	}
	hs.do("GET", fmt.Sprintf("/broadcasts/%d/recipients?status=nope", b.ID), "").wantError(t, http.StatusUnprocessableEntity, "validation", "")
	hs.do("GET", "/broadcasts/999/recipients", "").wantError(t, http.StatusNotFound, "not_found", "")
}
