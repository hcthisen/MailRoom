package app_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"

	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/forms"
	"mailroom/internal/segments"
	"mailroom/internal/sequences"
)

// dripYAML is a two-step sequence triggered by the tag "joined" that ends
// on the tag "customer".
const dripYAML = `name: Welcome drip
trigger:
  tag: joined
exit_tags: [customer]
steps:
  - delay: "0"
    subject: Welcome, {{first_name | "friend"}}
    body: Hi {{first_name | "there"}}, glad you joined. [Read more](https://example.org/start)
  - delay: "1d"
    subject: Day two
    body: Second email.
`

// importDrip creates the drip sequence.
func (hs *harness) importDrip() sequences.Sequence {
	hs.t.Helper()
	var seq sequences.Sequence
	hs.api("POST", "/sequences/import", fmt.Sprintf(`{"yaml":%q}`, dripYAML)).want(hs.t, http.StatusCreated).decode(hs.t, &seq)
	return seq
}

// runDue runs the scheduler once and sends what is due.
func (hs *harness) runDue() {
	hs.t.Helper()
	if _, err := hs.app.Sequences.RunDue(hs.ctx); err != nil {
		hs.t.Fatal(err)
	}
	if _, err := hs.app.Sending.SendDue(hs.ctx); err != nil {
		hs.t.Fatal(err)
	}
}

// enrollment returns the status and exit reason of the contact's
// enrollment in seq.
func (hs *harness) enrollment(seqID, contactID int64) (status, reason string) {
	hs.t.Helper()
	if err := hs.app.DB.Read.QueryRow(`SELECT status, exit_reason FROM enrollments WHERE sequence_id = ? AND contact_id = ?`,
		seqID, contactID).Scan(&status, &reason); err != nil {
		hs.t.Fatalf("enrollment of contact %d: %v", contactID, err)
	}
	return status, reason
}

// TestSignupDripExitTag: a form sign-up enrols the contact (forms uses
// sequences as its Enroller), the scheduler queues the first step and the
// sender sends it through the provider, and the exit tag added through the
// API ends the drip (TagAdded hook) before the second step.
func TestSignupDripExitTag(t *testing.T) {
	hs := newHarness(t, nil)
	seq := hs.importDrip()
	var f forms.Form
	hs.api("POST", "/forms", fmt.Sprintf(`{"name":"Newsletter","fields":[{"key":"first_name"}],"sequence_id":%d}`, seq.ID)).
		want(t, http.StatusCreated).decode(t, &f)

	hs.raw("POST", "/f/"+f.PublicID, "application/json", `{"email":"Anna@Example.org","first_name":"Anna"}`).want(t, http.StatusOK)
	var c contacts.Contact
	var list struct{ Items []contacts.Contact }
	hs.api("GET", "/contacts?q=anna@example.org", "").want(t, http.StatusOK).decode(t, &list)
	if len(list.Items) != 1 {
		t.Fatalf("contacts = %+v", list.Items)
	}
	c = list.Items[0]
	if st, _ := hs.enrollment(seq.ID, c.ID); st != "active" {
		t.Fatalf("enrollment = %s", st)
	}

	hs.runDue()
	sent := hs.provider.SentTo("anna@example.org")
	if len(sent) != 1 {
		t.Fatalf("sent = %d messages", len(sent))
	}
	msg, err := mail.ReadMessage(bytes.NewReader(sent[0].Raw))
	if err != nil {
		t.Fatal(err)
	}
	if subj := msg.Header.Get("Subject"); subj != "Welcome, Anna" {
		t.Fatalf("subject = %q", subj)
	}
	if !strings.HasPrefix(msg.Header.Get("List-Unsubscribe"), "<https://newsletter.example.com/u/") {
		t.Fatalf("List-Unsubscribe = %q", msg.Header.Get("List-Unsubscribe"))
	}

	hs.api("POST", fmt.Sprintf("/contacts/%d/tags", c.ID), `{"tags":["customer"]}`).want(t, http.StatusOK)
	if st, reason := hs.enrollment(seq.ID, c.ID); st != "exited" || reason != sequences.ExitTag {
		t.Fatalf("after the exit tag: %s %s", st, reason)
	}
	hs.clock.Advance(25 * time.Hour)
	hs.runDue()
	if n := len(hs.provider.SentTo("anna@example.org")); n != 1 {
		t.Fatalf("sent after exit = %d", n)
	}
}

// TestTagTriggerAndOneClickUnsubscribe: tagging a contact through the API
// enrols it (TagAdded hook), and the RFC 8058 one-click POST to the link
// in the sent email unsubscribes it and ends the drip (StatusChanged hook),
// so the second step is never sent.
func TestTagTriggerAndOneClickUnsubscribe(t *testing.T) {
	hs := newHarness(t, nil)
	seq := hs.importDrip()
	var c contacts.Contact
	hs.api("POST", "/contacts", `{"email":"bob@example.org","first_name":"Bob","tags":["joined"]}`).want(t, http.StatusCreated).decode(t, &c)
	if st, _ := hs.enrollment(seq.ID, c.ID); st != "active" {
		t.Fatalf("tag trigger enrollment = %s", st)
	}
	hs.runDue()
	sent := hs.provider.SentTo("bob@example.org")
	if len(sent) != 1 {
		t.Fatalf("sent = %d", len(sent))
	}
	msg, err := mail.ReadMessage(bytes.NewReader(sent[0].Raw))
	if err != nil {
		t.Fatal(err)
	}
	link := strings.Trim(strings.Split(msg.Header.Get("List-Unsubscribe"), ",")[0], "<> ")
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	hs.raw("POST", u.Path, "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click").want(t, http.StatusOK)

	var got contacts.Contact
	hs.api("GET", fmt.Sprintf("/contacts/%d", c.ID), "").want(t, http.StatusOK).decode(t, &got)
	if got.Status != core.StatusUnsubscribed {
		t.Fatalf("status = %s", got.Status)
	}
	if st, reason := hs.enrollment(seq.ID, c.ID); st != "exited" || reason != sequences.ExitNotActive {
		t.Fatalf("after unsubscribe: %s %s", st, reason)
	}
	if n := hs.count(`SELECT count(*) FROM contact_status_changes WHERE contact_id = ? AND source = 'one_click' AND message_id IS NOT NULL`, c.ID); n != 1 {
		t.Fatalf("one-click status changes = %d", n)
	}
	hs.clock.Advance(25 * time.Hour)
	hs.runDue()
	if n := len(hs.provider.SentTo("bob@example.org")); n != 1 {
		t.Fatalf("sent after unsubscribe = %d", n)
	}
}

// TestSuppressionBlocksEverySend: a manual suppression (through the API)
// stops a queued message at send time (D-6), and the address can't be
// re-added.
func TestSuppressionBlocksEverySend(t *testing.T) {
	hs := newHarness(t, nil)
	hs.importDrip()
	var c contacts.Contact
	hs.api("POST", "/contacts", `{"email":"carol@example.org","tags":["joined"]}`).want(t, http.StatusCreated).decode(t, &c)
	if _, err := hs.app.Sequences.RunDue(hs.ctx); err != nil {
		t.Fatal(err)
	}
	hs.api("POST", "/suppressions", `{"email":"carol@example.org","note":"asked by phone"}`).want(t, http.StatusCreated)
	if _, err := hs.app.Sending.SendDue(hs.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(hs.provider.SentTo("carol@example.org")); n != 0 {
		t.Fatalf("a suppressed address got %d emails", n)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE contact_id = ? AND status = 'skipped'`, c.ID); n != 1 {
		t.Fatalf("skipped messages = %d", n)
	}
	hs.api("POST", "/suppressions/remove", `{"email":"carol@example.org","note":"asked again"}`).want(t, http.StatusOK)
	var got contacts.Contact
	hs.api("GET", fmt.Sprintf("/contacts/%d", c.ID), "").decode(t, &got)
	if got.Status != core.StatusActive || hs.count(`SELECT count(*) FROM suppressions`) != 0 {
		t.Fatalf("after removal: %s, %d suppressions", got.Status, hs.count(`SELECT count(*) FROM suppressions`))
	}
}

// TestTagRenameFollowsIntoSegments: contacts is wired to segments as its
// core.TagReferrer, so renaming a tag rewrites saved segments, and a tag a
// segment uses can't be deleted.
func TestTagRenameFollowsIntoSegments(t *testing.T) {
	hs := newHarness(t, nil)
	hs.api("POST", "/contacts", `{"email":"dan@example.org","tags":["vip"]}`).want(t, http.StatusCreated)
	var seg segments.Segment
	hs.api("POST", "/segments", `{"name":"Not VIP","filter":{"rules":[{"field":"tag","op":"not_has","value":"vip"}]}}`).
		want(t, http.StatusCreated).decode(t, &seg)
	var tags struct{ Items []contacts.Tag }
	hs.api("GET", "/tags", "").decode(t, &tags)
	if len(tags.Items) != 1 {
		t.Fatalf("tags = %+v", tags.Items)
	}
	tagID := tags.Items[0].ID

	hs.api("PATCH", fmt.Sprintf("/tags/%d", tagID), `{"name":"gold"}`).want(t, http.StatusOK)
	var after segments.Segment
	hs.api("GET", fmt.Sprintf("/segments/%d", seg.ID), "").want(t, http.StatusOK).decode(t, &after)
	if v, _ := after.Filter.Rules[0].Value.(string); v != "gold" || after.Version != seg.Version+1 {
		t.Fatalf("segment after rename = %+v", after)
	}
	var count segments.SegmentCounts
	hs.api("GET", fmt.Sprintf("/segments/%d/count", seg.ID), "").want(t, http.StatusOK).decode(t, &count)
	if count.Total != 0 {
		t.Fatalf("the only contact has the renamed tag, count = %+v", count)
	}
	hs.api("DELETE", fmt.Sprintf("/tags/%d", tagID), "").want(t, http.StatusConflict)
}

// TestBroadcastEmailLockedWhileSending: once a broadcast is sending, its
// email can't be edited through /emails (B-2).
func TestBroadcastEmailLockedWhileSending(t *testing.T) {
	hs := newHarness(t, nil)
	hs.api("POST", "/contacts", `{"email":"erin@example.org"}`).want(t, http.StatusCreated)
	var b struct {
		ID    int64 `json:"id"`
		Email struct {
			ID int64 `json:"id"`
		} `json:"email"`
	}
	hs.api("POST", "/broadcasts", `{"name":"May news","email":{"subject":"May","body":"Hello"}}`).want(t, http.StatusCreated).decode(t, &b)
	hs.api("PATCH", fmt.Sprintf("/emails/%d", b.Email.ID), `{"subject":"May (edited)"}`).want(t, http.StatusOK)
	hs.api("POST", fmt.Sprintf("/broadcasts/%d/send", b.ID), "").want(t, http.StatusAccepted)
	hs.api("PATCH", fmt.Sprintf("/emails/%d", b.Email.ID), `{"subject":"Too late"}`).want(t, http.StatusConflict)
}
