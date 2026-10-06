package broadcasts_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/broadcasts"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/segments"
)

func TestCreateAndGet(t *testing.T) {
	hs := newHarness(t)
	seg := hs.segment("VIPs", "vip")
	var b broadcasts.Broadcast
	r := hs.do("POST", "/broadcasts", fmt.Sprintf(`{"name":"  October  ","audience":{"type":"segment","segment_id":%d},
		"from_name":"Example News","reply_to":"Hello@Example.com",
		"email":{"subject":"Hi {{first_name | \"there\"}}","preheader":"News","format":"html","body":"<p>Hi</p>"}}`, seg)).
		wantStatus(t, http.StatusCreated)
	r.decode(t, &b)
	if b.Name != "October" || b.Status != broadcasts.StatusDraft || b.Audience.Type != "segment" || b.Audience.SegmentID != seg ||
		b.Audience.SegmentName != "VIPs" || b.FromName == nil || *b.FromName != "Example News" ||
		b.ReplyTo == nil || *b.ReplyTo != "hello@example.com" || b.Email.Format != "html" || b.Email.Body != "<p>Hi</p>" || b.Version != 1 {
		t.Fatalf("created = %+v", b)
	}
	if r.header.Get("ETag") != api.ETag(1) {
		t.Fatalf("ETag = %q", r.header.Get("ETag"))
	}
	// The broadcast owns an email in the library.
	if n := hs.count(`SELECT count(*) FROM emails WHERE id = ? AND name = 'Broadcast: October'`, b.Email.ID); n != 1 {
		t.Fatalf("email rows = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'broadcast.create' AND target_id = ?`, fmt.Sprint(b.ID)); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}

	var got broadcasts.Broadcast
	hs.doAs(readToken, "GET", fmt.Sprintf("/broadcasts/%d", b.ID), "").wantStatus(t, 200).decode(t, &got)
	if got.Email.Body != "<p>Hi</p>" || got.Audience.SegmentName != "VIPs" {
		t.Fatalf("get = %+v", got)
	}
	// Defaults: audience all, sender from the settings.
	d := hs.create("Default", "")
	if d.Audience.Type != "all" || d.FromName != nil || d.ReplyTo != nil || d.Email.Format != "markdown" {
		t.Fatalf("defaults = %+v", d)
	}
	hs.do("GET", "/broadcasts/999", "").wantError(t, http.StatusNotFound, "not_found", "")
	hs.doAs(readToken, "POST", "/broadcasts", `{"name":"x","email":{"subject":"s","body":"b"}}`).wantError(t, http.StatusForbidden, "forbidden", "")
}

func TestCreateValidation(t *testing.T) {
	hs := newHarness(t)
	email := `"email":{"subject":"Hi","body":"Body"}`
	tests := []struct {
		name, body, field string
	}{
		{"name required", `{"name":"  ",` + email + `}`, "name"},
		{"reply_to off domain", `{"name":"x","reply_to":"a@other.org",` + email + `}`, "reply_to"},
		{"reply_to invalid", `{"name":"x","reply_to":"nope",` + email + `}`, "reply_to"},
		{"from_name control chars", `{"name":"x","from_name":"a\nb",` + email + `}`, "from_name"},
		{"unknown segment", `{"name":"x","audience":{"type":"segment","segment_id":42},` + email + `}`, "audience.segment_id"},
		{"segment id missing", `{"name":"x","audience":{"type":"segment"},` + email + `}`, "audience.segment_id"},
		{"segment id with all", `{"name":"x","audience":{"type":"all","segment_id":3},` + email + `}`, "audience.segment_id"},
		{"bad merge field", `{"name":"x","email":{"subject":"Hi","body":"{{nope}}"}}`, "email.body"},
		{"empty subject", `{"name":"x","email":{"subject":" ","body":"b"}}`, "email.subject"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs.do("POST", "/broadcasts", tc.body).wantError(t, http.StatusUnprocessableEntity, "validation", tc.field)
		})
	}
	if n := hs.count(`SELECT count(*) FROM broadcasts`) + hs.count(`SELECT count(*) FROM emails`); n != 0 {
		t.Fatalf("rows left behind: %d", n)
	}
	// Malformed JSON is a validation error in the one error format.
	hs.do("POST", "/broadcasts", `{"name":`).wantError(t, http.StatusBadRequest, "validation", "")
}

func TestCreateDryRun(t *testing.T) {
	hs := newHarness(t)
	var b broadcasts.Broadcast
	// A dry run created nothing: 200, not 201, and no ETag.
	r := hs.do("POST", "/broadcasts?dry_run=true", `{"name":"x","email":{"subject":"s","body":"b"}}`).wantStatus(t, http.StatusOK)
	if r.header.Get("ETag") != "" {
		t.Fatalf("dry run ETag = %q", r.header.Get("ETag"))
	}
	r.decode(t, &b)
	if b.Name != "x" {
		t.Fatalf("dry run result = %+v", b)
	}
	if n := hs.count(`SELECT count(*) FROM broadcasts`) + hs.count(`SELECT count(*) FROM emails`) + hs.count(`SELECT count(*) FROM audit_log WHERE operation LIKE 'broadcast.%'`); n != 0 {
		t.Fatalf("dry run left %d rows", n)
	}
}

func TestUpdateETag(t *testing.T) {
	hs := newHarness(t)
	b := hs.create("B", "")
	path := fmt.Sprintf("/broadcasts/%d", b.ID)

	// Stale If-Match: 412, nothing changes.
	hs.do("PATCH", path, `{"name":"x"}`, "If-Match", `"v9"`).wantError(t, http.StatusPreconditionFailed, "precondition_failed", "")
	hs.do("PATCH", path, `{"name":"x"}`, "If-Match", `garbage`).wantError(t, http.StatusUnprocessableEntity, "validation", "header.If-Match")

	var got broadcasts.Broadcast
	r := hs.do("PATCH", path, `{"name":"Renamed","from_name":"Team","reply_to":"","email":{"subject":"New {{email}}","body":"New body"}}`, "If-Match", api.ETag(b.Version)).wantStatus(t, 200)
	r.decode(t, &got)
	if got.Name != "Renamed" || got.FromName == nil || *got.FromName != "Team" || got.ReplyTo == nil || *got.ReplyTo != "" ||
		got.Email.Subject != "New {{email}}" || got.Email.Body != "New body" || got.Version <= b.Version {
		t.Fatalf("patched = %+v", got)
	}
	if r.header.Get("ETag") != api.ETag(got.Version) {
		t.Fatalf("ETag %q, version %d", r.header.Get("ETag"), got.Version)
	}
	// The old ETag no longer matches.
	hs.do("PATCH", path, `{"name":"x"}`, "If-Match", api.ETag(b.Version)).wantError(t, http.StatusPreconditionFailed, "precondition_failed", "")

	// A change of the email through the emails API also moves the ETag.
	before := hs.get(b.ID).Version
	if err := hs.d.WithTx(hs.ctx, func(tx *sql.Tx) error {
		other := "Other"
		_, err := hs.content.UpdateEmail(hs.ctx, tx, got.Email.ID, content.EmailPatch{Subject: &other}, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after := hs.get(b.ID).Version; after <= before {
		t.Fatalf("version %d -> %d after email change", before, after)
	}

	// Reset the sender overrides; the email name follows the rename.
	hs.do("PATCH", path, `{"from_name":"","use_default_reply_to":true}`).wantStatus(t, 200).decode(t, &got)
	if got.FromName != nil || got.ReplyTo != nil {
		t.Fatalf("reset = %+v", got)
	}
	if n := hs.count(`SELECT count(*) FROM emails WHERE id = ? AND name = 'Broadcast: Renamed'`, got.Email.ID); n != 1 {
		t.Fatalf("email not renamed")
	}
	hs.do("PATCH", path, `{"reply_to":"a@example.com","use_default_reply_to":true}`).wantError(t, http.StatusUnprocessableEntity, "validation", "reply_to")

	// Validation of the email content is reported under email.*.
	hs.do("PATCH", path, `{"email":{"format":"html","body":"{{x.y}}"}}`).wantError(t, http.StatusUnprocessableEntity, "validation", "email.body")

	// A patch that changes nothing keeps the version.
	v := hs.get(b.ID).Version
	hs.do("PATCH", path, `{"name":"Renamed"}`).wantStatus(t, 200).decode(t, &got)
	if got.Version != v {
		t.Fatalf("no-op patch bumped the version %d -> %d", v, got.Version)
	}

	// Dry run: validated, not stored.
	hs.do("PATCH", path+"?dry_run=true", `{"name":"Dry"}`).wantStatus(t, 200).decode(t, &got)
	if got.Name != "Dry" || hs.get(b.ID).Name != "Renamed" {
		t.Fatalf("dry run patch stored")
	}

	// An audience change is validated and resolved.
	seg := hs.segment("S", "t")
	hs.do("PATCH", path, fmt.Sprintf(`{"audience":{"type":"segment","segment_id":%d}}`, seg)).wantStatus(t, 200).decode(t, &got)
	if got.Audience.SegmentName != "S" {
		t.Fatalf("audience = %+v", got.Audience)
	}
}

func TestListPagination(t *testing.T) {
	hs := newHarness(t)
	var ids []int64
	for i := range 5 {
		ids = append(ids, hs.create(fmt.Sprintf("B%d", i), "").ID)
	}
	hs.setStatus(ids[1], broadcasts.StatusCancelled, "")
	hs.setStatus(ids[3], broadcasts.StatusCancelled, "")

	var page api.ListBody[broadcasts.Broadcast]
	hs.do("GET", "/broadcasts?limit=2", "").wantStatus(t, 200).decode(t, &page)
	if len(page.Items) != 2 || page.Items[0].ID != ids[4] || page.Items[1].ID != ids[3] || page.NextCursor == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	if page.Items[0].Email.Body != "" || page.Items[0].Email.Subject == "" {
		t.Fatalf("list item email = %+v", page.Items[0].Email)
	}
	var seen []int64
	cursor := ""
	for {
		var p api.ListBody[broadcasts.Broadcast]
		hs.do("GET", "/broadcasts?limit=2&cursor="+cursor, "").wantStatus(t, 200).decode(t, &p)
		for _, b := range p.Items {
			seen = append(seen, b.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if fmt.Sprint(seen) != fmt.Sprint([]int64{ids[4], ids[3], ids[2], ids[1], ids[0]}) {
		t.Fatalf("all pages = %v", seen)
	}
	hs.do("GET", "/broadcasts?status=cancelled", "").wantStatus(t, 200).decode(t, &page)
	if len(page.Items) != 2 || page.Items[0].ID != ids[3] || page.Items[1].ID != ids[1] {
		t.Fatalf("cancelled = %+v", page.Items)
	}
	hs.do("GET", "/broadcasts?status=bogus", "").wantError(t, http.StatusUnprocessableEntity, "validation", "")
	hs.do("GET", "/broadcasts?cursor=zzz", "").wantError(t, http.StatusUnprocessableEntity, "validation", "query.cursor")
}

func TestDeleteRemovesEmailAndMessages(t *testing.T) {
	hs := newHarness(t)
	ids := hs.contacts("a", 3, core.StatusActive, "")
	b := hs.create("B", "")
	hs.sendAndSnapshot(b.ID)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/pause", b.ID), "").wantStatus(t, 200)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/cancel", b.ID), "").wantStatus(t, 200)
	_ = ids

	hs.do("DELETE", fmt.Sprintf("/broadcasts/%d", b.ID), "", "If-Match", `"v1"`).wantError(t, http.StatusPreconditionFailed, "precondition_failed", "")
	hs.do("DELETE", fmt.Sprintf("/broadcasts/%d", b.ID), "", "If-Match", api.ETag(hs.get(b.ID).Version)).wantStatus(t, http.StatusNoContent)
	if n := hs.count(`SELECT count(*) FROM emails WHERE id = ?`, b.Email.ID) + hs.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("left %d rows", n)
	}
	hs.do("DELETE", fmt.Sprintf("/broadcasts/%d", b.ID), "").wantError(t, http.StatusNotFound, "not_found", "")
}

func TestDuplicate(t *testing.T) {
	hs := newHarness(t)
	src := hs.create(strings.Repeat("n", 200), `{"type":"all"}`)
	hs.do("PATCH", fmt.Sprintf("/broadcasts/%d", src.ID), `{"from_name":"Team","reply_to":"hi@example.com"}`).wantStatus(t, 200)
	hs.setStatus(src.ID, broadcasts.StatusSent, "")
	var dup broadcasts.Broadcast
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/duplicate", src.ID), "").wantStatus(t, http.StatusCreated).decode(t, &dup)
	if dup.ID == src.ID || dup.Status != broadcasts.StatusDraft || !strings.HasSuffix(dup.Name, " (copy)") || len([]rune(dup.Name)) != 200 ||
		dup.Email.ID == src.Email.ID || dup.Email.Subject != src.Email.Subject || dup.Email.Body != src.Email.Body ||
		dup.FromName == nil || *dup.FromName != "Team" || dup.ReplyTo == nil || *dup.ReplyTo != "hi@example.com" {
		t.Fatalf("duplicate = %+v", dup)
	}
	hs.do("POST", "/broadcasts/999/duplicate", "").wantError(t, http.StatusNotFound, "not_found", "")
}

// TestDuplicateResolvesSegment: a duplicate's segment audience is resolved
// again. A renamed segment shows its current name; a deleted one is 422,
// and a new segment never takes over its ID (segments.id is AUTOINCREMENT),
// so the copy can't show one segment's name and send to another.
func TestDuplicateResolvesSegment(t *testing.T) {
	hs := newHarness(t)
	seg := hs.segment("Customers without VIP", "customer")
	src := hs.create("October", fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg))
	hs.setStatus(src.ID, broadcasts.StatusCancelled, "")
	renamed := "Customers"
	if _, err := hs.segments.Update(hs.ctx, seg, segments.UpdateInput{Name: &renamed}, 0, false); err != nil {
		t.Fatal(err)
	}
	var dup broadcasts.Broadcast
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/duplicate", src.ID), "").wantStatus(t, http.StatusCreated).decode(t, &dup)
	if dup.Audience.SegmentID != seg || dup.Audience.SegmentName != "Customers" {
		t.Fatalf("duplicate audience = %+v, want the segment's current name", dup.Audience)
	}
	hs.setStatus(dup.ID, broadcasts.StatusCancelled, "")

	if err := hs.segments.Delete(hs.ctx, seg, 0, false); err != nil {
		t.Fatal(err)
	}
	if other := hs.segment("tmp seg", "customer"); other == seg {
		t.Fatalf("a new segment reused the deleted segment's ID %d", seg)
	}
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/duplicate", src.ID), "").
		wantError(t, http.StatusUnprocessableEntity, "validation", "audience.segment_id")
}

func TestSendTest(t *testing.T) {
	hs := newHarness(t)
	c := hs.contacts("c", 1, core.StatusUnsubscribed, "")
	b := hs.create("B", "")
	var res broadcasts.BroadcastTestResult
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/test", b.ID), fmt.Sprintf(`{"to":["A@example.org","a@example.org","b@example.org"],"contact_id":%d}`, c[0])).
		wantStatus(t, http.StatusAccepted).decode(t, &res)
	if len(res.MessageIDs) != 2 || res.To[0] != "a@example.org" {
		t.Fatalf("test result = %+v", res)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'test' AND broadcast_id IS NULL AND email_id = ? AND json_extract(data, '$.test') = 1`, b.Email.ID); n != 2 {
		t.Fatalf("test messages = %d", n)
	}
	// Without overrides the messages carry no sender; with them, the tests
	// use the broadcast's from_name and reply_to (B-7).
	if n := hs.count(`SELECT count(*) FROM messages WHERE kind = 'test' AND json_type(data, '$.from_name') IS NULL AND json_type(data, '$.reply_to') IS NULL`); n != 2 {
		t.Fatalf("tests without override = %d", n)
	}
	hs.do("PATCH", fmt.Sprintf("/broadcasts/%d", b.ID), `{"from_name":"Special","reply_to":""}`).wantStatus(t, 200)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/test", b.ID), `{"to":["c@example.org"]}`).wantStatus(t, http.StatusAccepted).decode(t, &res)
	if n := hs.count(`SELECT count(*) FROM messages WHERE id = ? AND json_extract(data, '$.from_name') = 'Special' AND json_extract(data, '$.reply_to') = ''`, res.MessageIDs[0]); n != 1 {
		t.Fatalf("override not stored on test message %d", res.MessageIDs[0])
	}
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/test", b.ID), `{"to":[]}`).wantError(t, http.StatusUnprocessableEntity, "validation", "")
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/test", b.ID), `{"to":["1@x.org","2@x.org","3@x.org","4@x.org","5@x.org","6@x.org"]}`).wantError(t, http.StatusUnprocessableEntity, "validation", "")
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/test", b.ID), `{"to":["nope"]}`).wantError(t, http.StatusUnprocessableEntity, "validation", "to[0]")
	// Tests don't count in the stats.
	var st broadcasts.BroadcastStats
	hs.do("GET", fmt.Sprintf("/broadcasts/%d/stats", b.ID), "").wantStatus(t, 200).decode(t, &st)
	if st.Recipients != 0 || st.Queued != 0 {
		t.Fatalf("stats count tests: %+v", st)
	}
}

// TestIdempotentSend: a retried send with the same Idempotency-Key replays
// the first response and starts nothing twice.
func TestIdempotentSend(t *testing.T) {
	hs := newHarness(t)
	b := hs.create("B", "")
	path := fmt.Sprintf("/broadcasts/%d/send", b.ID)
	first := hs.do("POST", path, "", api.IdempotencyHeader, "send-1").wantStatus(t, http.StatusAccepted)
	again := hs.do("POST", path, "", api.IdempotencyHeader, "send-1").wantStatus(t, http.StatusAccepted)
	if string(first.body) != string(again.body) || again.header.Get(api.IdempotencyReplayHeader) == "" {
		t.Fatalf("replay differs: %s vs %s (%v)", first.body, again.body, again.header)
	}
	if n := hs.count(`SELECT count(*) FROM jobs WHERE type = ?`, broadcasts.JobSnapshot); n != 1 {
		t.Fatalf("snapshot jobs = %d", n)
	}
	// Without the key, a second send is a conflict: the broadcast is sending.
	hs.do("POST", path, "").wantError(t, http.StatusConflict, "conflict", "")
}
