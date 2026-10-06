package events_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/events"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
	"mailroom/internal/tokens"
)

func (h *harness) insertLink(url string) int64 {
	h.t.Helper()
	res, err := h.db.Write.Exec(`INSERT INTO links (url, created_at) VALUES (?, ?)`, url, db.FormatTime(h.clock.Now()))
	if err != nil {
		h.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (h *harness) setTracking(clicks, opens bool) {
	h.t.Helper()
	if _, err := h.settings.Update(context.Background(), settings.Patch{
		Tracking: &settings.TrackingPatch{Clicks: &clicks, Opens: &opens},
	}, 0); err != nil {
		h.t.Fatalf("update settings: %v", err)
	}
}

func TestClickRedirectsAndRecords(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	msg := h.insertMessage("broadcast", c.ID, c.Email, "", 0)
	const target = "https://example.org/offer?utm_source=news&x=%20y"
	link := h.insertLink(target)
	tok := h.token(tokens.PurposeClick, tokens.Payload{ContactID: c.ID, MessageID: msg, LinkID: link})

	rec := h.do(http.MethodGet, "/c/"+tok+"?url=https://evil.example.com", "", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != target {
		t.Fatalf("Location = %q, want %q", loc, target)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("redirect is cacheable")
	}
	if n := h.count(`SELECT COUNT(*) FROM clicks WHERE link_id = ? AND message_id = ? AND contact_id = ?`, link, msg, c.ID); n != 1 {
		t.Fatalf("clicks = %d", n)
	}

	// HEAD (link scanners) redirects without recording.
	if rec := h.do(http.MethodHead, "/c/"+tok, "", ""); rec.Code != http.StatusFound {
		t.Fatalf("HEAD status = %d", rec.Code)
	}
	if n := h.count(`SELECT COUNT(*) FROM clicks`); n != 1 {
		t.Fatalf("HEAD recorded a click (%d)", n)
	}
}

func TestClickMissingRows(t *testing.T) {
	h := newHarness(t)
	link := h.insertLink("https://example.org/")
	// The message and contact no longer exist: the click is kept without them.
	tok := h.token(tokens.PurposeClick, tokens.Payload{ContactID: 999, MessageID: 998, LinkID: link})
	if rec := h.do(http.MethodGet, "/c/"+tok, "", ""); rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := h.count(`SELECT COUNT(*) FROM clicks WHERE link_id = ? AND message_id IS NULL AND contact_id IS NULL`, link); n != 1 {
		t.Fatalf("clicks = %d", n)
	}
}

// eraseReusingID simulates a GDPR erase of the newest contact (erase NULLs
// its messages' contact and address, then deletes the row) followed by a
// new contact with the same ID. contacts.id is AUTOINCREMENT, so SQLite
// never reuses it by itself; the ID is set explicitly to prove the token
// checks don't depend on that (e.g. after a restore of an older backup).
// It returns the new contact.
func (h *harness) eraseReusingID(old core.Contact) core.Contact {
	h.t.Helper()
	testutil.Exec(h.t, h.db, `UPDATE messages SET contact_id = NULL, to_email = NULL WHERE contact_id = ?`, old.ID)
	testutil.Exec(h.t, h.db, `DELETE FROM contacts WHERE id = ?`, old.ID)
	c := testutil.InsertContact(h.t, h.db, "newcomer@example.net", core.StatusActive)
	if c.ID == old.ID {
		h.t.Fatalf("contacts.id %d was reused; it must be AUTOINCREMENT", c.ID)
	}
	testutil.Exec(h.t, h.db, `UPDATE contacts SET id = ? WHERE id = ?`, old.ID, c.ID)
	c.ID = old.ID
	return c
}

func TestStaleTokensAfterEraseTouchNoOne(t *testing.T) {
	h := newHarness(t)
	h.setTracking(true, true)
	old := testutil.InsertContact(t, h.db, "erased@example.net", core.StatusActive)
	msg := h.insertMessage("broadcast", old.ID, old.Email, "", 0)
	link := h.insertLink("https://example.org/")
	unsub := h.unsubURL(old.ID, msg)
	click := h.token(tokens.PurposeClick, tokens.Payload{ContactID: old.ID, MessageID: msg, LinkID: link})
	open := h.token(tokens.PurposeOpen, tokens.Payload{ContactID: old.ID, MessageID: msg})
	c := h.eraseReusingID(old)

	page := h.do(http.MethodGet, unsub, "", "")
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "<form") {
		t.Fatalf("GET: status %d; a stale token must show the result page, not the form", page.Code)
	}
	for _, body := range []string{"List-Unsubscribe=One-Click", "scope=all"} {
		if rec := h.do(http.MethodPost, unsub, formType, body); rec.Code != http.StatusOK {
			t.Fatalf("POST %s: status %d", body, rec.Code)
		}
	}
	if got := h.contactStatus(c.ID); got != core.StatusActive {
		t.Fatalf("the new contact with the reused id was %s by the erased contact's token", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM events`); n != 0 {
		t.Fatalf("events = %d", n)
	}

	if rec := h.do(http.MethodGet, "/c/"+click, "", ""); rec.Code != http.StatusFound {
		t.Fatalf("click status = %d", rec.Code)
	}
	h.do(http.MethodGet, "/o/"+open, "", "")
	if n := h.count(`SELECT COUNT(*) FROM clicks WHERE message_id = ? AND contact_id IS NULL`, msg); n != 1 {
		t.Fatal("click not recorded anonymously")
	}
	if n := h.count(`SELECT COUNT(*) FROM opens WHERE message_id = ? AND contact_id IS NULL`, msg); n != 1 {
		t.Fatal("open not recorded anonymously")
	}
}

func TestClickNotFound(t *testing.T) {
	h := newHarness(t)
	js := h.insertLink("javascript:alert(1)")
	rel := h.insertLink("/relative/path")
	ok := h.insertLink("https://example.org/")
	tests := map[string]string{
		"unknown link":    h.token(tokens.PurposeClick, tokens.Payload{LinkID: 12345, MessageID: 1}),
		"no link id":      h.token(tokens.PurposeClick, tokens.Payload{MessageID: 1}),
		"javascript link": h.token(tokens.PurposeClick, tokens.Payload{LinkID: js}),
		"relative link":   h.token(tokens.PurposeClick, tokens.Payload{LinkID: rel}),
		"open token":      h.token(tokens.PurposeOpen, tokens.Payload{LinkID: ok, MessageID: 1}),
		"garbage":         "abc.def",
	}
	for name, tok := range tests {
		t.Run(name, func(t *testing.T) {
			rec := h.do(http.MethodGet, "/c/"+tok, "", "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if rec.Header().Get("Location") != "" {
				t.Fatal("404 carries a Location")
			}
		})
	}
	if n := h.count(`SELECT COUNT(*) FROM clicks`); n != 0 {
		t.Fatalf("clicks = %d", n)
	}
}

func TestClickRedirectsWhenRecordingFails(t *testing.T) {
	h := newHarness(t)
	link := h.insertLink("https://example.org/a")
	testutil.Exec(t, h.db, `DROP TABLE clicks`)
	tok := h.token(tokens.PurposeClick, tokens.Payload{LinkID: link, MessageID: 1, ContactID: 1})
	rec := h.do(http.MethodGet, "/c/"+tok, "", "")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://example.org/a" {
		t.Fatalf("status = %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestClickRateLimitAndSwitch(t *testing.T) {
	h := newHarness(t, func(d *events.Deps) { d.Limiter = api.NewLimiter(0.0001, 2, d.Clock) })
	link := h.insertLink("https://example.org/")
	tok := h.token(tokens.PurposeClick, tokens.Payload{LinkID: link})
	for range 5 {
		if rec := h.do(http.MethodGet, "/c/"+tok, "", ""); rec.Code != http.StatusFound {
			t.Fatalf("rate-limited click was not redirected: %d", rec.Code)
		}
	}
	if n := h.count(`SELECT COUNT(*) FROM clicks`); n != 2 {
		t.Fatalf("clicks = %d, want 2 (burst)", n)
	}

	h2 := newHarness(t)
	h2.setTracking(false, false)
	link2 := h2.insertLink("https://example.org/")
	if rec := h2.do(http.MethodGet, "/c/"+h2.token(tokens.PurposeClick, tokens.Payload{LinkID: link2}), "", ""); rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := h2.count(`SELECT COUNT(*) FROM clicks`); n != 0 {
		t.Fatalf("clicks recorded with tracking off: %d", n)
	}
}

func TestOpenPixel(t *testing.T) {
	h := newHarness(t)
	c := testutil.InsertContact(t, h.db, "alice@example.net", core.StatusActive)
	msg := h.insertMessage("broadcast", c.ID, c.Email, "", 0)
	tok := h.token(tokens.PurposeOpen, tokens.Payload{ContactID: c.ID, MessageID: msg})
	check := func(t *testing.T, target string) {
		t.Helper()
		rec := h.do(http.MethodGet, target, "", "")
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/gif" {
			t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
		}
		if b := rec.Body.Bytes(); !bytes.HasPrefix(b, []byte("GIF89a")) || len(b) != 43 {
			t.Fatalf("body is not the 1x1 GIF (%d bytes)", len(b))
		}
	}

	// Open tracking is off by default (K-2): the pixel is served, nothing recorded.
	check(t, "/o/"+tok)
	if n := h.count(`SELECT COUNT(*) FROM opens`); n != 0 {
		t.Fatalf("opens with tracking off = %d", n)
	}

	h.setTracking(true, true)
	check(t, "/o/"+tok)
	if n := h.count(`SELECT COUNT(*) FROM opens WHERE message_id = ? AND contact_id = ?`, msg, c.ID); n != 1 {
		t.Fatalf("opens = %d", n)
	}
	check(t, "/o/invalid.token")
	check(t, "/o/"+h.token(tokens.PurposeOpen, tokens.Payload{ContactID: c.ID, MessageID: 77777}))
	if n := h.count(`SELECT COUNT(*) FROM opens`); n != 1 {
		t.Fatalf("invalid tokens or missing messages recorded opens: %d", n)
	}
}
