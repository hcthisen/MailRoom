package broadcasts_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/jobs"
	"mailroom/internal/sending"
	"mailroom/internal/testutil"
)

// TestSnapshotAudience checks who gets a message: active contacts of the
// audience whose address is not suppressed.
func TestSnapshotAudience(t *testing.T) {
	hs := newHarness(t)
	vip := hs.contacts("vip", 4, core.StatusActive, "vip")
	plain := hs.contacts("plain", 3, core.StatusActive, "")
	for _, st := range []core.ContactStatus{core.StatusUnsubscribed, core.StatusPending, core.StatusBounced, core.StatusComplained, core.StatusSuppressedManual} {
		hs.contacts(string(st), 1, st, "vip")
	}
	testutil.InsertSuppression(t, hs.d, "vip0@example.net", "manual")
	testutil.InsertSuppression(t, hs.d, "plain2@example.net", "erased")
	seg := hs.segment("VIPs", "vip")

	tests := []struct {
		name       string
		audience   string
		want       []int64
		suppressed int64
	}{
		{"all", `{"type":"all"}`, []int64{vip[1], vip[2], vip[3], plain[0], plain[1]}, 2},
		{"segment", fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg), []int64{vip[1], vip[2], vip[3]}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := hs.create(tc.name, tc.audience)
			res := hs.sendAndSnapshot(b.ID)
			if !res.Done || res.Queued != int64(len(tc.want)) || res.Suppressed != tc.suppressed || res.RecipientsTotal != int64(len(tc.want)) {
				t.Fatalf("result = %+v", res)
			}
			rows, err := hs.d.Read.Query(`SELECT contact_id, to_email, email_id, kind, status FROM messages WHERE broadcast_id = ? ORDER BY contact_id`, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var id, emailID int64
				var to, kind, status string
				if err := rows.Scan(&id, &to, &emailID, &kind, &status); err != nil {
					t.Fatal(err)
				}
				if emailID != b.Email.ID || kind != "broadcast" || status != "queued" || to == "" {
					t.Fatalf("message = %d %s %d %s %s", id, to, emailID, kind, status)
				}
				got = append(got, id)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("recipients = %v, want %v", got, tc.want)
			}
			after := hs.get(b.ID)
			if after.SnapshotDoneAt == nil || after.RecipientsTotal != int64(len(tc.want)) || after.Status != broadcasts.StatusSending || after.StartedAt == nil {
				t.Fatalf("broadcast = %+v", after)
			}
		})
	}
}

// TestSnapshotLarge queues 5k recipients in batches, and a repeated or
// resumed run never queues anyone twice.
func TestSnapshotLarge(t *testing.T) {
	hs := newHarness(t)
	ids := hs.contacts("c", 5000, core.StatusActive, "")
	hs.contacts("u", 100, core.StatusUnsubscribed, "")
	for i := range 10 {
		testutil.InsertSuppression(t, hs.d, fmt.Sprintf("c%d@example.net", i*100), "manual")
	}
	b := hs.create("Big", "")
	res := hs.sendAndSnapshot(b.ID)
	if !res.Done || res.Queued != 4990 || res.Suppressed != 10 || res.RecipientsTotal != 4990 {
		t.Fatalf("result = %+v", res)
	}
	if n := hs.count(`SELECT count(DISTINCT contact_id) FROM messages WHERE broadcast_id = ?`, b.ID); n != 4990 {
		t.Fatalf("distinct recipients = %d", n)
	}

	// Simulate a crash after the batch holding contact ids[2600]: the
	// later batches were never committed.
	cut := ids[2600]
	lost := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ? AND contact_id > ?`, b.ID, cut)
	testutil.Exec(t, hs.d, `DELETE FROM messages WHERE broadcast_id = ? AND contact_id > ?`, b.ID, cut)
	testutil.Exec(t, hs.d, `UPDATE broadcasts SET snapshot_done_at = NULL, recipients_total = 0 WHERE id = ?`, b.ID)
	j, err := hs.runner.Submit(hs.ctx, nil, broadcasts.JobSnapshot, map[string]any{"broadcast_id": b.ID, "max_contact_id": ids[len(ids)-1]})
	if err != nil {
		t.Fatal(err)
	}
	res2 := decodeSnapshot(t, hs.waitJob(j.ID))
	if !res2.Done || res2.RecipientsTotal != 4990 || res2.Queued != int64(lost) || lost == 0 {
		t.Fatalf("resumed result = %+v", res2)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ?`, b.ID); n != 4990 {
		t.Fatalf("messages after resume = %d", n)
	}
	if n := hs.count(`SELECT count(*) FROM (SELECT contact_id FROM messages WHERE broadcast_id = ? GROUP BY contact_id HAVING count(*) > 1)`, b.ID); n != 0 {
		t.Fatalf("%d duplicated recipients", n)
	}

	// Running it again with the done marker cleared queues nobody twice.
	testutil.Exec(t, hs.d, `UPDATE broadcasts SET snapshot_done_at = NULL WHERE id = ?`, b.ID)
	j, err = hs.runner.Submit(hs.ctx, nil, broadcasts.JobSnapshot, map[string]any{"broadcast_id": b.ID, "max_contact_id": ids[len(ids)-1]})
	if err != nil {
		t.Fatal(err)
	}
	res3 := decodeSnapshot(t, hs.waitJob(j.ID))
	if !res3.Done || res3.Queued != 0 || res3.RecipientsTotal != 4990 {
		t.Fatalf("repeat result = %+v", res3)
	}

	// A job for a finished snapshot does nothing.
	j, err = hs.runner.Submit(hs.ctx, nil, broadcasts.JobSnapshot, map[string]any{"broadcast_id": b.ID, "max_contact_id": ids[len(ids)-1]})
	if err != nil {
		t.Fatal(err)
	}
	if res4 := decodeSnapshot(t, hs.waitJob(j.ID)); !res4.Done || res4.Queued != 0 {
		t.Fatalf("finished result = %+v", res4)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ?`, b.ID); n != 4990 {
		t.Fatalf("messages = %d", n)
	}
}

// TestSnapshotRepeatsIdempotently re-runs a snapshot whose done marker was
// cleared: it continues after the highest queued contact, never duplicating.
func TestSnapshotRepeatsIdempotently(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("c", 700, core.StatusActive, "")
	b := hs.create("B", "")
	hs.sendAndSnapshot(b.ID)
	// The job resumes after the highest queued contact; with the lower
	// half deleted it must still not duplicate the upper half.
	testutil.Exec(t, hs.d, `DELETE FROM messages WHERE broadcast_id = ? AND contact_id <= 350`, b.ID)
	testutil.Exec(t, hs.d, `UPDATE broadcasts SET snapshot_done_at = NULL WHERE id = ?`, b.ID)
	j, err := hs.runner.Submit(hs.ctx, nil, broadcasts.JobSnapshot, map[string]any{"broadcast_id": b.ID, "max_contact_id": 700})
	if err != nil {
		t.Fatal(err)
	}
	res := decodeSnapshot(t, hs.waitJob(j.ID))
	if !res.Done || res.Queued != 0 || res.RecipientsTotal != 350 {
		t.Fatalf("result = %+v", res)
	}
	if n := hs.count(`SELECT count(*) FROM (SELECT contact_id FROM messages WHERE broadcast_id = ? GROUP BY contact_id HAVING count(*) > 1)`, b.ID); n != 0 {
		t.Fatalf("%d duplicated recipients", n)
	}
}

// TestSnapshotBoundaries sets snapshot_done_at with the last batch for
// audiences around the batch size, including an empty one.
func TestSnapshotBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 499, 500, 501, 1000, 1234} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			hs := newHarness(t)
			hs.contacts("c", n, core.StatusActive, "")
			b := hs.create("B", "")
			res := hs.sendAndSnapshot(b.ID)
			got := hs.get(b.ID)
			if !res.Done || res.Queued != int64(n) || got.SnapshotDoneAt == nil || got.RecipientsTotal != int64(n) {
				t.Fatalf("result = %+v, broadcast = %+v", res, got)
			}
		})
	}
}

// TestEmptyAudienceCompletes: a snapshot with no recipients is done at
// once, and the sending package's completion check marks it sent.
func TestEmptyAudienceCompletes(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("u", 3, core.StatusUnsubscribed, "vip")
	seg := hs.segment("VIPs", "vip")
	b := hs.create("B", fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg))
	res := hs.sendAndSnapshot(b.ID)
	if !res.Done || res.RecipientsTotal != 0 {
		t.Fatalf("result = %+v", res)
	}
	snd := sending.New(sending.Deps{DB: hs.d, Clock: hs.clock, Settings: hs.settings})
	done, err := snd.CompleteBroadcasts(hs.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != b.ID {
		t.Fatalf("completed = %v", done)
	}
	if got := hs.get(b.ID); got.Status != broadcasts.StatusSent || got.FinishedAt == nil {
		t.Fatalf("broadcast = %+v", got)
	}
	// sent is final: nothing moves it.
	for _, action := range []string{"send", "pause", "resume", "cancel", "schedule", "unschedule"} {
		body := ""
		if action == "schedule" {
			body = `{"at":"2030-01-01T00:00:00Z"}`
		}
		hs.do("POST", fmt.Sprintf("/broadcasts/%d/%s", b.ID, action), body).wantError(t, http.StatusConflict, "conflict", "")
	}
}

// TestSnapshotStopsWhenCancelled: a snapshot that runs after the broadcast
// was cancelled queues nothing; a paused broadcast is still snapshotted.
func TestSnapshotStopsWhenCancelled(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("c", 10, core.StatusActive, "")
	cancelled := hs.create("Cancelled", "")
	jc := hs.send(cancelled.ID)
	paused := hs.create("Paused", "")
	jp := hs.send(paused.ID)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/pause", cancelled.ID), "").wantStatus(t, 200)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/cancel", cancelled.ID), "").wantStatus(t, 200)
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/pause", paused.ID), "").wantStatus(t, 200)

	if res := decodeSnapshot(t, hs.waitJob(jc.ID)); res.Done || res.Stopped != "broadcast is cancelled" || res.Queued != 0 {
		t.Fatalf("cancelled result = %+v", res)
	}
	if n := hs.count(`SELECT count(*) FROM messages WHERE broadcast_id = ?`, cancelled.ID); n != 0 {
		t.Fatalf("cancelled broadcast got %d messages", n)
	}
	if res := decodeSnapshot(t, hs.waitJob(jp.ID)); !res.Done || res.Queued != 10 {
		t.Fatalf("paused result = %+v", res)
	}
	if got := hs.get(paused.ID); got.Status != broadcasts.StatusPaused || got.SnapshotDoneAt == nil {
		t.Fatalf("paused = %+v", got)
	}
}

// TestSnapshotBoundedToStart: contacts created after the broadcast started
// are not in its audience.
func TestSnapshotBoundedToStart(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("early", 5, core.StatusActive, "")
	b := hs.create("B", "")
	j := hs.send(b.ID)
	hs.contacts("late", 5, core.StatusActive, "")
	if res := decodeSnapshot(t, hs.waitJob(j.ID)); res.RecipientsTotal != 5 {
		t.Fatalf("result = %+v", res)
	}
}

// TestSnapshotFailure: an audience that can't be resolved pauses the
// broadcast (snapshot_failed) with an alert; resuming retries the snapshot.
func TestSnapshotFailure(t *testing.T) {
	hs := newHarness(t)
	seg := hs.segment("S", "vip")
	b := hs.create("B", fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg))
	j := hs.send(b.ID)
	testutil.Exec(t, hs.d, `DELETE FROM segments WHERE id = ?`, seg)
	if got := hs.waitJob(j.ID); got.Status != jobs.StatusFailed {
		t.Fatalf("job = %+v", got)
	}
	got := hs.get(b.ID)
	if got.Status != broadcasts.StatusPaused || got.PausedReason != broadcasts.PauseSnapshotFailed {
		t.Fatalf("broadcast = %+v", got)
	}
	alerts := hs.alerter.all()
	if len(alerts) != 1 || alerts[0].key != fmt.Sprintf("broadcast_snapshot_failed:%d", b.ID) || !alerts[0].inTx {
		t.Fatalf("alerts = %+v", alerts)
	}
	var resumed broadcasts.Broadcast
	hs.do("POST", fmt.Sprintf("/broadcasts/%d/resume", b.ID), "").wantStatus(t, 200).decode(t, &resumed)
	if resumed.Status != broadcasts.StatusSending || resumed.SnapshotJobID == nil || *resumed.SnapshotJobID == j.ID {
		t.Fatalf("resumed = %+v", resumed)
	}
}

func decodeSnapshot(t *testing.T, j jobs.Job) broadcasts.SnapshotResult {
	t.Helper()
	var res broadcasts.SnapshotResult
	if err := json.Unmarshal(j.Result, &res); err != nil {
		t.Fatalf("decode %s: %v", j.Result, err)
	}
	return res
}
