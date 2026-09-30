package imports

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// TestImportTenThousandRows is a performance sanity check: 10 000 rows
// (a tenth of them existing contacts) import through the job runner in
// well under a minute.
func TestImportTenThousandRows(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	hs := newHarness(t)
	for i := range 1000 {
		testutil.InsertContact(t, hs.d, fmt.Sprintf("user%d@example.com", i*10), core.StatusActive)
	}
	const n = 10_000
	csv := csvRows("email,first_name,last_name,status,tags,consent_source,consent_date,attr.plan", n, func(i int) string {
		status := "active"
		if i%7 == 0 {
			status = "unsubscribed"
		}
		return fmt.Sprintf("user%d@example.com,First%d,Last,%s,news;batch%d,ghl,2024-01-01,pro", i, i, status, i%5)
	})
	hs.startJobs()

	dryStart := time.Now()
	dry := hs.dryRun(csv, "")
	dryTook := time.Since(dryStart)

	start := time.Now()
	rep := hs.importNow(csv, "").Report
	took := time.Since(start)
	t.Logf("10k rows: dry run %v, import %v", dryTook, took)

	if rep.Rows != n || rep.Created != n-1000 || rep.Updated != 1000 || rep.RejectedCount != 0 || rep.TagCounts["news"] != n {
		t.Fatalf("report = %+v", rep)
	}
	dry.DryRun = false
	if dry.Created != rep.Created || dry.Updated != rep.Updated || dry.StatusCounts["unsubscribed"] != rep.StatusCounts["unsubscribed"] {
		t.Fatalf("dry run %+v differs from import %+v", dry, rep)
	}
	if got := hs.count(`SELECT count(*) FROM contacts`); got != n {
		t.Fatalf("contacts = %d", got)
	}
	if got := hs.count(`SELECT count(*) FROM consent_events WHERE type = 'import'`); got != n {
		t.Fatalf("consent events = %d", got)
	}
	if took > 60*time.Second || dryTook > 20*time.Second {
		t.Fatalf("too slow: import %v, dry run %v", took, dryTook)
	}
}

// TestDocExample dry-runs the example of docs/import-format.md, so the
// documented file stays valid and its documented report true.
func TestDocExample(t *testing.T) {
	b, err := os.ReadFile("../../docs/import-format.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```csv\n(.*?)```").FindSubmatch(b)
	if m == nil {
		t.Fatal("no csv example in the doc")
	}
	hs := newHarness(t)
	rep := hs.dryRun(string(m[1]), "")
	if rep.Rows != 5 || rep.Created != 5 || rep.RejectedCount != 0 || len(rep.Warnings) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	want := map[string]int{"active": 2, "bounced": 1, "complained": 1, "unsubscribed": 1}
	for k, v := range want {
		if rep.StatusCounts[k] != v {
			t.Fatalf("status counts = %v", rep.StatusCounts)
		}
	}
	if rep.TagCounts["newsletter"] != 4 || rep.TagCounts["customer"] != 1 || rep.TagCounts["webinar"] != 1 {
		t.Fatalf("tag counts = %v", rep.TagCounts)
	}
	// The documented report example matches.
	if !strings.Contains(string(b), `"tag_counts": {"customer": 1, "newsletter": 4, "webinar": 1}`) {
		t.Fatal("the doc's report example is out of date")
	}

	// And it imports with the values the doc shows.
	hs.startJobs()
	hs.importNow(string(m[1]), "")
	erin := hs.contact("erin@example.com")
	if erin.LastName != `O"Brien` || erin.Attributes["plan"] != "free" {
		t.Fatalf("erin = %+v", erin)
	}
	if c := hs.contact("carol@example.com"); c.Attributes["company"] != "Carol & Co, Ltd" || c.Status != core.StatusBounced {
		t.Fatalf("carol = %+v", c)
	}
}
