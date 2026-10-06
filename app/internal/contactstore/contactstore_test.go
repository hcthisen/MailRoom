package contactstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
	"mailroom/internal/testutil"
)

type fixture struct {
	d       *db.DB
	store   *contactstore.Store
	clock   *testutil.Clock
	tags    []hooks.TagAdded
	status  []hooks.StatusChanged
	created []hooks.ContactCreated
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{d: testutil.NewDB(t), clock: testutil.NewClock(time.Time{})}
	bus := hooks.New()
	bus.OnTagAdded(func(_ context.Context, _ *sql.Tx, e hooks.TagAdded) error { f.tags = append(f.tags, e); return nil })
	bus.OnStatusChanged(func(_ context.Context, _ *sql.Tx, e hooks.StatusChanged) error {
		f.status = append(f.status, e)
		return nil
	})
	bus.OnContactCreated(func(_ context.Context, _ *sql.Tx, e hooks.ContactCreated) error {
		f.created = append(f.created, e)
		return nil
	})
	f.store = &contactstore.Store{DB: f.d, Bus: bus, Clock: f.clock}
	return f
}

// tx runs fn in a write transaction and returns its error.
func (f *fixture) tx(t *testing.T, fn func(tx *sql.Tx) error) error {
	t.Helper()
	return f.d.WithTx(context.Background(), fn)
}

func (f *fixture) create(t *testing.T, in contactstore.CreateInput) core.Contact {
	t.Helper()
	var c core.Contact
	if err := f.tx(t, func(tx *sql.Tx) (err error) {
		c, err = f.store.Create(context.Background(), tx, in)
		return err
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return c
}

func TestCreateAndGet(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(t, contactstore.CreateInput{
		Email: " Alice@Example.com ", FirstName: " Alice ", LastName: "Smith",
		Attributes: map[string]any{"company": "Acme", "seats": 3},
		Tags:       []string{"vip", " VIP ", "newsletter"},
		Source:     "api",
	})
	if c.Email != "alice@example.com" || c.FirstName != "Alice" || c.Status != core.StatusActive || c.Version != 1 {
		t.Fatalf("contact = %+v", c)
	}
	if strings.Join(c.Tags, ",") != "newsletter,vip" {
		t.Fatalf("tags = %v", c.Tags)
	}
	if c.Attributes["company"] != "Acme" || c.Attributes["seats"] != float64(3) {
		t.Fatalf("attributes = %v", c.Attributes)
	}
	if !c.CreatedAt.Equal(f.clock.Now()) || !c.StatusChangedAt.Equal(f.clock.Now()) {
		t.Fatalf("times = %v / %v", c.CreatedAt, c.StatusChangedAt)
	}
	if len(f.created) != 1 || f.created[0] != (hooks.ContactCreated{ContactID: c.ID, Source: "api"}) {
		t.Fatalf("ContactCreated events = %v", f.created)
	}
	if len(f.tags) != 2 || f.tags[0].TagName != "vip" || f.tags[0].Source != "api" {
		t.Fatalf("TagAdded events = %v", f.tags)
	}

	byEmail, err := f.store.GetByEmail(ctx, nil, "ALICE@example.com")
	if err != nil || byEmail.ID != c.ID {
		t.Fatalf("GetByEmail = %+v, %v", byEmail, err)
	}
	if _, err := f.store.Get(ctx, nil, 9999); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if _, err := f.store.GetByEmail(ctx, nil, "nobody@example.com"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("GetByEmail missing = %v", err)
	}
	if _, err := f.store.GetByEmail(ctx, nil, "not-an-email"); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("GetByEmail invalid = %v", err)
	}
}

func TestCreateErrors(t *testing.T) {
	f := newFixture(t)
	testutil.InsertContact(t, f.d, "taken@example.com", core.StatusActive)
	testutil.InsertSuppression(t, f.d, "erased@example.com", "erased")
	testutil.InsertSuppression(t, f.d, "manual@example.com", "manual")
	tests := []struct {
		name string
		in   contactstore.CreateInput
		want error
	}{
		{"duplicate", contactstore.CreateInput{Email: "Taken@example.com"}, core.ErrConflict},
		{"erased address", contactstore.CreateInput{Email: "ERASED@example.com"}, core.ErrSuppressed},
		{"manually suppressed", contactstore.CreateInput{Email: "manual@example.com"}, core.ErrSuppressed},
		{"bad email", contactstore.CreateInput{Email: "nope"}, core.ErrValidation},
		{"bad status", contactstore.CreateInput{Email: "b@example.com", Status: "deleted"}, core.ErrValidation},
		{"bad tag", contactstore.CreateInput{Email: "b@example.com", Tags: []string{strings.Repeat("x", 65)}}, core.ErrValidation},
		{"empty tag", contactstore.CreateInput{Email: "b@example.com", Tags: []string{"  "}}, core.ErrValidation},
		{"bad attribute key", contactstore.CreateInput{Email: "b@example.com", Attributes: map[string]any{"a b": 1}}, core.ErrValidation},
		{"long name", contactstore.CreateInput{Email: "b@example.com", FirstName: strings.Repeat("n", 201)}, core.ErrValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := f.tx(t, func(tx *sql.Tx) error {
				_, err := f.store.Create(context.Background(), tx, tt.in)
				return err
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("contacts = %d, want 1", n)
	}
	if len(f.created) != 0 {
		t.Fatal("failed creates must not publish")
	}
}

func TestCreateWithStatus(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, contactstore.CreateInput{Email: "u@example.com", Status: core.StatusUnsubscribed, Source: "import"})
	if c.Status != core.StatusUnsubscribed {
		t.Fatalf("status = %s", c.Status)
	}
}

func TestHookErrorRollsBack(t *testing.T) {
	f := newFixture(t)
	errHook := errors.New("hook failed")
	f.store.Bus.OnTagAdded(func(context.Context, *sql.Tx, hooks.TagAdded) error { return errHook })
	err := f.tx(t, func(tx *sql.Tx) error {
		_, err := f.store.Create(context.Background(), tx, contactstore.CreateInput{Email: "a@example.com", Tags: []string{"x"}})
		return err
	})
	if !errors.Is(err, errHook) {
		t.Fatalf("err = %v", err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM contacts`); n != 0 {
		t.Fatal("hook error must roll back the contact")
	}
}

func TestUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(t, contactstore.CreateInput{Email: "a@example.com", FirstName: "A", Attributes: map[string]any{"keep": "k", "drop": "d"}})
	str := func(s string) *string { return &s }

	update := func(in contactstore.UpdateInput, ifVersion int64) (core.Contact, error) {
		var out core.Contact
		err := f.tx(t, func(tx *sql.Tx) (err error) {
			out, err = f.store.Update(ctx, tx, c.ID, in, ifVersion)
			return err
		})
		return out, err
	}

	f.clock.Advance(time.Minute)
	got, err := update(contactstore.UpdateInput{FirstName: str("Ann"), Attributes: map[string]any{"drop": nil, "new": 1}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.FirstName != "Ann" || got.Version != 2 || got.Attributes["keep"] != "k" || got.Attributes["new"] != float64(1) {
		t.Fatalf("after merge: %+v", got)
	}
	if _, ok := got.Attributes["drop"]; ok {
		t.Fatal("nil attribute must be removed")
	}
	if !got.UpdatedAt.Equal(f.clock.Now()) {
		t.Fatalf("updated_at = %v", got.UpdatedAt)
	}

	// Stale If-Match.
	if _, err := update(contactstore.UpdateInput{LastName: str("B")}, 1); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale version err = %v", err)
	}
	// No-op keeps the version.
	same, err := update(contactstore.UpdateInput{FirstName: str("Ann"), Attributes: map[string]any{"new": 1}}, 0)
	if err != nil || same.Version != 2 {
		t.Fatalf("no-op update: version %d, %v", same.Version, err)
	}
	// Replace attributes.
	repl, err := update(contactstore.UpdateInput{Attributes: map[string]any{"only": true}, ReplaceAttributes: true}, 2)
	if err != nil || len(repl.Attributes) != 1 || repl.Attributes["only"] != true || repl.Version != 3 {
		t.Fatalf("replace: %+v, %v", repl, err)
	}
	if _, err := update(contactstore.UpdateInput{Attributes: map[string]any{"bad-key": 1}}, 0); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("bad key err = %v", err)
	}
	err = f.tx(t, func(tx *sql.Tx) error {
		_, err := f.store.Update(ctx, tx, 9999, contactstore.UpdateInput{}, 0)
		return err
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing contact err = %v", err)
	}
}

func TestTags(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(t, contactstore.CreateInput{Email: "a@example.com", Tags: []string{"Existing"}})
	f.tags = nil

	add := func(names ...string) error {
		return f.tx(t, func(tx *sql.Tx) error { return f.store.AddTags(ctx, tx, c.ID, names, "form:abc") })
	}
	// "existing" matches "Existing" case-insensitively: only "new" is published.
	if err := add("existing", "new", "NEW"); err != nil {
		t.Fatal(err)
	}
	if len(f.tags) != 1 || f.tags[0].TagName != "new" || f.tags[0].Source != "form:abc" || f.tags[0].ContactID != c.ID || f.tags[0].TagID == 0 {
		t.Fatalf("TagAdded = %+v", f.tags)
	}
	got, _ := f.store.Get(ctx, nil, c.ID)
	if strings.Join(got.Tags, ",") != "Existing,new" || got.Version != 2 {
		t.Fatalf("tags = %v version = %d", got.Tags, got.Version)
	}
	// Adding the same tags again publishes nothing and keeps the version.
	if err := add("NEW", "existing"); err != nil {
		t.Fatal(err)
	}
	got, _ = f.store.Get(ctx, nil, c.ID)
	if len(f.tags) != 1 || got.Version != 2 {
		t.Fatalf("re-add published %d events, version %d", len(f.tags), got.Version)
	}
	// The tag row is shared with its original spelling.
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM tags`); n != 2 {
		t.Fatalf("tags rows = %d", n)
	}

	remove := func(names ...string) error {
		return f.tx(t, func(tx *sql.Tx) error { return f.store.RemoveTags(ctx, tx, c.ID, names) })
	}
	if err := remove("EXISTING", "unknown"); err != nil {
		t.Fatal(err)
	}
	got, _ = f.store.Get(ctx, nil, c.ID)
	if strings.Join(got.Tags, ",") != "new" || got.Version != 3 {
		t.Fatalf("after remove: tags = %v version = %d", got.Tags, got.Version)
	}
	if err := remove("unknown"); err != nil {
		t.Fatal(err)
	}
	got, _ = f.store.Get(ctx, nil, c.ID)
	if got.Version != 3 {
		t.Fatal("removing nothing must not bump the version")
	}

	err := f.tx(t, func(tx *sql.Tx) error { return f.store.AddTags(ctx, tx, 9999, []string{"x"}, "api") })
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing contact err = %v", err)
	}
	if err := add(""); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("empty tag err = %v", err)
	}
}

func TestNormalizeTags(t *testing.T) {
	got, err := contactstore.NormalizeTags([]string{" a ", "A", "b", "Ä", "ä"})
	if err != nil {
		t.Fatal(err)
	}
	// SQLite NOCASE folds ASCII only, so Ä and ä are distinct tags.
	if strings.Join(got, ",") != "a,b,Ä,ä" {
		t.Fatalf("NormalizeTags = %v", got)
	}
	if _, err := contactstore.NormalizeTag("bad\ttag"); err == nil {
		t.Fatal("control characters must be rejected")
	}
	if _, err := contactstore.NormalizeTag(strings.Repeat("é", 64)); err != nil {
		t.Fatalf("64 runes must be accepted: %v", err)
	}
}

// transition describes one §6 rule check.
type transition struct {
	from, to core.ContactStatus
	source   string
	note     string
	allowed  bool
}

func transitionTable() []transition {
	const (
		P  = core.StatusPending
		A  = core.StatusActive
		U  = core.StatusUnsubscribed
		B  = core.StatusBounced
		C  = core.StatusComplained
		SM = core.StatusSuppressedManual
	)
	tt := []transition{
		// pending → active: a confirmed opt-in, or admin/api with a note.
		{P, A, "optin", "", true},
		{P, A, "admin", "confirmed by phone", true},
		{P, A, "api", "consent on paper", true},
		{P, A, "admin", "", false},
		{P, A, "api", " ", false},
		{P, A, "form", "", false},
		{P, A, "import", "", false},
		{P, A, "system", "note", false},
		// pending → any other.
		{P, U, "link", "", true},
		{P, B, "bounce", "", true},
		{P, C, "complaint", "", true},
		{P, SM, "admin", "", true},
		// active → unsubscribed / bounced / complained / suppressed_manual.
		{A, U, "one_click", "", true},
		{A, U, "link", "", true},
		{A, B, "bounce", "", true},
		{A, C, "complaint", "", true},
		{A, SM, "api", "", true},
		// unsubscribed → active: only optin, or admin/api with a note.
		{U, A, "optin", "", true},
		{U, A, "admin", "customer asked by phone", true},
		{U, A, "api", "customer asked by email", true},
		{U, A, "admin", "", false},
		{U, A, "admin", "   ", false},
		{U, A, "api", "", false},
		{U, A, "form", "", false},
		{U, A, "import", "", false},
		// bounced → active: only admin/api with a note.
		{B, A, "admin", "mailbox fixed", true},
		{B, A, "api", "mailbox fixed", true},
		{B, A, "admin", "", false},
		{B, A, "api", "", false},
		{B, A, "optin", "", false},
		// complained → active: never.
		{C, A, "admin", "please", false},
		{C, A, "api", "", false},
		{C, A, "optin", "", false},
		// suppressed_manual → active: only admin or api.
		{SM, A, "admin", "", true},
		{SM, A, "api", "", true},
		{SM, A, "optin", "", false},
		{SM, A, "form", "", false},
		// Moves between non-active statuses may only make reactivation harder.
		{U, B, "bounce", "", true},
		{U, C, "complaint", "", true},
		{U, SM, "admin", "", false},
		{SM, B, "bounce", "", true},
		{SM, C, "complaint", "", true},
		{SM, U, "link", "", false},
		{B, C, "complaint", "", true},
		{B, U, "link", "", false},
		{B, SM, "admin", "", false},
		{C, U, "link", "", false},
		{C, B, "bounce", "", false},
		{C, SM, "admin", "", false},
	}
	// anything → pending: never.
	for _, from := range core.ContactStatuses {
		if from != P {
			tt = append(tt, transition{from, P, "admin", "note", false})
		}
	}
	return tt
}

func TestCanTransitionCoversEveryPair(t *testing.T) {
	covered := map[[2]core.ContactStatus]bool{}
	for _, tr := range transitionTable() {
		covered[[2]core.ContactStatus{tr.from, tr.to}] = true
	}
	for _, from := range core.ContactStatuses {
		for _, to := range core.ContactStatuses {
			if from != to && !covered[[2]core.ContactStatus{from, to}] {
				t.Errorf("transition %s → %s has no test case", from, to)
			}
		}
	}
}

func TestSetStatusTransitions(t *testing.T) {
	for _, tr := range transitionTable() {
		name := fmt.Sprintf("%s→%s via %s note=%q", tr.from, tr.to, tr.source, tr.note)
		t.Run(name, func(t *testing.T) {
			if err := contactstore.CanTransition(tr.from, tr.to, tr.source, tr.note); (err == nil) != tr.allowed {
				t.Fatalf("CanTransition = %v, allowed %v", err, tr.allowed)
			}

			f := newFixture(t)
			c := testutil.InsertContact(t, f.d, "s@example.com", tr.from)
			f.clock.Advance(time.Hour)
			var changed bool
			err := f.tx(t, func(tx *sql.Tx) (err error) {
				changed, err = f.store.SetStatus(context.Background(), tx, c.ID, tr.to, contactstore.StatusChange{
					Source: tr.source, Note: tr.note, Actor: "user:1",
				})
				return err
			})
			got, gerr := f.store.Get(context.Background(), nil, c.ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			changes := testutil.Count(t, f.d.Read, `SELECT count(*) FROM contact_status_changes WHERE contact_id = ?`, c.ID)
			if !tr.allowed {
				if !errors.Is(err, core.ErrForbidden) {
					t.Fatalf("err = %v, want ErrForbidden", err)
				}
				if changed || got.Status != tr.from || got.Version != c.Version || changes != 0 || len(f.status) != 0 {
					t.Fatalf("refused transition changed state: changed=%v status=%s version=%d rows=%d events=%d",
						changed, got.Status, got.Version, changes, len(f.status))
				}
				return
			}
			if err != nil || !changed {
				t.Fatalf("SetStatus = %v, %v", changed, err)
			}
			if got.Status != tr.to || got.Version != c.Version+1 || !got.StatusChangedAt.Equal(f.clock.Now()) {
				t.Fatalf("contact = %+v", got)
			}
			if changes != 1 {
				t.Fatalf("status change rows = %d", changes)
			}
			want := hooks.StatusChanged{ContactID: c.ID, From: tr.from, To: tr.to, Source: tr.source}
			if len(f.status) != 1 || f.status[0] != want {
				t.Fatalf("StatusChanged = %+v, want %+v", f.status, want)
			}
		})
	}
}

func TestSetStatusDetails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := testutil.InsertContact(t, f.d, "d@example.com", core.StatusActive)
	emailID := testutil.InsertEmail(t, f.d, "s", "b")
	bID := testutil.InsertBroadcast(t, f.d, "b", emailID)
	var msgID int64
	err := f.d.Write.QueryRowContext(ctx, `INSERT INTO messages (kind, broadcast_id, email_id, contact_id, to_email, status, next_attempt_at, queued_at, updated_at)
		VALUES ('broadcast', ?, ?, ?, ?, 'sent', 'x', 'x', 'x') RETURNING id`, bID, emailID, c.ID, c.Email).Scan(&msgID)
	if err != nil {
		t.Fatal(err)
	}

	set := func(to core.ContactStatus, ch contactstore.StatusChange) (bool, error) {
		var changed bool
		err := f.tx(t, func(tx *sql.Tx) (err error) {
			changed, err = f.store.SetStatus(ctx, tx, c.ID, to, ch)
			return err
		})
		return changed, err
	}
	changed, err := set(core.StatusUnsubscribed, contactstore.StatusChange{Source: contactstore.SourceOneClick, MessageID: msgID, Actor: "public"})
	if err != nil || !changed {
		t.Fatalf("unsubscribe: %v %v", changed, err)
	}
	var from, to, source, actor string
	var mid sql.NullInt64
	if err := f.d.Read.QueryRowContext(ctx, `SELECT from_status, to_status, source, message_id, actor FROM contact_status_changes WHERE contact_id = ?`, c.ID).
		Scan(&from, &to, &source, &mid, &actor); err != nil {
		t.Fatal(err)
	}
	if from != "active" || to != "unsubscribed" || source != "one_click" || mid.Int64 != msgID || actor != "public" {
		t.Fatalf("row = %s %s %s %v %s", from, to, source, mid, actor)
	}
	if f.status[0].MessageID != msgID {
		t.Fatal("event must carry the message id")
	}

	// Already unsubscribed: changed=false, nothing recorded.
	changed, err = set(core.StatusUnsubscribed, contactstore.StatusChange{Source: contactstore.SourceLink})
	if err != nil || changed {
		t.Fatalf("repeat: %v %v", changed, err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM contact_status_changes`); n != 1 || len(f.status) != 1 {
		t.Fatal("no-op must not record or publish")
	}

	if _, err := set("gone", contactstore.StatusChange{Source: "api"}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("bad status err = %v", err)
	}
	if _, err := set(core.StatusActive, contactstore.StatusChange{Source: "carrier-pigeon"}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("bad source err = %v", err)
	}
	err = f.tx(t, func(tx *sql.Tx) error {
		_, err := f.store.SetStatus(ctx, tx, 9999, core.StatusActive, contactstore.StatusChange{Source: "api"})
		return err
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing contact err = %v", err)
	}
}

func TestRecordConsent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := testutil.InsertContact(t, f.d, "c@example.com", core.StatusActive)
	consentDate := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	record := func(e contactstore.ConsentEvent) error {
		return f.tx(t, func(tx *sql.Tx) error { return f.store.RecordConsent(ctx, tx, e) })
	}
	if err := record(contactstore.ConsentEvent{
		ContactID: c.ID, Type: contactstore.ConsentImport, Source: "import:ghl-form",
		ConsentText: "I agree", OccurredAt: consentDate, UserAgent: strings.Repeat("u", 600),
	}); err != nil {
		t.Fatal(err)
	}
	if err := record(contactstore.ConsentEvent{ContactID: c.ID, Type: contactstore.ConsentSignup, Source: "form:ab12", FormID: 3, IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	var occurred, created, ua string
	var formID sql.NullInt64
	if err := f.d.Read.QueryRowContext(ctx, `SELECT occurred_at, created_at, user_agent, form_id FROM consent_events WHERE type = 'import'`).
		Scan(&occurred, &created, &ua, &formID); err != nil {
		t.Fatal(err)
	}
	if occurred != db.FormatTime(consentDate) || created != db.FormatTime(f.clock.Now()) || len(ua) != 512 || formID.Valid {
		t.Fatalf("row = %s %s len(ua)=%d form=%v", occurred, created, len(ua), formID)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM consent_events WHERE contact_id = ? AND type = 'signup' AND form_id = 3 AND occurred_at = created_at`, c.ID); n != 1 {
		t.Fatal("signup consent not recorded with defaults")
	}

	bad := []struct {
		e    contactstore.ConsentEvent
		want error
	}{
		{contactstore.ConsentEvent{ContactID: c.ID, Type: "vibes", Source: "x"}, core.ErrValidation},
		{contactstore.ConsentEvent{ContactID: c.ID, Type: contactstore.ConsentAPI}, core.ErrValidation},
		{contactstore.ConsentEvent{Type: contactstore.ConsentAPI, Source: "api:x"}, core.ErrValidation},
		{contactstore.ConsentEvent{ContactID: 9999, Type: contactstore.ConsentAPI, Source: "api:x"}, core.ErrNotFound},
	}
	for i, b := range bad {
		if err := record(b.e); !errors.Is(err, b.want) {
			t.Errorf("case %d: err = %v, want %v", i, err, b.want)
		}
	}
}

func TestIsSuppressed(t *testing.T) {
	f := newFixture(t)
	testutil.InsertSuppression(t, f.d, "gone@example.com", "erased")
	for email, want := range map[string]bool{"gone@example.com": true, "here@example.com": false} {
		got, err := f.store.IsSuppressed(context.Background(), nil, email)
		if err != nil || got != want {
			t.Errorf("IsSuppressed(%s) = %v, %v", email, got, err)
		}
	}
}

// TestFailedWriteLeavesTxUnchanged: a contactstore call that fails (here a
// hook handler for the second tag) undoes everything it did, so a bulk
// caller that records the failure and continues commits nothing of it.
func TestFailedWriteLeavesTxUnchanged(t *testing.T) {
	errHook := errors.New("hook failed")
	ctx := context.Background()
	tests := []struct {
		name string
		call func(f *fixture, tx *sql.Tx, id int64) error
	}{
		{"create", func(f *fixture, tx *sql.Tx, _ int64) error {
			_, err := f.store.Create(ctx, tx, contactstore.CreateInput{Email: "new@example.com", Tags: []string{"a", "b"}})
			return err
		}},
		{"add tags", func(f *fixture, tx *sql.Tx, id int64) error {
			return f.store.AddTags(ctx, tx, id, []string{"a", "b"}, "api")
		}},
		{"set status", func(f *fixture, tx *sql.Tx, id int64) error {
			_, err := f.store.SetStatus(ctx, tx, id, core.StatusUnsubscribed, contactstore.StatusChange{Source: "link"})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			existing := testutil.InsertContact(t, f.d, "old@example.com", core.StatusActive)
			var sideEffects int
			f.store.Bus.OnTagAdded(func(ctx context.Context, tx *sql.Tx, e hooks.TagAdded) error {
				if e.TagName == "b" {
					return errHook
				}
				sideEffects++
				_, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('hook-'||?, '1', 'x')`, e.TagName)
				return err
			})
			f.store.Bus.OnStatusChanged(func(context.Context, *sql.Tx, hooks.StatusChanged) error { return errHook })
			err := f.tx(t, func(tx *sql.Tx) error {
				if err := tt.call(f, tx, existing.ID); !errors.Is(err, errHook) {
					t.Errorf("call = %v, want the hook error", err)
				}
				// The caller records the failure and carries on.
				_, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('after', '1', 'x')`)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			checks := map[string]int{
				`SELECT count(*) FROM contacts`:                                 1,
				`SELECT count(*) FROM contact_tags`:                             0,
				`SELECT count(*) FROM tags`:                                     0,
				`SELECT count(*) FROM contact_status_changes`:                   0,
				`SELECT count(*) FROM settings WHERE key LIKE 'hook-%'`:         0,
				`SELECT count(*) FROM settings WHERE key = 'after'`:             1,
				`SELECT count(*) FROM contacts WHERE status = 'active'`:         1,
				`SELECT count(*) FROM contacts WHERE version = 1`:               1,
				`SELECT count(*) FROM contacts WHERE email = 'new@example.com'`: 0,
			}
			for q, want := range checks {
				if got := testutil.Count(t, f.d.Read, q); got != want {
					t.Errorf("%s = %d, want %d", q, got, want)
				}
			}
		})
	}
}

// TestManualSuppressionFollowsStatus checks that the suppressions table
// stays in step with suppressed_manual for every caller of the store.
func TestManualSuppressionFollowsStatus(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rows := func(email string) string {
		t.Helper()
		var out []string
		r, err := f.d.Read.QueryContext(ctx, `SELECT reason || ':' || note FROM suppressions WHERE email_hash = ?`, core.EmailHash(email))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}
	set := func(id int64, to core.ContactStatus, ch contactstore.StatusChange) {
		t.Helper()
		if err := f.tx(t, func(tx *sql.Tx) error {
			_, err := f.store.SetStatus(ctx, tx, id, to, ch)
			return err
		}); err != nil {
			t.Fatalf("SetStatus %s: %v", to, err)
		}
	}

	// Created as suppressed_manual: the row comes with the contact.
	c := f.create(t, contactstore.CreateInput{Email: "m@example.com", Status: core.StatusSuppressedManual, Source: "api"})
	if got := rows(c.Email); got != "manual:" {
		t.Fatalf("after create = %q", got)
	}
	// Lifting it removes the row; suppressing again adds it with the note.
	set(c.ID, core.StatusActive, contactstore.StatusChange{Source: contactstore.SourceAdmin})
	if got := rows(c.Email); got != "" {
		t.Fatalf("after lift = %q", got)
	}
	set(c.ID, core.StatusSuppressedManual, contactstore.StatusChange{Source: contactstore.SourceAPI, Note: " asked by phone "})
	if got := rows(c.Email); got != "manual:asked by phone" {
		t.Fatalf("after suppress = %q", got)
	}
	// A bounce recorded over it keeps the row (the address stays blocked).
	set(c.ID, core.StatusBounced, contactstore.StatusChange{Source: contactstore.SourceBounce})
	if got := rows(c.Email); got != "manual:asked by phone" {
		t.Fatalf("after bounce = %q", got)
	}

	// An existing erased row is never replaced or removed.
	e := testutil.InsertContact(t, f.d, "e@example.com", core.StatusActive)
	testutil.InsertSuppression(t, f.d, e.Email, "erased")
	set(e.ID, core.StatusSuppressedManual, contactstore.StatusChange{Source: contactstore.SourceAPI})
	set(e.ID, core.StatusActive, contactstore.StatusChange{Source: contactstore.SourceAPI})
	if got := rows(e.Email); got != "erased:" {
		t.Fatalf("erased row = %q", got)
	}

	// Other status changes don't touch the table.
	u := testutil.InsertContact(t, f.d, "u@example.com", core.StatusActive)
	set(u.ID, core.StatusUnsubscribed, contactstore.StatusChange{Source: contactstore.SourceLink})
	if got := rows(u.Email); got != "" {
		t.Fatalf("unsubscribe added %q", got)
	}
}

// TestOptOutSurvivesReactivation: an unsubscribe request from a bounced or
// manually suppressed contact keeps the status but is remembered, so
// lifting the bounce or the suppression lands on unsubscribed (U-3), and
// the manual suppression row still goes with the lift.
func TestOptOutSurvivesReactivation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	optOut := func(id int64) bool {
		t.Helper()
		var rec bool
		if err := f.tx(t, func(tx *sql.Tx) (err error) {
			rec, err = f.store.RecordOptOut(ctx, tx, id)
			return err
		}); err != nil {
			t.Fatalf("RecordOptOut: %v", err)
		}
		return rec
	}
	activate := func(id int64, source, note string) bool {
		t.Helper()
		var changed bool
		if err := f.tx(t, func(tx *sql.Tx) (err error) {
			changed, err = f.store.SetStatus(ctx, tx, id, core.StatusActive, contactstore.StatusChange{Source: source, Note: note})
			return err
		}); err != nil {
			t.Fatalf("SetStatus active: %v", err)
		}
		return changed
	}
	status := func(id int64) (core.ContactStatus, int64) {
		t.Helper()
		c, err := f.store.Get(ctx, nil, id)
		if err != nil {
			t.Fatal(err)
		}
		return c.Status, c.Version
	}

	// Active and unsubscribed contacts record nothing: their status says it.
	for _, st := range []core.ContactStatus{core.StatusActive, core.StatusPending, core.StatusUnsubscribed} {
		c := testutil.InsertContact(t, f.d, string(st)+"@example.com", st)
		if optOut(c.ID) {
			t.Errorf("%s: opt-out recorded", st)
		}
	}
	if optOut(99999) {
		t.Error("opt-out recorded for a missing contact")
	}

	// Bounced: the status stays, the version moves, a repeat keeps the first.
	b := testutil.InsertContact(t, f.d, "b@example.com", core.StatusBounced)
	_, v0 := status(b.ID)
	if !optOut(b.ID) {
		t.Fatal("bounced: opt-out not recorded")
	}
	if st, v := status(b.ID); st != core.StatusBounced || v != v0+1 {
		t.Fatalf("after opt-out: %s v%d", st, v)
	}
	if optOut(b.ID) {
		t.Fatal("second opt-out recorded again")
	}
	if !activate(b.ID, contactstore.SourceAPI, "mailbox fixed") {
		t.Fatal("reactivation changed nothing")
	}
	if st, _ := status(b.ID); st != core.StatusUnsubscribed {
		t.Fatalf("reactivated bounced contact = %s, want unsubscribed", st)
	}
	var to, note string
	if err := f.d.Read.QueryRow(`SELECT to_status, note FROM contact_status_changes WHERE contact_id = ? ORDER BY id DESC LIMIT 1`, b.ID).
		Scan(&to, &note); err != nil {
		t.Fatal(err)
	}
	if to != "unsubscribed" || !strings.HasPrefix(note, "mailbox fixed (kept unsubscribed") {
		t.Fatalf("status change = %s %q", to, note)
	}
	last := f.status[len(f.status)-1]
	if last.From != core.StatusBounced || last.To != core.StatusUnsubscribed {
		t.Fatalf("hook = %+v", last)
	}
	// The request is used up: a later resubscribe works as for anyone.
	if !activate(b.ID, contactstore.SourceOptin, "") {
		t.Fatal("resubscribe changed nothing")
	}
	if st, _ := status(b.ID); st != core.StatusActive {
		t.Fatalf("after resubscribe = %s", st)
	}

	// Suppressed manually: lifting it removes the manual row and lands on
	// unsubscribed.
	m := f.create(t, contactstore.CreateInput{Email: "m@example.com", Status: core.StatusSuppressedManual, Source: "api"})
	if !optOut(m.ID) {
		t.Fatal("suppressed: opt-out not recorded")
	}
	activate(m.ID, contactstore.SourceAdmin, "")
	if st, _ := status(m.ID); st != core.StatusUnsubscribed {
		t.Fatalf("lifted suppression = %s, want unsubscribed", st)
	}
	var n int
	if err := f.d.Read.QueryRow(`SELECT count(*) FROM suppressions WHERE email_hash = ?`, core.EmailHash(m.Email)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("manual suppression rows = %d, %v", n, err)
	}

	// Unsubscribed, then bounced or complained (a late SES event, an
	// import with status=bounced): the opt-out is kept with the time of
	// the unsubscribe, so lifting the bounce lands on unsubscribed.
	for _, to := range []core.ContactStatus{core.StatusBounced, core.StatusComplained} {
		u := testutil.InsertContact(t, f.d, "late-"+string(to)+"@example.com", core.StatusActive)
		if err := f.tx(t, func(tx *sql.Tx) error {
			if _, err := f.store.SetStatus(ctx, tx, u.ID, core.StatusUnsubscribed, contactstore.StatusChange{Source: contactstore.SourceLink}); err != nil {
				return err
			}
			_, err := f.store.SetStatus(ctx, tx, u.ID, to, contactstore.StatusChange{Source: contactstore.SourceBounce})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var requested, changedAt string
		if err := f.d.Read.QueryRow(`SELECT coalesce(unsubscribe_requested_at, ''), status_changed_at FROM contacts WHERE id = ?`, u.ID).
			Scan(&requested, &changedAt); err != nil {
			t.Fatal(err)
		}
		if requested == "" || requested > changedAt {
			t.Fatalf("%s: unsubscribe_requested_at = %q (status changed %s)", to, requested, changedAt)
		}
		if optOut(u.ID) {
			t.Fatalf("%s: a second request must keep the first", to)
		}
		if to == core.StatusBounced {
			activate(u.ID, contactstore.SourceAPI, "mailbox fixed")
			if st, _ := status(u.ID); st != core.StatusUnsubscribed {
				t.Fatalf("reactivated late bouncer = %s, want unsubscribed", st)
			}
		}
	}

	// Without a request, reactivation goes to active as before.
	plain := testutil.InsertContact(t, f.d, "plain@example.com", core.StatusBounced)
	activate(plain.ID, contactstore.SourceAPI, "fixed")
	if st, _ := status(plain.ID); st != core.StatusActive {
		t.Fatalf("plain reactivation = %s", st)
	}
}
