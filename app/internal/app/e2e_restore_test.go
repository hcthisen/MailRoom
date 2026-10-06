package app_test

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/auth"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/events"
	"mailroom/internal/sending"
	"mailroom/internal/sequences"
	"mailroom/internal/system"
)

// TestE2ERestoreKeepsOptOuts: a backup is taken while a broadcast and a
// drip step are queued. Afterwards the mail goes out, contacts
// unsubscribe, are suppressed or erased, and a new contact opts out. Then
// the server is stopped and the backup restored (disaster recovery, or
// going back to an older binary). The restored server starts with sending
// paused, keeps every opt-out and erasure made after the backup, and
// sends nothing a second time once sending is resumed.
func TestE2ERestoreKeepsOptOuts(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	seq := hs.createSequence(`{"name":"Drip","trigger_tag":"joined"}`, dripStep{"Drip one", 0}, dripStep{"Drip two", 86400})
	ids := map[string]int64{}
	for _, name := range []string{"unsub", "supp", "erase", "stay"} {
		ids[name] = hs.createContact(fmt.Sprintf(`{"email":"%s@example.org"}`, name)).ID
	}
	drip := hs.createContact(`{"email":"drip@example.org","tags":["joined"]}`)
	b1 := hs.createBroadcast("First", "First news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b1.ID); n != 5 {
		t.Fatalf("recipients = %d", n)
	}
	hs.schedule() // queues "Drip one"

	cfg := loadConfig(t, hs.vars)
	backup := filepath.Join(cfg.BackupsDir(), "mailroom-20260105T023000Z.db")
	if err := hs.app.DB.Backup(hs.ctx, backup); err != nil {
		t.Fatal(err)
	}

	// After the backup: everything goes out, and people opt out.
	hs.sendAll()
	hs.clock.Advance(day)
	hs.schedule() // queues "Drip two", completing the enrollment
	hs.sendAll()
	sentBefore := hs.provider.Count()
	if sentBefore != 7 {
		t.Fatalf("sent = %d, want 5 broadcast + 2 drip", sentBefore)
	}
	hs.raw("POST", hs.mailTo("unsub@example.org")[0].unsubscribePath(t), "application/x-www-form-urlencoded",
		"List-Unsubscribe=One-Click").want(t, http.StatusOK)
	hs.api("POST", "/suppressions", `{"email":"supp@example.org","note":"asked by phone"}`).want(t, http.StatusCreated)
	hs.api("DELETE", fmt.Sprintf("/contacts/%d", ids["erase"]), "").want(t, http.StatusOK)
	late := hs.createContact(`{"email":"late@example.org"}`)
	hs.api("POST", fmt.Sprintf("/contacts/%d/status", late.ID), `{"status":"unsubscribed"}`).want(t, http.StatusOK)

	hs.close()
	res, err := system.Restore(hs.ctx, system.RestoreOptions{
		DBPath: cfg.DBPath(), BackupsDir: cfg.BackupsDir(), Clock: hs.clock, Logger: quietLogger(),
	}, backup)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SendingPaused || res.CarriedOver == nil || res.SafetyBackup == "" {
		t.Fatalf("restore = %+v", res)
	}
	// Messages: 4 broadcast and 1 drip queued in the backup (the erased
	// contact's is skipped by the erase), plus "Drip two" queued after it.
	if c := res.CarriedOver; c.ErasedContacts != 1 || c.ContactsAdded != 1 || c.Statuses < 2 || c.Enrollments != 1 || c.Messages != 6 {
		t.Fatalf("carried over = %+v", c)
	}
	hs.open()
	hs.startJobs()

	var st sending.State
	hs.api("GET", "/sending/state", "").want(t, http.StatusOK).decode(t, &st)
	if !st.Paused || st.Source != sending.PauseManual || !strings.HasPrefix(st.Reason, system.RestoredPauseReason) {
		t.Fatalf("sending state after restore = %+v", st)
	}
	for name, want := range map[string]core.ContactStatus{
		"unsub": core.StatusUnsubscribed, "supp": core.StatusSuppressedManual, "stay": core.StatusActive,
	} {
		if got := hs.contactStatus(ids[name]); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	if _, ok := hs.contactByEmail("erase@example.org"); ok {
		t.Error("the erased contact came back with the restore")
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE to_email = 'erase@example.org'`); n != 0 {
		t.Errorf("messages still hold the erased address: %d", n)
	}
	var lateNow contacts.Contact
	hs.api("GET", fmt.Sprintf("/contacts/%d", late.ID), "").want(t, http.StatusOK).decode(t, &lateNow)
	if lateNow.Email != late.Email || lateNow.Status != core.StatusUnsubscribed {
		t.Errorf("contact created after the backup = %+v, want it unsubscribed under its old ID", lateNow)
	}
	if st, _ := hs.enrollment(seq.ID, drip.ID); st != sequences.EnrollmentCompleted {
		t.Errorf("drip enrollment = %s, want completed as before the restore", st)
	}
	// A new contact never gets an ID that links already sent carry.
	fresh := hs.createContact(`{"email":"fresh@example.org"}`)
	if fresh.ID <= late.ID {
		t.Errorf("new contact ID %d reuses an ID from before the restore (late was %d)", fresh.ID, late.ID)
	}

	hs.api("POST", "/sending/resume", "").want(t, http.StatusOK)
	hs.clock.Advance(day)
	hs.schedule()
	hs.sendAll()
	if n := hs.provider.Count(); n != sentBefore {
		t.Fatalf("sent %d more emails after the restore, want none twice", n-sentBefore)
	}
	b2 := hs.createBroadcast("Second", "Second news", `{"type":"all"}`)
	if n := hs.sendBroadcast(b2.ID); n != 3 {
		t.Fatalf("second broadcast recipients = %d, want stay, drip and fresh", n)
	}
}

// TestE2ERestoreKeepsRevocations: a restore never brings back access or
// sequence opt-outs taken away after the backup. A token revoked, a user
// deleted and a password changed after the backup stay so, every UI
// session ends; a per-sequence opt-out on an enrollment that was completed
// in the backup, or created after it, is kept, so re-adding the trigger
// tag doesn't start the sequence again.
func TestE2ERestoreKeepsRevocations(t *testing.T) {
	hs := newHarness(t, nil)
	hs.startJobs()
	leaked, leakedSecret, err := app.CreateToken(hs.ctx, hs.app.DB, auth.TokenInput{Name: "reporting", Scope: api.ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"owner@example.org", "leaver@example.org", "changer@example.org"} {
		hs.api("POST", "/users", fmt.Sprintf(`{"email":%q,"password":"an old password 1"}`, email)).want(t, http.StatusCreated)
	}
	userID := func(email string) int64 {
		t.Helper()
		var id int64
		if err := hs.app.DB.Read.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	leaver, changer := userID("leaver@example.org"), userID("changer@example.org")
	now := hs.clock.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	for i, uid := range []int64{leaver, changer} {
		if _, err := hs.app.DB.Write.Exec(`INSERT INTO sessions (id, user_id, csrf_token, created_at, last_seen_at, expires_at) VALUES (?, ?, 'c', ?, ?, '2099-01-01T00:00:00.000000Z')`,
			fmt.Sprintf("session-%d", i), uid, now, now); err != nil {
			t.Fatal(err)
		}
	}

	seq := hs.createSequence(`{"name":"Drip","trigger_tag":"joined","reentry":"always"}`, dripStep{"Drip one", 0})
	done := hs.createContact(`{"email":"done@example.org","tags":["joined"]}`)
	later := hs.createContact(`{"email":"later@example.org"}`)
	hs.schedule() // queues "Drip one" for done: its enrollment completes

	cfg := loadConfig(t, hs.vars)
	backup := filepath.Join(cfg.BackupsDir(), "mailroom-20260105T023000Z.db")
	if err := hs.app.DB.Backup(hs.ctx, backup); err != nil {
		t.Fatal(err)
	}

	// After the backup: access is taken away, and both contacts stop the drip.
	hs.api("DELETE", fmt.Sprintf("/tokens/%d", leaked.ID), "").want(t, http.StatusNoContent)
	hs.api("DELETE", fmt.Sprintf("/users/%d", leaver), "").want(t, http.StatusNoContent)
	hs.api("PATCH", fmt.Sprintf("/users/%d", changer), `{"password":"a brand new password 2"}`).want(t, http.StatusOK)
	var newHash string
	if err := hs.app.DB.Read.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, changer).Scan(&newHash); err != nil {
		t.Fatal(err)
	}
	hs.api("POST", fmt.Sprintf("/contacts/%d/tags", later.ID), `{"tags":["joined"]}`).want(t, http.StatusOK)
	hs.schedule()
	hs.sendAll()
	for _, c := range []contacts.Contact{done, later} {
		path := hs.mailTo(c.Email)[0].unsubscribePath(t)
		hs.raw("POST", path, "application/x-www-form-urlencoded", "scope="+events.ScopeSequence).want(t, http.StatusOK)
	}

	hs.close()
	res, err := system.Restore(hs.ctx, system.RestoreOptions{
		DBPath: cfg.DBPath(), BackupsDir: cfg.BackupsDir(), Clock: hs.clock, Logger: quietLogger(),
	}, backup)
	if err != nil {
		t.Fatal(err)
	}
	if c := res.CarriedOver; c == nil || c.TokensRevoked != 1 || c.UsersRemoved != 1 || c.UsersUpdated != 1 || c.Enrollments != 2 || res.SessionsEnded != 2 {
		t.Fatalf("restore = %+v, carried over = %+v", res, res.CarriedOver)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "Credentials") {
		t.Fatalf("warnings = %q", res.Warnings)
	}
	hs.open()
	hs.startJobs()

	hs.raw("GET", api.Prefix+"/contacts", "", "", "Authorization", "Bearer "+leakedSecret).want(t, http.StatusUnauthorized)
	if n := hs.count(`SELECT count(*) FROM users WHERE id = ?`, leaver); n != 0 {
		t.Fatal("the deleted user came back")
	}
	if n := hs.count(`SELECT count(*) FROM users WHERE id = ? AND password_hash = ?`, changer, newHash); n != 1 {
		t.Fatal("the old password of the changed user came back")
	}
	if n := hs.count(`SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("sessions after the restore = %d", n)
	}
	for _, c := range []contacts.Contact{done, later} {
		if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ? AND exit_reason = ?`, c.ID, sequences.ExitUnsubscribed); n != 1 {
			t.Fatalf("%s: opted-out enrollments after the restore = %d", c.Email, n)
		}
		hs.api("DELETE", fmt.Sprintf("/contacts/%d/tags/joined", c.ID), "")
		hs.api("POST", fmt.Sprintf("/contacts/%d/tags", c.ID), `{"tags":["joined"]}`).want(t, http.StatusOK)
		if n := hs.count(`SELECT count(*) FROM enrollments WHERE contact_id = ? AND sequence_id = ? AND status = 'active'`, c.ID, seq.ID); n != 0 {
			t.Fatalf("%s was enrolled again after opting out of the sequence", c.Email)
		}
	}
}
