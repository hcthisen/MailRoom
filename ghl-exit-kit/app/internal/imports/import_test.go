package imports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/hooks"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

// fixtureCSV exercises every rule against the contacts seeded by seed.
const fixtureCSV = "email,first_name,last_name,status,tags,consent_source,consent_date,attr.company,unknown_col\n" +
	"new.active@example.com,New,Active,,newsletter;VIP,ghl-form,2024-03-01,Acme,x\n" + // 2 created active
	"new.unsub@example.com,,,unsubscribed,newsletter,,,,\n" + // 3 created unsubscribed
	"new.complained@example.com,,,complained,,,,,\n" + // 4 created complained
	"was.unsub@example.com,Wally,,active,vip,,,,\n" + // 5 existing unsubscribed: not reactivated, name + tag updated
	"was.active@example.com,,,unsubscribed,,,,,\n" + // 6 existing active -> unsubscribed
	"was.complained@example.com,,,bounced,,,,,\n" + // 7 existing complained: status kept
	"same@example.com,Sam,,active,vip,,,,\n" + // 8 existing, already matching: unchanged
	"erased@example.com,,,,,,,,\n" + // 9 suppressed
	"not-an-email,,,,,,,,\n" + // 10 rejected
	"New.Active@example.com,,,,customer,,,,\n" // 11 merged into line 2

// seed creates the existing contacts fixtureCSV refers to.
func (hs *harness) seed() {
	hs.t.Helper()
	testutil.InsertContact(hs.t, hs.d, "was.unsub@example.com", core.StatusUnsubscribed)
	testutil.InsertContact(hs.t, hs.d, "was.active@example.com", core.StatusActive)
	testutil.InsertContact(hs.t, hs.d, "was.complained@example.com", core.StatusComplained)
	same := testutil.InsertContact(hs.t, hs.d, "same@example.com", core.StatusActive, "vip")
	testutil.Exec(hs.t, hs.d, `UPDATE contacts SET first_name = 'Sam' WHERE id = ?`, same.ID)
	testutil.InsertSuppression(hs.t, hs.d, "erased@example.com", "erased")
}

// wantFixtureReport is the report of fixtureCSV after seed.
func wantFixtureReport(dryRun bool) ImportReport {
	return ImportReport{
		DryRun: dryRun, OnExisting: OnExistingUpdate,
		Rows: 10, Created: 3, Updated: 2, Unchanged: 2, DuplicatesMerged: 1, NotReactivated: 1, StatusKept: 1,
		RejectedCount: 2,
		Rejected: []ImportRejection{
			{Line: 9, Reason: ReasonSuppressed},
			{Line: 10, Reason: "email must contain exactly one @"},
		},
		StatusCounts: map[string]int{"active": 2, "unsubscribed": 3, "complained": 2},
		TagCounts:    map[string]int{"newsletter": 2, "VIP": 3, "customer": 1},
		Warnings:     []string{`unknown column "unknown_col" was ignored`},
	}
}

func TestImportEndToEnd(t *testing.T) {
	hs := newHarness(t)
	hs.seed()
	hs.startJobs()
	start := hs.clock.Now()

	ij := hs.importNow(fixtureCSV, "")
	if ij.Status != jobs.StatusSucceeded || ij.Progress != 1 || ij.OnExisting != OnExistingUpdate || ij.Filename != "contacts.csv" || ij.SizeBytes != int64(len(fixtureCSV)) {
		t.Fatalf("import = %+v", ij)
	}
	if !reflect.DeepEqual(*ij.Report, wantFixtureReport(false)) {
		t.Fatalf("report =\n%+v\nwant\n%+v", *ij.Report, wantFixtureReport(false))
	}
	rep := ij.Report
	if rep.Rows != rep.Created+rep.Updated+rep.Unchanged+rep.Skipped+rep.DuplicatesMerged+rep.RejectedCount {
		t.Fatal("rows must be the sum of the outcomes")
	}

	// New contacts: the CSV status, names, attributes, merged tags.
	na := hs.contact("new.active@example.com")
	if na.Status != core.StatusActive || na.FirstName != "New" || na.Attributes["company"] != "Acme" ||
		strings.Join(na.Tags, ",") != "customer,newsletter,vip" {
		t.Fatalf("new.active = %+v", na)
	}
	if hs.contact("new.unsub@example.com").Status != core.StatusUnsubscribed ||
		hs.contact("new.complained@example.com").Status != core.StatusComplained {
		t.Fatal("non-active statuses must be imported")
	}
	// Consent events for every new contact, source import:<consent_source>.
	var src, occurred string
	if err := hs.d.Read.QueryRow(`SELECT source, occurred_at FROM consent_events WHERE contact_id = ? AND type = 'import'`, na.ID).Scan(&src, &occurred); err != nil {
		t.Fatal(err)
	}
	if src != "import:ghl-form" || occurred != db.FormatTime(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("consent = %s at %s", src, occurred)
	}
	nu := hs.contact("new.unsub@example.com")
	if err := hs.d.Read.QueryRow(`SELECT source, occurred_at FROM consent_events WHERE contact_id = ?`, nu.ID).Scan(&src, &occurred); err != nil {
		t.Fatal(err)
	}
	if src != "import:unspecified" || occurred != db.FormatTime(start) {
		t.Fatalf("unspecified consent = %s at %s", src, occurred)
	}

	// Never reactivate: was.unsub stays unsubscribed but gets name and tag.
	wu := hs.contact("was.unsub@example.com")
	if wu.Status != core.StatusUnsubscribed || wu.FirstName != "Wally" || !slices.Equal(wu.Tags, []string{"vip"}) {
		t.Fatalf("was.unsub = %+v", wu)
	}
	// active -> unsubscribed with a status history row from source import.
	wa := hs.contact("was.active@example.com")
	if wa.Status != core.StatusUnsubscribed {
		t.Fatalf("was.active = %s", wa.Status)
	}
	var from, to, source, actor, note string
	if err := hs.d.Read.QueryRow(`SELECT from_status, to_status, source, actor, note FROM contact_status_changes WHERE contact_id = ?`, wa.ID).
		Scan(&from, &to, &source, &actor, &note); err != nil {
		t.Fatal(err)
	}
	if from != "active" || to != "unsubscribed" || source != "import" || actor != "token:1" || note != statusNote {
		t.Fatalf("status change = %s %s %s %s %q", from, to, source, actor, note)
	}
	if hs.contact("was.complained@example.com").Status != core.StatusComplained {
		t.Fatal("complained must stay complained")
	}
	if n := hs.count(`SELECT count(*) FROM contacts WHERE email = 'erased@example.com'`); n != 0 {
		t.Fatal("a suppressed address must not be created")
	}
	if n := hs.count(`SELECT count(*) FROM contacts`); n != 7 {
		t.Fatalf("contacts = %d", n)
	}

	// Hooks: every tag added with source import.
	for _, e := range hs.tagEvents() {
		if e.Source != contactstore.SourceImport {
			t.Fatalf("tag event source = %q", e.Source)
		}
	}
	if evs := hs.statusEvents(); len(evs) != 1 || evs[0].Source != contactstore.SourceImport || evs[0].To != core.StatusUnsubscribed {
		t.Fatalf("status events = %+v", evs)
	}
	for _, e := range hs.created {
		if e.Source != contactstore.SourceImport {
			t.Fatalf("created source = %q", e.Source)
		}
	}

	// The upload is gone, and the submit was audited.
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("uploads left: %v", files)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'import.create' AND target_type = 'job' AND target_id = ? AND actor_name = 'setup-agent'`, ij.JobID); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	// Job params never hold the CSV or an address.
	var params string
	hs.d.Read.QueryRow(`SELECT params FROM jobs WHERE id = ?`, ij.JobID).Scan(&params)
	if strings.Contains(params, "@") {
		t.Fatalf("params = %s", params)
	}
}

func TestImportIsIdempotent(t *testing.T) {
	hs := newHarness(t)
	hs.seed()
	hs.startJobs()
	hs.importNow(fixtureCSV, "")
	consents := hs.count(`SELECT count(*) FROM consent_events`)
	tagRows := hs.count(`SELECT count(*) FROM contact_tags`)
	statusRows := hs.count(`SELECT count(*) FROM contact_status_changes`)

	again := hs.importNow(fixtureCSV, "").Report
	if again.Created != 0 || again.Updated != 0 || again.Unchanged != 7 || again.RejectedCount != 2 {
		t.Fatalf("second run = %+v", again)
	}
	if hs.count(`SELECT count(*) FROM consent_events`) != consents || hs.count(`SELECT count(*) FROM contact_tags`) != tagRows ||
		hs.count(`SELECT count(*) FROM contact_status_changes`) != statusRows {
		t.Fatal("a repeated import must not add consent events, tags or status changes")
	}
}

func TestImportSkipExisting(t *testing.T) {
	hs := newHarness(t)
	hs.seed()
	hs.startJobs()
	rep := hs.importNow(fixtureCSV, OnExistingSkip).Report
	// was.active still becomes unsubscribed; the others are skipped.
	if rep.Created != 3 || rep.Updated != 1 || rep.Skipped != 3 || rep.Unchanged != 0 || rep.NotReactivated != 1 || rep.StatusKept != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if hs.contact("was.active@example.com").Status != core.StatusUnsubscribed {
		t.Fatal("a blocking status must be applied even with skip")
	}
	wu := hs.contact("was.unsub@example.com")
	if wu.FirstName != "" || len(wu.Tags) != 0 {
		t.Fatalf("skip changed the contact: %+v", wu)
	}
}

func TestImportConsentForExistingContacts(t *testing.T) {
	hs := newHarness(t)
	c := testutil.InsertContact(hs.t, hs.d, "a@example.com", core.StatusActive)
	hs.startJobs()
	csv := "email,consent_source,consent_date\na@example.com,ghl,2024-01-02\n"
	hs.importNow(csv, "")
	hs.importNow(csv, "")
	hs.importNow("email,consent_source\na@example.com,ghl\n", "")
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND source = 'import:ghl'`, c.ID); n != 1 {
		t.Fatalf("consent events = %d, want 1", n)
	}
	hs.importNow("email,consent_source\na@example.com,other\n", "")
	// A row without consent columns is logged as import:unspecified, once.
	hs.importNow("email\na@example.com\n", "")
	hs.importNow("email\na@example.com\n", "")
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ?`, c.ID); n != 3 {
		t.Fatalf("consent events = %d, want 3", n)
	}
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ? AND source = 'import:unspecified'`, c.ID); n != 1 {
		t.Fatalf("unspecified consent events = %d, want 1", n)
	}
}

func TestImportConsentForExistingContactsInSkipMode(t *testing.T) {
	hs := newHarness(t)
	c := testutil.InsertContact(hs.t, hs.d, "a@example.com", core.StatusActive)
	hs.startJobs()
	csv := "email,first_name,consent_source,consent_date\na@example.com,Ann,ghl,2024-01-02\n"
	rep := hs.importNow(csv, OnExistingSkip).Report
	hs.importNow(csv, OnExistingSkip)
	if rep.Skipped != 1 || hs.contact("a@example.com").FirstName != "" {
		t.Fatalf("skip must leave the contact alone: %+v", rep)
	}
	var occurred string
	if err := hs.d.Read.QueryRow(`SELECT occurred_at FROM consent_events WHERE contact_id = ? AND type = 'import' AND source = 'import:ghl'`, c.ID).Scan(&occurred); err != nil {
		t.Fatalf("skip mode must still log the row's consent (I-2): %v", err)
	}
	if occurred != db.FormatTime(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("occurred_at = %s", occurred)
	}
	if n := hs.count(`SELECT count(*) FROM consent_events WHERE contact_id = ?`, c.ID); n != 1 {
		t.Fatalf("consent events = %d, want 1", n)
	}
}

// TestImportNeverDropsBlockingStatus: a row whose other fields are
// invalid, or whose other writes fail, still blocks its address (G-6).
func TestImportNeverDropsBlockingStatus(t *testing.T) {
	hs := newHarness(t)
	hs.store.Bus.OnTagAdded(func(_ context.Context, _ *sql.Tx, e hooks.TagAdded) error {
		if e.TagName == "boom" {
			return errors.New("sequence says no")
		}
		return nil
	})
	for _, e := range []string{"date@example.com", "attrs@example.com", "hook@example.com", "keep@example.com"} {
		testutil.InsertContact(hs.t, hs.d, e, core.StatusActive)
	}
	bigAttrs, _ := json.Marshal(map[string]string{"a": strings.Repeat("v", 9<<10)})
	testutil.Exec(t, hs.d, `UPDATE contacts SET attributes = ? WHERE email = 'attrs@example.com'`, string(bigAttrs))
	big := strings.Repeat("w", 9<<10)
	csv := "email,first_name,status,tags,consent_date,attr.b\n" +
		"date@example.com,,unsubscribed,,2024-03-01 10:00:00,\n" + // 2 existing: bad date
		"newtag@example.com,Nia,unsubscribed," + strings.Repeat("t", 65) + ",,\n" + // 3 new: tag too long
		"attrs@example.com,Al,bounced,,,\n" + // 4 existing: valid on its own
		"attrs@example.com,,complained,,," + big + "\n" + // 5 merged into 4: attributes overflow with the stored ones
		"hook@example.com,Hu,unsubscribed,boom,,\n" + // 6 existing: tag hook fails
		"newhook@example.com,Ned,complained,boom,,\n" + // 7 new: tag hook fails
		"keep@example.com,,active,,bad,\n" + // 8 existing: active row, bad date: rejected, stays active
		"ok@example.com,,,,,\n" // 9 new, fine
	rep := hs.dryRun(csv, "")
	if rep.StatusOnly != 3 || rep.RejectedCount != 4 {
		t.Fatalf("dry run = %+v", rep)
	}

	hs.startJobs()
	rep = *hs.importNow(csv, "").Report
	for email, want := range map[string]core.ContactStatus{
		"date@example.com": core.StatusUnsubscribed, "newtag@example.com": core.StatusUnsubscribed,
		"attrs@example.com": core.StatusComplained, "hook@example.com": core.StatusUnsubscribed,
		"newhook@example.com": core.StatusComplained, "keep@example.com": core.StatusActive, "ok@example.com": core.StatusActive,
	} {
		if c := hs.contact(email); c.Status != want {
			t.Errorf("%s: status %s, want %s", email, c.Status, want)
		}
	}
	// Only the status was written for the status-only rows.
	if c := hs.contact("newtag@example.com"); c.FirstName != "" || len(c.Tags) != 0 {
		t.Errorf("newtag = %+v", c)
	}
	if c := hs.contact("hook@example.com"); c.FirstName != "" || len(c.Tags) != 0 {
		t.Errorf("hook = %+v", c)
	}
	if c := hs.contact("newhook@example.com"); c.FirstName != "" || len(c.Tags) != 0 {
		t.Errorf("newhook = %+v", c)
	}
	if c := hs.contact("attrs@example.com"); c.FirstName != "" || c.Attributes["b"] != nil {
		t.Errorf("attrs = %+v", c)
	}
	// New status-only contacts still get their import consent event (I-2).
	for _, e := range []string{"newtag@example.com", "newhook@example.com"} {
		if n := hs.count(`SELECT count(*) FROM consent_events ce JOIN contacts c ON c.id = ce.contact_id
			WHERE c.email = ? AND ce.type = 'import' AND ce.source = 'import:unspecified'`, e); n != 1 {
			t.Errorf("%s: consent events = %d", e, n)
		}
	}

	if rep.StatusOnly != 5 || rep.RejectedCount != 6 || rep.Created != 1 || rep.Updated != 0 || rep.DuplicatesMerged != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Rows != rep.Created+rep.Updated+rep.Unchanged+rep.Skipped+rep.DuplicatesMerged+rep.RejectedCount {
		t.Fatalf("rows must be the sum of the outcomes: %+v", rep)
	}
	// Status-only contacts count under their applied status.
	if sc := rep.StatusCounts; len(sc) != 3 || sc["active"] != 1 || sc["unsubscribed"] != 3 || sc["complained"] != 2 {
		t.Fatalf("status_counts = %v", sc)
	}
	reasons := map[int]string{}
	for _, r := range rep.Rejected {
		reasons[r.Line] = r.Reason
		if strings.Contains(r.Reason, "example.com") {
			t.Fatalf("reason %q leaks an address", r.Reason)
		}
	}
	for line, st := range map[int]string{2: "unsubscribed", 3: "unsubscribed", 4: "complained", 6: "unsubscribed", 7: "complained"} {
		if !strings.HasSuffix(reasons[line], "only the row's status ("+st+") was imported") {
			t.Errorf("line %d: reason %q", line, reasons[line])
		}
	}
	if r := reasons[8]; !strings.HasPrefix(r, "consent_date") || strings.Contains(r, "only the row's status") {
		t.Errorf("line 8: reason %q", r)
	}
}

// TestImportResumeParsesAsOfSubmitTime: a resumed run reads the file as
// of the submit time, so the checkpoint offset points at the same record.
func TestImportResumeParsesAsOfSubmitTime(t *testing.T) {
	hs := newHarness(t)
	t0 := hs.clock.Now()
	future := t0.Add(25 * time.Hour).Format(time.RFC3339)
	csv := "email,consent_date\nfuture@example.com," + future + "\n" +
		csvRows("email", BatchSize, func(i int) string { return "user" + itoa(i) + "@example.com," })[len("email\n"):]
	j, p := hs.submitRaw(csv)
	if !p.ParsedAt.Equal(t0) {
		t.Fatalf("parsed_at = %v, want %v", p.ParsedAt, t0)
	}

	// The first run committed one batch (the future row was rejected) and
	// was interrupted; the resume runs two hours later.
	testutil.Exec(t, hs.d, `UPDATE jobs SET status = 'running', started_at = ? WHERE id = ?`, db.FormatTime(t0), j.ID)
	cp := checkpoint{Offset: BatchSize, Stats: applyStats{Created: BatchSize, StatusCounts: map[string]int{"active": BatchSize}}}
	if err := hs.d.WithTx(context.Background(), func(tx *sql.Tx) error { return saveCheckpoint(context.Background(), tx, j.ID, cp) }); err != nil {
		t.Fatal(err)
	}
	hs.clock.Set(t0.Add(2 * time.Hour))
	res, err := hs.svc.runImport(actorCtx(), &fakeJob{id: j.ID, params: j.Params, resumed: true})
	if err != nil {
		t.Fatal(err)
	}
	rep := res.(ImportReport)
	if rep.Rows != BatchSize+1 || rep.Created != BatchSize || rep.RejectedCount != 1 || rep.Rejected[0].Line != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if n := hs.count(`SELECT count(*) FROM contacts`); n != 0 {
		t.Fatalf("the resumed run must not write records again: %d contacts", n)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	hs := newHarness(t)
	hs.seed()
	tables := []string{"contacts", "contact_tags", "tags", "consent_events", "contact_status_changes", "jobs", "audit_log", "suppressions"}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = hs.count(`SELECT count(*) FROM ` + tb)
	}
	versions := hs.count(`SELECT sum(version) FROM contacts`)

	rep := hs.dryRun(fixtureCSV, "")
	if !reflect.DeepEqual(rep, wantFixtureReport(true)) {
		t.Fatalf("dry run report =\n%+v\nwant\n%+v", rep, wantFixtureReport(true))
	}
	for _, tb := range tables {
		if n := hs.count(`SELECT count(*) FROM ` + tb); n != before[tb] {
			t.Errorf("%s: %d rows, was %d", tb, n, before[tb])
		}
	}
	if hs.count(`SELECT sum(version) FROM contacts`) != versions {
		t.Error("a dry run must not change contacts")
	}
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("a dry run must not store the file: %v", files)
	}
	if len(hs.tagEvents()) != 0 || len(hs.statusEvents()) != 0 {
		t.Fatal("a dry run must not publish hooks")
	}

	// The real import then reports the same.
	hs.startJobs()
	real := hs.importNow(fixtureCSV, "").Report
	real.DryRun = true
	if !reflect.DeepEqual(*real, rep) {
		t.Fatalf("real report differs from the dry run:\n%+v\n%+v", *real, rep)
	}
}

func TestImportRequestErrors(t *testing.T) {
	hs := newHarness(t)
	ctx := actorCtx()
	tests := []struct {
		name  string
		csv   string
		opts  Options
		field string
	}{
		{"no email column", "name\nAnn\n", Options{}, "file"},
		{"empty file", "", Options{}, "file"},
		{"bad on_existing", "email\na@example.com\n", Options{OnExisting: "replace"}, "on_existing"},
		{"broken header", "email,\"x\n", Options{}, "file"},
	}
	for _, tt := range tests {
		for _, dry := range []bool{false, true} {
			_, err := hs.svc.Import(ctx, strings.NewReader(tt.csv), tt.opts, dry)
			var ve *core.ValidationError
			if !errors.As(err, &ve) || ve.Field != tt.field {
				t.Fatalf("%s (dry %v): err = %v, want validation of %s", tt.name, dry, err, tt.field)
			}
		}
	}
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("refused uploads must be removed: %v", files)
	}
	if n := hs.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestImportFileTooLarge(t *testing.T) {
	hs := newHarness(t)
	_, err := hs.svc.Import(actorCtx(), bigCSV(MaxFileBytes+1), Options{}, false)
	if !errors.Is(err, errFileTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("uploads left: %v", files)
	}
}

// submitRaw stores csv and submits its job without running it.
func (hs *harness) submitRaw(csv string) (jobs.Job, jobParams) {
	hs.t.Helper()
	res, err := hs.svc.Import(actorCtx(), strings.NewReader(csv), Options{}, false)
	if err != nil {
		hs.t.Fatal(err)
	}
	var p jobParams
	if err := json.Unmarshal(res.Job.Params, &p); err != nil {
		hs.t.Fatal(err)
	}
	return *res.Job, p
}

func TestImportResumesFromCheckpoint(t *testing.T) {
	hs := newHarness(t)
	n := BatchSize*2 + 10
	csv := csvRows("email,tags", n, func(i int) string { return "user" + itoa(i) + "@example.com,t" })
	j, p := hs.submitRaw(csv)

	// A previous run wrote the first batch and was interrupted: its
	// checkpoint says BatchSize records are done (counted as created).
	testutil.Exec(t, hs.d, `UPDATE jobs SET status = 'running', started_at = ? WHERE id = ?`, db.FormatTime(hs.clock.Now()), j.ID)
	cp := checkpoint{Offset: BatchSize, Stats: applyStats{Created: BatchSize, StatusCounts: map[string]int{"active": BatchSize}, TagCounts: map[string]int{"t": BatchSize}}}
	if err := hs.d.WithTx(context.Background(), func(tx *sql.Tx) error { return saveCheckpoint(context.Background(), tx, j.ID, cp) }); err != nil {
		t.Fatal(err)
	}

	fj := &fakeJob{id: j.ID, params: j.Params, resumed: true}
	res, err := hs.svc.runImport(actorCtx(), fj)
	if err != nil {
		t.Fatal(err)
	}
	rep := res.(ImportReport)
	if rep.Rows != n || rep.Created != n || rep.StatusCounts["active"] != n || rep.TagCounts["t"] != n || fj.progress != 1 {
		t.Fatalf("report = %+v progress %v", rep, fj.progress)
	}
	// Only the records after the checkpoint were written by this run.
	if got := hs.count(`SELECT count(*) FROM contacts`); got != n-BatchSize {
		t.Fatalf("contacts = %d, want %d", got, n-BatchSize)
	}
	if hs.count(`SELECT count(*) FROM contacts WHERE email = 'user0@example.com'`) != 0 {
		t.Fatal("records before the checkpoint must not be written again")
	}
	if _, err := os.Stat(filepath.Join(hs.svc.Dir(), p.File)); !os.IsNotExist(err) {
		t.Fatalf("file must be removed when the job finishes: %v", err)
	}
}

func TestImportCheckpointIsCommittedWithEachBatch(t *testing.T) {
	hs := newHarness(t)
	n := BatchSize + 5
	j, _ := hs.submitRaw(csvRows("email", n, func(i int) string { return "user" + itoa(i) + "@example.com" }))
	testutil.Exec(t, hs.d, `UPDATE jobs SET status = 'running', started_at = ? WHERE id = ?`, db.FormatTime(hs.clock.Now()), j.ID)

	// Cancel after the first batch: the job stops and keeps its file.
	ctx, cancel := context.WithCancel(actorCtx())
	fj := &progressCancel{fakeJob: fakeJob{id: j.ID, params: j.Params}, cancel: cancel}
	_, err := hs.svc.runImport(ctx, fj)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	cp, err := hs.svc.loadCheckpoint(context.Background(), j.ID)
	if err != nil || cp == nil || cp.Offset != BatchSize || cp.Stats.Created != BatchSize {
		t.Fatalf("checkpoint = %+v, %v", cp, err)
	}
	if hs.count(`SELECT count(*) FROM contacts`) != BatchSize {
		t.Fatal("the first batch must be committed")
	}
	if files := hs.uploads(); len(files) != 1 {
		t.Fatalf("an interrupted import must keep its file: %v", files)
	}

	// The runner re-queues it at start; the resumed run finishes the rest.
	hs.startJobs()
	done := hs.waitJob(j.ID)
	if done.Status != jobs.StatusSucceeded {
		t.Fatalf("job = %+v", done)
	}
	ij, _ := hs.svc.Get(context.Background(), j.ID)
	if ij.Report.Created != n || hs.count(`SELECT count(*) FROM contacts`) != n {
		t.Fatalf("report = %+v", ij.Report)
	}
	if files := hs.uploads(); len(files) != 0 {
		t.Fatalf("uploads left: %v", files)
	}
}

// progressCancel cancels its context at the first progress report.
type progressCancel struct {
	fakeJob
	cancel context.CancelFunc
}

func (p *progressCancel) SetProgress(v float64) { p.fakeJob.SetProgress(v); p.cancel() }

func TestImportMissingFileFailsJob(t *testing.T) {
	hs := newHarness(t)
	j, p := hs.submitRaw("email\na@example.com\n")
	os.Remove(filepath.Join(hs.svc.Dir(), p.File))
	hs.startJobs()
	done := hs.waitJob(j.ID)
	if done.Status != jobs.StatusFailed || !strings.Contains(done.Error, "no longer there") {
		t.Fatalf("job = %+v", done)
	}
	ij, _ := hs.svc.Get(context.Background(), j.ID)
	if ij.Report != nil {
		t.Fatal("a failed job without work has no report")
	}
}

func TestImportRemovesOrphanedUploads(t *testing.T) {
	hs := newHarness(t)
	queued, qp := hs.submitRaw("email\nq@example.com\n")
	if err := os.WriteFile(filepath.Join(hs.svc.Dir(), strings.Repeat("a", 32)+".csv"), []byte("email\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hs.svc.Dir(), "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	hs.clock.Set(time.Now().Add(orphanAge + time.Hour))

	// Run a second import by hand while the first is still queued.
	j, _ := hs.submitRaw("email\nb@example.com\n")
	testutil.Exec(t, hs.d, `UPDATE jobs SET status = 'running' WHERE id = ?`, j.ID)
	if _, err := hs.svc.runImport(actorCtx(), &fakeJob{id: j.ID, params: j.Params}); err != nil {
		t.Fatal(err)
	}
	files := hs.uploads()
	slices.Sort(files)
	if !slices.Equal(files, []string{qp.File, "keep.txt"}) {
		t.Fatalf("files = %v (queued job %d keeps %s)", files, queued.ID, qp.File)
	}
}

func TestImportRecordFailureRollsBackOnlyThatRecord(t *testing.T) {
	hs := newHarness(t)
	hookErr := errors.New("sequence says no")
	hs.store.Bus.OnTagAdded(func(_ context.Context, _ *sql.Tx, e hooks.TagAdded) error {
		if e.TagName == "boom" {
			return hookErr
		}
		return nil
	})
	hs.startJobs()
	rep := hs.importNow("email,tags\na@example.com,ok\nb@example.com,ok;boom\nc@example.com,\n", "").Report
	if rep.Created != 2 || rep.RejectedCount != 1 || rep.Rejected[0].Line != 3 || strings.Contains(rep.Rejected[0].Reason, "example.com") {
		t.Fatalf("report = %+v", rep)
	}
	if hs.count(`SELECT count(*) FROM contacts WHERE email = 'b@example.com'`) != 0 ||
		hs.count(`SELECT count(*) FROM consent_events`) != 2 {
		t.Fatal("the failed record must leave nothing behind")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
