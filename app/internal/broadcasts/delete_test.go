package broadcasts_test

import (
	"fmt"
	"net/http"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// TestDeleteKeepsDeliveryHistory: a cancelled broadcast that sent (or is
// sending) any message can't be deleted, because deleting would cascade
// its messages, events and clicks away. Unsent (queued/skipped) messages
// alone don't block the delete.
func TestDeleteKeepsDeliveryHistory(t *testing.T) {
	tests := []struct {
		name    string
		mark    string // SQL SET clause for the first message; "" = none
		wantDel bool
	}{
		{"only queued and skipped", "", true},
		{"one sent", "status = 'sent', sent_at = queued_at", false},
		{"one delivered", "status = 'delivered', sent_at = queued_at, delivered_at = queued_at", false},
		{"one sending", "status = 'sending'", false},
		{"one failed without sent_at", "status = 'failed'", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.contacts("a", 3, core.StatusActive, "")
			b := hs.create("B", "")
			hs.sendAndSnapshot(b.ID)
			hs.do("POST", fmt.Sprintf("/broadcasts/%d/pause", b.ID), "").wantStatus(t, 200)
			if tc.mark != "" {
				testutil.Exec(t, hs.d, `UPDATE messages SET `+tc.mark+` WHERE id = (SELECT min(id) FROM messages WHERE broadcast_id = ?)`, b.ID)
			}
			hs.do("POST", fmt.Sprintf("/broadcasts/%d/cancel", b.ID), "").wantStatus(t, 200)
			path := fmt.Sprintf("/broadcasts/%d", b.ID)
			if !tc.wantDel {
				hs.do("DELETE", path, "").wantError(t, http.StatusConflict, "conflict", "")
				if got := hs.get(b.ID); got.Status != broadcasts.StatusCancelled {
					t.Fatalf("status = %s", got.Status)
				}
				if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ?`, b.ID); n != 3 {
					t.Fatalf("messages = %d", n)
				}
				return
			}
			hs.do("DELETE", path, "", "If-Match", api.ETag(hs.get(b.ID).Version)).wantStatus(t, http.StatusNoContent)
			if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ?`, b.ID); n != 0 {
				t.Fatalf("messages left = %d", n)
			}
		})
	}
}
