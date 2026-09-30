package broadcasts_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"mailroom/internal/broadcasts"
)

// TestStateMachine runs every action against every status through the API
// (B-2): allowed transitions succeed with the expected new status, every
// other combination is 409 conflict and changes nothing.
func TestStateMachine(t *testing.T) {
	statuses := []string{
		broadcasts.StatusDraft, broadcasts.StatusScheduled, broadcasts.StatusSending,
		broadcasts.StatusPaused, broadcasts.StatusSent, broadcasts.StatusCancelled,
	}
	future := time.Now().Add(1000 * time.Hour).UTC().Format(time.RFC3339)
	actions := []struct {
		name    string
		method  string
		path    string // %d = id
		body    string
		allowed []string
		want    string // status afterwards; "" = unchanged, "gone" = deleted
		status  int    // success status
	}{
		{"schedule", "POST", "/broadcasts/%d/schedule", `{"at":"` + future + `"}`, []string{"draft"}, "scheduled", 200},
		{"unschedule", "POST", "/broadcasts/%d/unschedule", "", []string{"scheduled"}, "draft", 200},
		{"send", "POST", "/broadcasts/%d/send", "", []string{"draft", "scheduled"}, "sending", 202},
		{"pause", "POST", "/broadcasts/%d/pause", "", []string{"sending"}, "paused", 200},
		{"resume", "POST", "/broadcasts/%d/resume", "", []string{"paused"}, "sending", 200},
		{"cancel", "POST", "/broadcasts/%d/cancel", "", []string{"draft", "scheduled", "paused"}, "cancelled", 200},
		{"patch", "PATCH", "/broadcasts/%d", `{"name":"Renamed","email":{"subject":"New subject"}}`, []string{"draft", "scheduled"}, "", 200},
		{"delete", "DELETE", "/broadcasts/%d", "", []string{"draft", "cancelled"}, "gone", 204},
	}
	for _, act := range actions {
		for _, from := range statuses {
			allowed := slices.Contains(act.allowed, from)
			t.Run(act.name+"/"+from, func(t *testing.T) {
				hs := newHarness(t)
				b := hs.create("B", "")
				hs.setStatus(b.ID, from, map[bool]string{true: broadcasts.PauseManual}[from == broadcasts.StatusPaused])
				r := hs.do(act.method, fmt.Sprintf(act.path, b.ID), act.body)
				if !allowed {
					r.wantError(t, http.StatusConflict, "conflict", "")
					if got := hs.get(b.ID); got.Status != from || got.Name != "B" {
						t.Fatalf("after refused %s: %+v", act.name, got)
					}
					return
				}
				r.wantStatus(t, act.status)
				switch act.want {
				case "gone":
					hs.do("GET", fmt.Sprintf("/broadcasts/%d", b.ID), "").wantError(t, http.StatusNotFound, "not_found", "")
				case "":
					if got := hs.get(b.ID); got.Status != from || got.Name != "Renamed" || got.Email.Subject != "New subject" {
						t.Fatalf("after patch: %+v", got)
					}
				default:
					if got := hs.get(b.ID); got.Status != act.want {
						t.Fatalf("status = %s, want %s", got.Status, act.want)
					}
				}
			})
		}
	}
}

// TestNoAPISetsSent: nothing in the API moves a broadcast to sent; a
// broadcast in sending stays there whatever is called.
func TestNoAPISetsSent(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("a", 3, "active", "")
	b := hs.create("B", "")
	res := hs.sendAndSnapshot(b.ID)
	if !res.Done || res.RecipientsTotal != 3 {
		t.Fatalf("snapshot = %+v", res)
	}
	if err := hs.svc.Tick(hs.ctx); err != nil {
		t.Fatal(err)
	}
	if got := hs.get(b.ID); got.Status != broadcasts.StatusSending {
		t.Fatalf("status = %s, want sending (only the sender completes broadcasts)", got.Status)
	}
}

func TestPatchBlockedWhileSending(t *testing.T) {
	hs := newHarness(t)
	b := hs.create("B", "")
	hs.send(b.ID)
	e := hs.do("PATCH", fmt.Sprintf("/broadcasts/%d", b.ID), `{"email":{"body":"changed"}}`).wantError(t, http.StatusConflict, "conflict", "")
	if e.Message == "" {
		t.Fatal("empty message")
	}
	if got := hs.get(b.ID); got.Email.Body == "changed" {
		t.Fatal("email changed while sending")
	}
}

func TestScheduleValidation(t *testing.T) {
	hs := newHarness(t)
	b := hs.create("B", "")
	past := hs.clock.Now().Add(-time.Minute).Format(time.RFC3339)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/schedule", b.ID), `{"at":"`+past+`"}`).wantError(t, http.StatusUnprocessableEntity, "validation", "at")
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/schedule", b.ID), `{}`).wantError(t, http.StatusUnprocessableEntity, "validation", "")
	at := hs.clock.Now().Add(2 * time.Hour)
	var got broadcasts.Broadcast
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/schedule", b.ID), `{"at":"`+at.Format(time.RFC3339)+`"}`).wantStatus(t, 200).decode(t, &got)
	if got.Status != broadcasts.StatusScheduled || got.ScheduledAt == nil || !got.ScheduledAt.Equal(at) {
		t.Fatalf("scheduled = %+v", got)
	}
	// Unschedule clears the time.
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/unschedule", b.ID), "").wantStatus(t, 200).decode(t, &got)
	if got.Status != broadcasts.StatusDraft || got.ScheduledAt != nil {
		t.Fatalf("unscheduled = %+v", got)
	}
}

func TestSendNeedsFromAddress(t *testing.T) {
	hs := newHarness(t)
	b := hs.create("B", "")
	hs.clearFrom()
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/send", b.ID), "").wantError(t, http.StatusUnprocessableEntity, "validation", "sender.from_email")
	if got := hs.get(b.ID); got.Status != broadcasts.StatusDraft {
		t.Fatalf("status = %s", got.Status)
	}
}
