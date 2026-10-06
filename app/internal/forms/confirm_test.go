package forms_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/forms"
	"mailroom/internal/testutil"
)

// pendingSignup submits to a double opt-in form and returns the contact
// and its confirmation path.
func pendingSignup(h *harness, f forms.Form, email string) (core.Contact, string) {
	h.t.Helper()
	h.submit(f.PublicID, vals("email", email, "first_name", "Pat", "attr.company", "Patco"), "RemoteAddr", "203.0.113.20:1").
		wantStatus(h.t, http.StatusSeeOther)
	c := h.contact(email)
	return c, h.confirmPath(c.ID)
}

func TestConfirmGetShowsButtonOnly(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	c, path := pendingSignup(h, f, "pat@example.com")
	for range 3 { // link scanners may open it repeatedly
		r := h.publicReq("GET", path, "", "").wantStatus(t, http.StatusOK)
		page := r.text()
		if !strings.Contains(page, `<form action="`+baseURL+path+`" method="post">`) || !strings.Contains(page, "Confirm subscription") {
			t.Fatalf("confirm page = %s", page)
		}
		if r.header.Get("Referrer-Policy") != "no-referrer" || r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("headers = %v", r.header)
		}
	}
	if got := h.contact("pat@example.com"); got.Status != core.StatusPending || len(got.Tags) != 0 {
		t.Fatalf("GET must not confirm: %+v", got)
	}
	if n := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ? AND confirmed_at IS NULL`, c.ID); n != 1 {
		t.Fatalf("open opt-ins = %d", n)
	}
}

func TestConfirmOptin(t *testing.T) {
	h := newHarness(t)
	f, seq := signupForm(h, `,"double_optin":true`)
	c, path := pendingSignup(h, f, "pat@example.com")
	h.clock.Advance(time.Hour)
	r := h.publicReq("POST", path, "", "", "RemoteAddr", "203.0.113.99:7", "User-Agent", "Clicker/2").wantStatus(t, http.StatusOK)
	if !strings.Contains(r.plain(), "You're subscribed") {
		t.Fatalf("page = %s", r.body)
	}
	got := h.contact("pat@example.com")
	if got.Status != core.StatusActive || got.FirstName != "Pat" || got.Attributes["company"] != "Patco" || strings.Join(got.Tags, ",") != "community" {
		t.Fatalf("contact = %+v", got)
	}
	var source, actor string
	if err := h.d.Read.QueryRow(`SELECT source, actor FROM contact_status_changes WHERE contact_id = ? AND to_status = 'active'`, c.ID).Scan(&source, &actor); err != nil {
		t.Fatal(err)
	}
	if source != "optin" || actor != "public" {
		t.Fatalf("status change = %s %s", source, actor)
	}
	cs := h.consents(c.ID)
	if len(cs) != 2 || cs[0].Type != "optin_requested" || cs[0].IP != "203.0.113.20" {
		t.Fatalf("consents = %+v", cs)
	}
	confirmed := cs[1]
	if confirmed.Type != "optin_confirmed" || confirmed.Source != "form:"+f.PublicID || confirmed.IP != "203.0.113.99" ||
		confirmed.UserAgent != "Clicker/2" || confirmed.ConsentText != f.ConsentText || confirmed.FormID.Int64 != f.ID {
		t.Fatalf("confirmed consent = %+v", confirmed)
	}
	if calls := h.enroller.Calls(); len(calls) != 1 || calls[0] != (enrollCall{seq, c.ID, "form:" + f.PublicID}) {
		t.Fatalf("enroll = %+v", calls)
	}
	if n := h.count(`SELECT count(*) FROM pending_optins WHERE contact_id = ? AND confirmed_at IS NOT NULL`, c.ID); n != 1 {
		t.Fatalf("confirmed opt-ins = %d", n)
	}
	if ev := h.tagEvents(); len(ev) != 1 || ev[0].Source != "form:"+f.PublicID {
		t.Fatalf("tag events = %+v", ev)
	}

	// Used links: a friendly page, nothing happens twice.
	again := h.publicReq("POST", path, "", "").wantStatus(t, http.StatusGone)
	if !strings.Contains(again.plain(), "already been used") {
		t.Fatalf("second POST = %s", again.body)
	}
	h.publicReq("GET", path, "", "").wantStatus(t, http.StatusGone)
	if len(h.enroller.Calls()) != 1 || len(h.consents(c.ID)) != 2 {
		t.Fatal("a used link must not apply again")
	}
}

func TestConfirmAfterFormDeleted(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)
	c, path := pendingSignup(h, f, "pat@example.com")
	if err := h.svc.Delete(t.Context(), f.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	h.publicReq("POST", path, "", "").wantStatus(t, http.StatusOK)
	got := h.contact("pat@example.com")
	if got.Status != core.StatusActive || strings.Join(got.Tags, ",") != "community" {
		t.Fatalf("contact = %+v", got)
	}
	cs := h.consents(c.ID)
	if last := cs[len(cs)-1]; last.Source != "form:"+f.PublicID || last.FormID.Valid {
		t.Fatalf("consent after deletion = %+v", last)
	}
}

func TestConfirmResubscribe(t *testing.T) {
	h := newHarness(t)
	f, seq := signupForm(h, "") // single opt-in: confirmation is still forced (F-7)
	c := testutil.InsertContact(t, h.d, "back@example.com", core.StatusUnsubscribed)
	h.submit(f.PublicID, vals("email", "back@example.com", "first_name", "Bea")).wantStatus(t, http.StatusSeeOther)
	path := h.confirmPath(c.ID)
	page := h.publicReq("GET", path, "", "").wantStatus(t, http.StatusOK).text()
	if !strings.Contains(page, "Yes, send me emails again") {
		t.Fatalf("page = %s", page)
	}
	if got := h.contact("back@example.com"); got.Status != core.StatusUnsubscribed {
		t.Fatalf("GET changed the status to %s", got.Status)
	}
	done := h.publicReq("POST", path, "", "", "RemoteAddr", "203.0.113.5:1").wantStatus(t, http.StatusOK).text()
	if !strings.Contains(done, "Welcome back") {
		t.Fatalf("page = %s", done)
	}
	got := h.contact("back@example.com")
	if got.Status != core.StatusActive || got.FirstName != "Bea" || strings.Join(got.Tags, ",") != "community" {
		t.Fatalf("contact = %+v", got)
	}
	cs := h.consents(c.ID)
	if len(cs) != 1 || cs[0].Type != "resubscribe" || cs[0].IP != "203.0.113.5" || cs[0].Source != "form:"+f.PublicID {
		t.Fatalf("consents = %+v", cs)
	}
	if calls := h.enroller.Calls(); len(calls) != 1 || calls[0] != (enrollCall{seq, c.ID, "form:" + f.PublicID}) {
		t.Fatalf("enroll = %+v", calls)
	}
}

func TestConfirmLinkErrors(t *testing.T) {
	h := newHarness(t)
	f, _ := signupForm(h, `,"double_optin":true`)

	t.Run("invalid", func(t *testing.T) {
		for _, p := range []string{"/f/confirm/garbage", "/f/confirm/" + strings.Repeat("x", 2000)} {
			r := h.publicReq("GET", p, "", "").wantStatus(t, http.StatusNotFound)
			if !strings.Contains(r.plain(), "not valid") {
				t.Fatalf("page = %s", r.body)
			}
			h.publicReq("POST", p, "", "").wantStatus(t, http.StatusNotFound)
		}
	})
	t.Run("tampered", func(t *testing.T) {
		_, path := pendingSignup(h, f, "tamper@example.com")
		h.publicReq("GET", path[:len(path)-2]+"xx", "", "").wantStatus(t, http.StatusNotFound)
	})
	t.Run("expired", func(t *testing.T) {
		c, path := pendingSignup(h, f, "late@example.com")
		h.clock.Advance(forms.OptinTTL + time.Minute)
		r := h.publicReq("GET", path, "", "").wantStatus(t, http.StatusGone)
		if !strings.Contains(r.plain(), "expired") {
			t.Fatalf("page = %s", r.body)
		}
		h.publicReq("POST", path, "", "").wantStatus(t, http.StatusGone)
		if got := h.contact("late@example.com"); got.Status != core.StatusPending || got.ID != c.ID {
			t.Fatalf("expired link changed the contact: %+v", got)
		}
	})
	t.Run("contact bounced meanwhile", func(t *testing.T) {
		c, path := pendingSignup(h, f, "bounce@example.com")
		h.setStatus(c.ID, core.StatusBounced)
		r := h.publicReq("POST", path, "", "").wantStatus(t, http.StatusGone)
		if !strings.Contains(r.plain(), "can't be subscribed") {
			t.Fatalf("page = %s", r.body)
		}
		if got := h.contact("bounce@example.com"); got.Status != core.StatusBounced {
			t.Fatalf("status = %s", got.Status)
		}
	})
	t.Run("address suppressed meanwhile", func(t *testing.T) {
		c, path := pendingSignup(h, f, "supp@example.com")
		testutil.InsertSuppression(t, h.d, "supp@example.com", "manual")
		h.publicReq("POST", path, "", "").wantStatus(t, http.StatusGone)
		if got := h.contact("supp@example.com"); got.Status != core.StatusPending || got.ID != c.ID {
			t.Fatalf("status = %s", got.Status)
		}
	})
}

// TestConfirmTagsReadAtUseTime: a confirmation adds the form's tags as
// they are at the click, following renames and merges that happened
// after the sign-up, and never re-creates a tag that is gone.
func TestConfirmTagsReadAtUseTime(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(h *harness, f forms.Form)
		wantTags string
	}{
		{"unchanged", func(*harness, forms.Form) {}, "community"},
		{"renamed", func(h *harness, _ forms.Form) {
			testutil.Exec(h.t, h.d, `UPDATE tags SET name = 'members' WHERE name = 'community'`)
		}, "members"},
		{"merged", func(h *harness, f forms.Form) { mergeTag(h, "community", "customers") }, "customers"},
		{"form deleted, tag renamed", func(h *harness, f forms.Form) {
			testutil.Exec(h.t, h.d, `DELETE FROM forms WHERE id = ?`, f.ID)
			testutil.Exec(h.t, h.d, `UPDATE tags SET name = 'members' WHERE name = 'community'`)
		}, "members"},
		{"form deleted, tag merged away", func(h *harness, f forms.Form) {
			testutil.Exec(h.t, h.d, `DELETE FROM forms WHERE id = ?`, f.ID)
			mergeTag(h, "community", "customers")
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			f, _ := signupForm(h, `,"double_optin":true`)
			_, path := pendingSignup(h, f, "pat@example.com")
			tc.mutate(h, f)
			h.publicReq("POST", path, "", "").wantStatus(t, http.StatusOK)
			got := h.contact("pat@example.com")
			if got.Status != core.StatusActive || strings.Join(got.Tags, ",") != tc.wantTags {
				t.Fatalf("contact = %+v, want tags %q", got, tc.wantTags)
			}
			if tc.wantTags != "community" {
				if n := h.count(`SELECT count(*) FROM tags WHERE name = 'community'`); n != 0 {
					t.Fatal("a renamed or merged tag must not be re-created")
				}
			}
		})
	}
}

// mergeTag does what contacts' tag merge does to forms: it repoints
// forms.tag_ids from the source tag to the target and deletes the source.
func mergeTag(h *harness, source, target string) {
	h.t.Helper()
	targetID := testutil.InsertTag(h.t, h.d, target)
	var sourceID int64
	if err := h.d.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, source).Scan(&sourceID); err != nil {
		h.t.Fatal(err)
	}
	testutil.Exec(h.t, h.d, `UPDATE forms SET tag_ids = (
			SELECT json_group_array(CASE WHEN value = ?1 THEN ?2 ELSE value END) FROM json_each(forms.tag_ids)),
			version = version + 1
		WHERE EXISTS (SELECT 1 FROM json_each(forms.tag_ids) WHERE value = ?1)`, sourceID, targetID)
	testutil.Exec(h.t, h.d, `DELETE FROM tags WHERE id = ?`, sourceID)
}

// TestConfirmAppliesFormSettingsAtClick: a confirmation applies the tags
// and the sequence the form has at the click, both read at use time,
// and the ones of when they signed up only once the form is deleted.
func TestConfirmAppliesFormSettingsAtClick(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(h *harness, f forms.Form, newSeq int64)
		wantTags string
		wantSeq  func(oldSeq, newSeq int64) int64 // 0 = no enrolment
	}{
		{"unchanged", func(*harness, forms.Form, int64) {}, "community",
			func(oldSeq, _ int64) int64 { return oldSeq }},
		{"tags and sequence changed", func(h *harness, f forms.Form, newSeq int64) {
			tags := []string{"vip"}
			if _, err := h.svc.Update(h.t.Context(), f.ID, forms.FormPatch{Tags: &tags, SequenceID: &newSeq}, 0, false); err != nil {
				h.t.Fatal(err)
			}
		}, "vip", func(_, newSeq int64) int64 { return newSeq }},
		{"sequence removed", func(h *harness, f forms.Form, _ int64) {
			none := int64(0)
			if _, err := h.svc.Update(h.t.Context(), f.ID, forms.FormPatch{SequenceID: &none}, 0, false); err != nil {
				h.t.Fatal(err)
			}
		}, "community", func(int64, int64) int64 { return 0 }},
		{"form changed, then deleted", func(h *harness, f forms.Form, newSeq int64) {
			tags := []string{"vip"}
			if _, err := h.svc.Update(h.t.Context(), f.ID, forms.FormPatch{Tags: &tags, SequenceID: &newSeq}, 0, false); err != nil {
				h.t.Fatal(err)
			}
			if err := h.svc.Delete(h.t.Context(), f.ID, 0, false); err != nil {
				h.t.Fatal(err)
			}
		}, "community", func(oldSeq, _ int64) int64 { return oldSeq }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			f, oldSeq := signupForm(h, `,"double_optin":true`)
			newSeq := testutil.InsertSequence(t, h.d, "Other")
			c, path := pendingSignup(h, f, "pat@example.com")
			tc.mutate(h, f, newSeq)
			h.publicReq("POST", path, "", "").wantStatus(t, http.StatusOK)
			got := h.contact("pat@example.com")
			if got.Status != core.StatusActive || strings.Join(got.Tags, ",") != tc.wantTags {
				t.Fatalf("contact = %+v, want tags %q", got, tc.wantTags)
			}
			calls := h.enroller.Calls()
			want := tc.wantSeq(oldSeq, newSeq)
			switch {
			case want == 0 && len(calls) != 0:
				t.Fatalf("enrolled without a sequence: %+v", calls)
			case want != 0 && (len(calls) != 1 || calls[0] != (enrollCall{want, c.ID, "form:" + f.PublicID})):
				t.Fatalf("enroll = %+v, want sequence %d", calls, want)
			}
		})
	}
}
