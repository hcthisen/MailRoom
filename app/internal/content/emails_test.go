package content_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/outbox"
	"mailroom/internal/testutil"
)

func ptr[T any](v T) *T { return &v }

func TestValidateEmail(t *testing.T) {
	ok := content.EmailInput{Subject: "Hi", Body: "Body"}
	tests := []struct {
		name  string
		mod   func(*content.EmailInput)
		field string
	}{
		{"ok", func(*content.EmailInput) {}, ""},
		{"html", func(in *content.EmailInput) { in.Format = "html" }, ""},
		{"merge everywhere", func(in *content.EmailInput) {
			in.Subject, in.Preheader, in.Body = "{{first_name}}", `{{attr.x | "y"}}`, "{{email}} {{unsubscribe_url}}"
		}, ""},
		{"subject missing", func(in *content.EmailInput) { in.Subject = "  " }, "subject"},
		{"subject 300 chars", func(in *content.EmailInput) { in.Subject = strings.Repeat("é", 300) }, ""},
		{"subject too long", func(in *content.EmailInput) { in.Subject = strings.Repeat("a", 301) }, "subject"},
		{"subject newline", func(in *content.EmailInput) { in.Subject = "Hi\r\nBcc: x@y.z" }, "subject"},
		{"subject merge", func(in *content.EmailInput) { in.Subject = "{{name}}" }, "subject"},
		{"preheader merge", func(in *content.EmailInput) { in.Preheader = "{{first_name | x}}" }, "preheader"},
		{"preheader too long", func(in *content.EmailInput) { in.Preheader = strings.Repeat("a", 301) }, "preheader"},
		{"name too long", func(in *content.EmailInput) { in.Name = strings.Repeat("a", 201) }, "name"},
		{"format", func(in *content.EmailInput) { in.Format = "mjml" }, "format"},
		{"body empty", func(in *content.EmailInput) { in.Body = " \n " }, "body"},
		{"body 1 MiB", func(in *content.EmailInput) { in.Body = strings.Repeat("a", content.MaxBodyBytes) }, ""},
		{"body too big", func(in *content.EmailInput) { in.Body = strings.Repeat("a", content.MaxBodyBytes+1) }, "body"},
		{"body invalid utf8", func(in *content.EmailInput) { in.Body = "a\xffb" }, "body"},
		{"body unknown field", func(in *content.EmailInput) { in.Body = "Hi {{contact.first_name}}" }, "body"},
		{"body unclosed", func(in *content.EmailInput) { in.Body = "Hi {{first_name" }, "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := ok
			tt.mod(&in)
			got, err := content.ValidateEmail(in)
			if tt.field == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.Format != content.FormatMarkdown && got.Format != content.FormatHTML {
					t.Fatalf("format = %q", got.Format)
				}
				return
			}
			if fieldOf(err) != tt.field {
				t.Fatalf("err = %v, want validation error for %s", err, tt.field)
			}
		})
	}
}

func TestEmailCRUD(t *testing.T) {
	f := newFixture(t)
	e := f.createEmail(content.EmailInput{Name: "  Welcome  ", Subject: " Hi ", Body: "Hello"})
	if e.ID == 0 || e.Name != "Welcome" || e.Subject != "Hi" || e.Format != "markdown" || e.Version != 1 || !e.CreatedAt.Equal(testutil.Epoch) {
		t.Fatalf("created = %+v", e)
	}
	got, err := f.svc.GetEmail(f.ctx, nil, e.ID)
	if err != nil || got != e {
		t.Fatalf("get = %+v %v, want %+v", got, err, e)
	}
	if ok, err := f.svc.EmailExists(f.ctx, nil, e.ID); !ok || err != nil {
		t.Fatalf("exists = %v %v", ok, err)
	}
	if ok, _ := f.svc.EmailExists(f.ctx, nil, 999); ok {
		t.Fatal("999 must not exist")
	}
	if _, err := f.svc.GetEmail(f.ctx, nil, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}

	update := func(p content.EmailPatch, ifVersion int64) (content.Email, error) {
		var out content.Email
		err := f.tx(func(tx *sql.Tx) error {
			var err error
			out, err = f.svc.UpdateEmail(f.ctx, tx, e.ID, p, ifVersion)
			return err
		})
		return out, err
	}
	f.clock.Advance(time.Hour)
	u, err := update(content.EmailPatch{Subject: ptr("New"), Body: ptr("Changed {{first_name}}")}, 1)
	if err != nil || u.Subject != "New" || u.Body != "Changed {{first_name}}" || u.Name != "Welcome" || u.Version != 2 || !u.UpdatedAt.After(u.CreatedAt) {
		t.Fatalf("update = %+v %v", u, err)
	}
	if _, err := update(content.EmailPatch{Name: ptr("x")}, 1); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale If-Match = %v", err)
	}
	if _, err := update(content.EmailPatch{Body: ptr("{{nope}}")}, 0); fieldOf(err) != "body" {
		t.Fatalf("invalid update = %v", err)
	}
	same, err := update(content.EmailPatch{Subject: ptr("New")}, 2)
	if err != nil || same.Version != 2 {
		t.Fatalf("no-op update = %+v %v", same, err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE target_type = 'email'`); n != 2 {
		t.Fatalf("audit rows = %d, want create + one update", n)
	}
	var changes string
	_ = f.d.Read.QueryRow(`SELECT changes FROM audit_log WHERE operation = 'email.update'`).Scan(&changes)
	if !strings.Contains(changes, `"subject":{"from":"Hi","to":"New"}`) || !strings.Contains(changes, `"body_bytes"`) {
		t.Fatalf("audit changes = %s", changes)
	}

	// List: summaries without bodies, filter, keyset.
	f.createEmail(content.EmailInput{Name: "Second", Subject: "Newsletter May", Body: "x"})
	f.createEmail(content.EmailInput{Name: "Third", Subject: "Other", Body: "xyz"})
	all, err := f.svc.ListEmails(f.ctx, content.ListFilter{Limit: 10})
	if err != nil || len(all) != 3 || all[0].BodyBytes != len("Changed {{first_name}}") || all[2].BodyBytes != 3 {
		t.Fatalf("list = %+v %v", all, err)
	}
	found, _ := f.svc.ListEmails(f.ctx, content.ListFilter{Limit: 10, Query: "NEWS"})
	if len(found) != 1 || found[0].Name != "Second" {
		t.Fatalf("filtered = %+v", found)
	}
	page, _ := f.svc.ListEmails(f.ctx, content.ListFilter{Limit: 10, AfterID: all[1].ID})
	if len(page) != 1 || page[0].ID != all[2].ID {
		t.Fatalf("after = %+v", page)
	}
}

func TestDeleteEmail(t *testing.T) {
	f := newFixture(t)
	del := func(id, ifVersion int64) error {
		return f.tx(func(tx *sql.Tx) error { return f.svc.DeleteEmail(f.ctx, tx, id, ifVersion) })
	}

	inStep := f.createEmail(content.EmailInput{Subject: "step", Body: "x"})
	seq := testutil.InsertSequence(t, f.d, "Welcome")
	testutil.InsertStep(t, f.d, seq, 1, 60, inStep.ID)

	inDraft := f.createEmail(content.EmailInput{Subject: "draft", Body: "x"})
	testutil.InsertBroadcast(t, f.d, "Draft", inDraft.ID)

	inSent := f.createEmail(content.EmailInput{Subject: "sent", Body: "x"})
	b := testutil.InsertBroadcast(t, f.d, "Sent", inSent.ID)
	testutil.Exec(t, f.d, `UPDATE broadcasts SET status = 'sent' WHERE id = ?`, b)

	queued := f.createEmail(content.EmailInput{Subject: "queued", Body: "x"})
	if err := f.tx(func(tx *sql.Tx) error {
		_, _, err := outbox.Enqueue(f.ctx, tx, f.clock, outbox.MessageSpec{Kind: core.KindTest, EmailID: queued.ID, ToEmail: "t@example.com"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	free := f.createEmail(content.EmailInput{Subject: "free", Body: "x"})

	tests := []struct {
		name    string
		id, ver int64
		want    error
		msg     string
	}{
		{"used by step", inStep.ID, 0, core.ErrConflict, "1 sequence step(s)"},
		{"used by draft broadcast", inDraft.ID, 0, core.ErrConflict, "draft broadcast"},
		{"used by sent broadcast", inSent.ID, 0, core.ErrConflict, "sent or scheduled broadcast"},
		{"queued messages", queued.ID, 0, core.ErrConflict, "waiting to be sent"},
		{"stale version", free.ID, 7, core.ErrPrecondition, ""},
		{"missing", 999, 0, core.ErrNotFound, ""},
		{"ok", free.ID, 1, nil, ""},
		{"gone", free.ID, 0, core.ErrNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := del(tt.id, tt.ver)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("delete = %v, want %v", err, tt.want)
			}
			if tt.msg != "" && !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("message %q lacks %q", err, tt.msg)
			}
		})
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'email.delete'`); n != 1 {
		t.Fatalf("delete audit rows = %d", n)
	}
}

// TestEmailUsage: get-email-usage and the list counts name what refers to
// an email (what DeleteEmail refuses over).
func TestEmailUsage(t *testing.T) {
	f := newFixture(t)
	used := f.createEmail(content.EmailInput{Subject: "used", Body: "x"})
	seq := testutil.InsertSequence(t, f.d, "Welcome")
	testutil.InsertStep(t, f.d, seq, 1, 60, used.ID)
	testutil.InsertStep(t, f.d, seq, 2, 60, used.ID)
	b := testutil.InsertBroadcast(t, f.d, "May news", used.ID)
	if err := f.tx(func(tx *sql.Tx) error {
		_, _, err := outbox.Enqueue(f.ctx, tx, f.clock, outbox.MessageSpec{Kind: core.KindTest, EmailID: used.ID, ToEmail: "t@example.com"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	free := f.createEmail(content.EmailInput{Subject: "free", Body: "x"})

	u, err := f.svc.EmailUsage(f.ctx, used.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Steps) != 2 || u.Steps[0].SequenceID != seq || u.Steps[0].SequenceName != "Welcome" || u.Steps[1].Position != 2 ||
		len(u.Broadcasts) != 1 || u.Broadcasts[0].ID != b || u.Broadcasts[0].Name != "May news" || u.Broadcasts[0].Status != "draft" ||
		u.PendingMessages != 1 || !u.InUse() {
		t.Fatalf("usage = %+v", u)
	}
	if u, err := f.svc.EmailUsage(f.ctx, free.ID); err != nil || u.InUse() || u.Steps == nil || u.Broadcasts == nil {
		t.Fatalf("free usage = %+v, %v", u, err)
	}
	if _, err := f.svc.EmailUsage(f.ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}

	list, err := f.svc.ListEmails(f.ctx, content.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64][2]int{}
	for _, e := range list {
		got[e.ID] = [2]int{e.UsedBySteps, e.UsedByBroadcasts}
	}
	if got[used.ID] != [2]int{2, 1} || got[free.ID] != [2]int{0, 0} {
		t.Fatalf("list counts = %v", got)
	}
}

func TestSendTest(t *testing.T) {
	f := newFixture(t)
	em := f.createEmail(content.EmailInput{Subject: "S", Body: "B"})
	c := f.contact("gus@example.com", "Gus", nil)
	send := func(emailID int64, to []string, contactID int64) ([]int64, []string, error) {
		var ids []int64
		var addrs []string
		err := f.tx(func(tx *sql.Tx) error {
			var err error
			ids, addrs, err = f.svc.SendTest(f.ctx, tx, emailID, to, contactID)
			return err
		})
		return ids, addrs, err
	}

	ids, addrs, err := send(em.ID, []string{" A@Example.com ", "b@example.com", "a@example.com"}, c.ID)
	if err != nil || len(ids) != 2 || strings.Join(addrs, ",") != "a@example.com,b@example.com" {
		t.Fatalf("send = %v %v %v", ids, addrs, err)
	}
	var kind, to, data string
	var emailID int64
	var bid sql.NullInt64
	if err := f.d.Read.QueryRow(`SELECT kind, to_email, email_id, data, broadcast_id FROM messages WHERE id = ?`, ids[0]).
		Scan(&kind, &to, &emailID, &data, &bid); err != nil {
		t.Fatal(err)
	}
	if kind != "test" || to != "a@example.com" || emailID != em.ID || bid.Valid || !strings.Contains(data, `"contact_id":`) {
		t.Fatalf("message = %s %s %d %s", kind, to, emailID, data)
	}

	// Default recipient is the owner (S-10).
	_, addrs, err = send(em.ID, nil, 0)
	if err != nil || len(addrs) != 1 || addrs[0] != "owner@example.org" {
		t.Fatalf("owner default = %v %v", addrs, err)
	}

	tests := []struct {
		name      string
		emailID   int64
		to        []string
		contactID int64
		field     string
		want      error
	}{
		{"too many", em.ID, []string{"1@x.io", "2@x.io", "3@x.io", "4@x.io", "5@x.io", "6@x.io"}, 0, "to", nil},
		{"invalid address", em.ID, []string{"ok@x.io", "nope"}, 0, "to[1]", nil},
		{"unknown contact", em.ID, []string{"ok@x.io"}, 999, "contact_id", nil},
		{"unknown email", 999, []string{"ok@x.io"}, 0, "", core.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := send(tt.emailID, tt.to, tt.contactID)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if fieldOf(err) != tt.field {
				t.Fatalf("err = %v, want field %s", err, tt.field)
			}
		})
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM messages`); n != 3 {
		t.Fatalf("messages = %d", n)
	}
}

// TestUpdateEmailOfLockedBroadcast covers B-2 integrity: the email of a
// broadcast past draft and scheduled can't change, others can.
func TestUpdateEmailOfLockedBroadcast(t *testing.T) {
	f := newFixture(t)
	e := f.createEmail(content.EmailInput{Name: "B", Subject: "Hi", Body: "Hello"})
	bID := testutil.InsertBroadcast(t, f.d, "b", e.ID)
	update := func(subject string) error {
		return f.tx(func(tx *sql.Tx) error {
			_, err := f.svc.UpdateEmail(f.ctx, tx, e.ID, content.EmailPatch{Subject: ptr(subject)}, 0)
			return err
		})
	}
	for _, status := range []string{"draft", "scheduled"} {
		testutil.Exec(t, f.d, `UPDATE broadcasts SET status = ? WHERE id = ?`, status, bID)
		if err := update("Hi " + status); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
	}
	for _, status := range []string{"sending", "paused", "sent", "cancelled"} {
		testutil.Exec(t, f.d, `UPDATE broadcasts SET status = ? WHERE id = ?`, status, bID)
		err := update("Changed while " + status)
		if !errors.Is(err, core.ErrConflict) || !strings.Contains(err.Error(), status) {
			t.Fatalf("%s: err = %v", status, err)
		}
		// A patch that changes nothing is still fine.
		if err := update("Hi scheduled"); err != nil {
			t.Fatalf("%s no-op: %v", status, err)
		}
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM emails WHERE id = ? AND subject = 'Hi scheduled'`, e.ID); n != 1 {
		t.Fatal("a locked email changed")
	}
}
