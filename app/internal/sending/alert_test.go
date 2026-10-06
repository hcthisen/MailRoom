package sending

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"mailroom/internal/settings"
)

func TestAlert(t *testing.T) {
	e := newEnv(t)
	alerts := func() int { return e.count(`SELECT count(*) FROM messages WHERE kind = 'alert'`) }

	if err := e.svc.Alert(e.ctx, nil, "backup_failed", "Backup failed", "disk full"); err != nil {
		t.Fatal(err)
	}
	var to, tmpl, data string
	if err := e.db.Read.QueryRow(`SELECT to_email, system_template, data FROM messages WHERE kind = 'alert'`).Scan(&to, &tmpl, &data); err != nil {
		t.Fatal(err)
	}
	if to != "owner@example.org" || tmpl != "alert" || data != `{"body":"disk full","key":"backup_failed","title":"Backup failed"}` {
		t.Fatalf("alert = %s %s %s", to, tmpl, data)
	}

	// Same key within 6 h: de-duplicated. Another key: queued.
	e.clock.Advance(5 * time.Hour)
	if err := e.svc.Alert(e.ctx, nil, "backup_failed", "Backup failed", "again"); err != nil || alerts() != 1 {
		t.Fatalf("dedup: %v, alerts %d", err, alerts())
	}
	if err := e.svc.Alert(e.ctx, nil, "cert_failed", "Certificate", "x"); err != nil || alerts() != 2 {
		t.Fatalf("other key: %v, alerts %d", err, alerts())
	}
	// After the window the key alerts again, to alert_email when set.
	e.clock.Advance(time.Hour + time.Second)
	alertTo := "ops@example.net"
	if _, err := e.settings.Update(e.ctx, settings.Patch{AlertEmail: &alertTo}, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Alert(e.ctx, nil, "backup_failed", "Backup failed", "third"); err != nil || alerts() != 3 {
		t.Fatalf("after window: %v, alerts %d", err, alerts())
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE kind = 'alert' AND to_email = 'ops@example.net'`); n != 1 {
		t.Fatalf("alert_email not used: %d", n)
	}

	// Joining the caller's transaction: a rollback drops the alert.
	errBoom := errors.New("boom")
	err := e.db.WithTx(e.ctx, func(tx *sql.Tx) error {
		if err := e.svc.Alert(e.ctx, tx, "breaker", "Circuit breaker", "paused"); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) || alerts() != 3 {
		t.Fatalf("rollback: %v, alerts %d", err, alerts())
	}

	if err := e.svc.Alert(e.ctx, nil, "", "t", "b"); err == nil {
		t.Fatal("empty key must be refused")
	}
}

func TestAlertWithoutAddress(t *testing.T) {
	e := newEnv(t)
	empty := ""
	if _, err := e.settings.Update(e.ctx, settings.Patch{OwnerEmail: &empty, AlertEmail: &empty}, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Alert(e.ctx, nil, "k", "t", "b"); err != nil {
		t.Fatalf("a missing address must not fail the caller: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("messages = %d", n)
	}
}

func TestAlertIsSentAndRendered(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Alert(e.ctx, nil, "k", "Title", "Body"); err != nil {
		t.Fatal(err)
	}
	e.sendDue()
	if e.provider.Count() != 1 {
		t.Fatal("alert not sent")
	}
	in := e.renderer.last()
	if in.SystemTemplate != "alert" || in.Marketing || in.Contact != nil || in.Data["title"] != "Title" || in.Data["body"] != "Body" {
		t.Fatalf("render input = %+v", in)
	}
	h := parseMessage(t, e.provider.Sent()[0].Raw).Header
	if h.Get("List-Unsubscribe") != "" || h.Get("Feedback-ID") != "alert:alert:mailroom:example.com" {
		t.Fatalf("headers = %v", h)
	}
}
