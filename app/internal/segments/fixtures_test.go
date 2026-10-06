package segments_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/segments"
	"mailroom/internal/testutil"
)

// day is 24 hours.
const day = 24 * time.Hour

// fixture is a DB with a known set of contacts, tags, attributes and clicks.
type fixture struct {
	db    *db.DB
	clock *testutil.Clock
	svc   *segments.Service
	ids   map[string]int64 // contact local part -> id
	b1    int64            // broadcast with the "offer" click of ann
	b2    int64            // broadcast with the "blog" click of bob
}

type contactSpec struct {
	email, first, last string
	status             core.ContactStatus
	tags               []string
	attrs              string // JSON
	age                time.Duration
}

// fixtureContacts are the contacts of newFixture, created at Epoch - age.
var fixtureContacts = []contactSpec{
	{"ann@example.com", "Ann", "Berg", core.StatusActive, []string{"customer", "VIP"}, `{"plan":"Pro","seats":5,"trial":false,"city":"Copenhagen"}`, 100 * day},
	{"bob@example.org", "Bob", "", core.StatusActive, []string{"customer"}, `{"plan":"basic","seats":1.5,"trial":true}`, 10 * day},
	{"cat@test.io", "", "", core.StatusUnsubscribed, []string{"vip"}, `{"plan":null,"note":"50%_off"}`, 1 * day},
	{"dan@example.com", "Dan", "O'Neil", core.StatusBounced, nil, `{}`, 400 * day},
	{"eve@sub.example.com", "Eve", "Smith", core.StatusPending, []string{"Newsletter"}, `{"plan":"5"}`, 0},
	{"f_y@example.net", "Fay", "", core.StatusActive, nil, `{}`, 3 * day},
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	d := testutil.NewDB(t)
	clock := testutil.NewClock(testutil.Epoch)
	f := &fixture{db: d, clock: clock, svc: segments.New(segments.Deps{DB: d, Clock: clock}), ids: map[string]int64{}}
	for _, c := range fixtureContacts {
		f.ids[local(c.email)] = insertContact(t, d, c)
	}
	emailID := testutil.InsertEmail(t, d, "Hello", "Hi")
	f.b1 = testutil.InsertBroadcast(t, d, "January", emailID)
	f.b2 = testutil.InsertBroadcast(t, d, "February", emailID)
	offer := insertLink(t, d, "https://example.com/offer")
	blog := insertLink(t, d, "https://example.com/blog")
	insertClick(t, d, offer, insertMessage(t, d, f.b1, f.ids["ann"]), f.ids["ann"], testutil.Epoch.Add(-2*day))
	insertClick(t, d, blog, insertMessage(t, d, f.b2, f.ids["bob"]), f.ids["bob"], testutil.Epoch.Add(-40*day))
	insertClick(t, d, offer, insertMessage(t, d, 0, f.ids["cat"]), f.ids["cat"], testutil.Epoch.Add(-5*day))
	return f
}

func local(email string) string {
	for i := range email {
		if email[i] == '@' {
			return email[:i]
		}
	}
	return email
}

func insertContact(t testing.TB, d *db.DB, c contactSpec) int64 {
	t.Helper()
	ts := db.FormatTime(testutil.Epoch.Add(-c.age))
	var id int64
	err := d.Write.QueryRowContext(context.Background(),
		`INSERT INTO contacts (email, first_name, last_name, attributes, status, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		c.email, c.first, c.last, c.attrs, string(c.status), ts, ts, ts).Scan(&id)
	if err != nil {
		t.Fatalf("insert contact %s: %v", c.email, err)
	}
	for _, tag := range c.tags {
		tagID := testutil.InsertTag(t, d, tag)
		testutil.Exec(t, d, `INSERT INTO contact_tags (contact_id, tag_id, added_at) VALUES (?, ?, ?)`, id, tagID, ts)
	}
	return id
}

func insertLink(t testing.TB, d *db.DB, url string) int64 {
	t.Helper()
	var id int64
	if err := d.Write.QueryRow(`INSERT INTO links (url, created_at) VALUES (?, ?) RETURNING id`,
		url, db.FormatTime(testutil.Epoch)).Scan(&id); err != nil {
		t.Fatalf("insert link: %v", err)
	}
	return id
}

// insertMessage inserts a sent message to contactID: a broadcast message
// when broadcastID is set, else a sequence message.
func insertMessage(t testing.TB, d *db.DB, broadcastID, contactID int64) int64 {
	t.Helper()
	kind := core.KindBroadcast
	if broadcastID == 0 {
		kind = core.KindSequence
	}
	ts := db.FormatTime(testutil.Epoch)
	var id int64
	if err := d.Write.QueryRow(`
		INSERT INTO messages (kind, broadcast_id, contact_id, to_email, status, next_attempt_at, queued_at, updated_at)
		VALUES (?, ?, ?, 'x@example.com', 'sent', ?, ?, ?) RETURNING id`,
		string(kind), db.NullID(broadcastID), contactID, ts, ts, ts).Scan(&id); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return id
}

func insertClick(t testing.TB, d *db.DB, linkID, messageID, contactID int64, at time.Time) {
	t.Helper()
	testutil.Exec(t, d, `INSERT INTO clicks (link_id, message_id, contact_id, created_at) VALUES (?, ?, ?, ?)`,
		linkID, messageID, contactID, db.FormatTime(at))
}

// parse parses a filter and fails the test on error.
func parse(t testing.TB, raw string) segments.SegmentFilter {
	t.Helper()
	f, err := segments.ParseFilter([]byte(raw))
	if err != nil {
		t.Fatalf("ParseFilter(%s): %v", raw, err)
	}
	return f
}

// memberNames returns the sorted local parts of the members' emails.
func memberNames(ms []segments.SegmentMember) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, local(m.Email))
	}
	sort.Strings(out)
	return out
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
