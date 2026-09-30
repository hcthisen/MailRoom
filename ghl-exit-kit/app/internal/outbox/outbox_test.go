package outbox_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
	"mailroom/internal/testutil"
)

type env struct {
	d          *db.DB
	clock      *testutil.Clock
	contact    core.Contact
	emailID    int64
	broadcast  int64
	enrollment int64
	step       int64
}

func setup(t *testing.T) env {
	t.Helper()
	d := testutil.NewDB(t)
	c := testutil.InsertContact(t, d, "alice@example.com", core.StatusActive)
	emailID := testutil.InsertEmail(t, d, "Hello", "Hi {{first_name}}")
	seq := testutil.InsertSequence(t, d, "welcome")
	step := testutil.InsertStep(t, d, seq, 1, 0, emailID)
	return env{
		d: d, clock: testutil.NewClock(time.Time{}), contact: c, emailID: emailID,
		broadcast:  testutil.InsertBroadcast(t, d, "news", emailID),
		enrollment: testutil.InsertEnrollment(t, d, seq, c.ID, step),
		step:       step,
	}
}

func TestEnqueueIdempotent(t *testing.T) {
	e := setup(t)
	tests := []struct {
		name string
		spec outbox.MessageSpec
	}{
		{"broadcast", outbox.MessageSpec{Kind: core.KindBroadcast, BroadcastID: e.broadcast, ContactID: e.contact.ID, EmailID: e.emailID, ToEmail: e.contact.Email}},
		{"sequence", outbox.MessageSpec{Kind: core.KindSequence, EnrollmentID: e.enrollment, StepID: e.step, ContactID: e.contact.ID, EmailID: e.emailID, ToEmail: e.contact.Email}},
	}
	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id1, created, err := outbox.Enqueue(ctx, e.d.Write, e.clock, tt.spec)
			if err != nil || !created || id1 == 0 {
				t.Fatalf("first Enqueue = %d, %v, %v", id1, created, err)
			}
			// A retry (even with different data) returns the same row.
			again := tt.spec
			again.Data = map[string]any{"retry": true}
			id2, created, err := outbox.Enqueue(ctx, e.d.Write, e.clock, again)
			if err != nil || created || id2 != id1 {
				t.Fatalf("second Enqueue = %d, %v, %v; want %d, false", id2, created, err, id1)
			}
			// Inside a transaction too.
			err = e.d.WithTx(ctx, func(tx *sql.Tx) error {
				id3, created, err := outbox.Enqueue(ctx, tx, e.clock, tt.spec)
				if err != nil || created || id3 != id1 {
					t.Fatalf("tx Enqueue = %d, %v, %v", id3, created, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	if n := testutil.Count(t, e.d.Read, `SELECT count(*) FROM messages`); n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
}

func TestEnqueueStoresRow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	notBefore := e.clock.Now().Add(time.Hour)
	id, created, err := outbox.Enqueue(ctx, e.d.Write, e.clock, outbox.MessageSpec{
		Kind: core.KindOptin, ContactID: e.contact.ID, SystemTemplate: core.TemplateOptinConfirm,
		ToEmail: " Alice@Example.com ", Data: map[string]any{"confirm_url": "https://x/f/confirm"}, NotBefore: &notBefore,
	})
	if err != nil || !created {
		t.Fatalf("Enqueue: %v", err)
	}
	var kind, status, to, data, next, queued, tmpl string
	var emailID, broadcastID *int64
	err = e.d.Read.QueryRowContext(ctx, `SELECT kind, status, to_email, data, next_attempt_at, queued_at, system_template, email_id, broadcast_id FROM messages WHERE id = ?`, id).
		Scan(&kind, &status, &to, &data, &next, &queued, &tmpl, &emailID, &broadcastID)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "optin" || status != "queued" || to != "alice@example.com" || tmpl != "optin_confirm" {
		t.Fatalf("row = %s %s %s %s", kind, status, to, tmpl)
	}
	if emailID != nil || broadcastID != nil {
		t.Fatal("zero IDs must be stored as NULL")
	}
	if data != `{"confirm_url":"https://x/f/confirm"}` {
		t.Fatalf("data = %s", data)
	}
	if next != db.FormatTime(notBefore) || queued != db.FormatTime(e.clock.Now()) {
		t.Fatalf("next = %s queued = %s", next, queued)
	}

	// Test and alert messages have no unique key, so each call creates a row.
	for i := 0; i < 2; i++ {
		if _, created, err := outbox.Enqueue(ctx, e.d.Write, e.clock, outbox.MessageSpec{Kind: core.KindAlert, SystemTemplate: core.TemplateAlert, ToEmail: "owner@example.com"}); err != nil || !created {
			t.Fatalf("alert %d: %v %v", i, created, err)
		}
	}
}

func TestValidate(t *testing.T) {
	const to = "a@example.com"
	tests := []struct {
		name  string
		spec  outbox.MessageSpec
		field string // "" = valid
	}{
		{"valid broadcast", outbox.MessageSpec{Kind: core.KindBroadcast, BroadcastID: 1, ContactID: 1, EmailID: 1, ToEmail: to}, ""},
		{"valid sequence", outbox.MessageSpec{Kind: core.KindSequence, EnrollmentID: 1, StepID: 1, ContactID: 1, EmailID: 1, ToEmail: to}, ""},
		{"valid optin", outbox.MessageSpec{Kind: core.KindOptin, ContactID: 1, SystemTemplate: core.TemplateResubscribeConfirm, ToEmail: to}, ""},
		{"valid test email", outbox.MessageSpec{Kind: core.KindTest, EmailID: 1, ToEmail: to}, ""},
		{"valid test with contact", outbox.MessageSpec{Kind: core.KindTest, EmailID: 1, ContactID: 1, ToEmail: to}, ""},
		{"valid alert", outbox.MessageSpec{Kind: core.KindAlert, SystemTemplate: core.TemplateAlert, ToEmail: to}, ""},
		{"unknown kind", outbox.MessageSpec{Kind: "sms", EmailID: 1, ToEmail: to}, "kind"},
		{"missing to", outbox.MessageSpec{Kind: core.KindTest, EmailID: 1}, "to_email"},
		{"bad to", outbox.MessageSpec{Kind: core.KindTest, EmailID: 1, ToEmail: "nope"}, "to_email"},
		{"no content", outbox.MessageSpec{Kind: core.KindTest, ToEmail: to}, "email_id"},
		{"both contents", outbox.MessageSpec{Kind: core.KindTest, EmailID: 1, SystemTemplate: core.TemplateAlert, ToEmail: to}, "email_id"},
		{"unknown template", outbox.MessageSpec{Kind: core.KindTest, SystemTemplate: "welcome", ToEmail: to}, "system_template"},
		{"broadcast without broadcast", outbox.MessageSpec{Kind: core.KindBroadcast, ContactID: 1, EmailID: 1, ToEmail: to}, "broadcast_id"},
		{"broadcast without contact", outbox.MessageSpec{Kind: core.KindBroadcast, BroadcastID: 1, EmailID: 1, ToEmail: to}, "contact_id"},
		{"broadcast with template", outbox.MessageSpec{Kind: core.KindBroadcast, BroadcastID: 1, ContactID: 1, SystemTemplate: core.TemplateAlert, ToEmail: to}, "email_id"},
		{"broadcast with step", outbox.MessageSpec{Kind: core.KindBroadcast, BroadcastID: 1, ContactID: 1, EmailID: 1, StepID: 1, ToEmail: to}, "enrollment_id"},
		{"sequence without step", outbox.MessageSpec{Kind: core.KindSequence, EnrollmentID: 1, ContactID: 1, EmailID: 1, ToEmail: to}, "step_id"},
		{"sequence without enrollment", outbox.MessageSpec{Kind: core.KindSequence, StepID: 1, ContactID: 1, EmailID: 1, ToEmail: to}, "enrollment_id"},
		{"sequence with broadcast", outbox.MessageSpec{Kind: core.KindSequence, EnrollmentID: 1, StepID: 1, ContactID: 1, EmailID: 1, BroadcastID: 1, ToEmail: to}, "broadcast_id"},
		{"optin without contact", outbox.MessageSpec{Kind: core.KindOptin, SystemTemplate: core.TemplateOptinConfirm, ToEmail: to}, "contact_id"},
		{"optin with alert template", outbox.MessageSpec{Kind: core.KindOptin, ContactID: 1, SystemTemplate: core.TemplateAlert, ToEmail: to}, "system_template"},
		{"optin with email", outbox.MessageSpec{Kind: core.KindOptin, ContactID: 1, EmailID: 1, ToEmail: to}, "system_template"},
		{"test with broadcast", outbox.MessageSpec{Kind: core.KindTest, EmailID: 1, BroadcastID: 1, ToEmail: to}, "broadcast_id"},
		{"alert with email", outbox.MessageSpec{Kind: core.KindAlert, EmailID: 1, ToEmail: to}, "system_template"},
		{"alert with contact", outbox.MessageSpec{Kind: core.KindAlert, SystemTemplate: core.TemplateAlert, ContactID: 1, ToEmail: to}, "contact_id"},
		{"negative id", outbox.MessageSpec{Kind: core.KindTest, EmailID: -1, ToEmail: to}, "email_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := outbox.Validate(tt.spec)
			if tt.field == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *core.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want ValidationError on %s", err, tt.field)
			}
			if ve.Field != tt.field {
				t.Fatalf("field = %q (%v), want %q", ve.Field, err, tt.field)
			}
		})
	}
}

func TestEnqueueRejectsInvalid(t *testing.T) {
	e := setup(t)
	_, _, err := outbox.Enqueue(context.Background(), e.d.Write, e.clock, outbox.MessageSpec{Kind: core.KindBroadcast, ToEmail: "a@example.com", EmailID: 1})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("err = %v", err)
	}
	if n := testutil.Count(t, e.d.Read, `SELECT count(*) FROM messages`); n != 0 {
		t.Fatal("invalid spec must not insert")
	}
}
