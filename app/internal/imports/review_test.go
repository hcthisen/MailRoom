package imports

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// TestRejectionReasonsNeverRepeatCells: a misplaced column can put an
// address into any cell; no rejection reason or warning may repeat it,
// since reports are kept in the job row, which GDPR erase leaves alone.
func TestRejectionReasonsNeverRepeatCells(t *testing.T) {
	const addr = "b.alt@private.example"
	header := "email,status,tags,consent_date,first_name," + addr
	tests := []struct {
		name string
		row  string
		want string
	}{
		{"address in status", "a@example.com," + addr + ",,,,", "status must be active, unsubscribed, bounced or complained"},
		{"address in consent_date", "a@example.com,,," + addr + ",,", "consent_date must be RFC 3339 (2024-03-01T10:00:00Z) or YYYY-MM-DD"},
		{"address in an overlong tag", "a@example.com,,x" + addr + strings.Repeat("t", 64) + ",,,", ReasonBadTag},
		{"address in a tag with control characters", "a@example.com,,\"" + addr + "\x01\",,,", ReasonBadTag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pf := mustParse(t, header+"\n"+tt.row+"\n")
			if len(pf.rejected) != 1 || pf.rejected[0].Reason != tt.want {
				t.Fatalf("rejected = %+v, want reason %q", pf.rejected, tt.want)
			}
			for _, s := range append([]string{pf.rejected[0].Reason}, pf.warnings...) {
				if strings.Contains(s, "@") {
					t.Fatalf("%q repeats a cell", s)
				}
			}
			if len(pf.warnings) != 1 || pf.warnings[0] != "unknown column 6 was ignored" {
				t.Fatalf("warnings = %q", pf.warnings)
			}
		})
	}

	// The same holds for the stored report of a real import, including a
	// status-only row (reason plus "only the row's status ... was imported").
	hs := newHarness(t)
	hs.startJobs()
	csv := header + "\n" +
		"a@example.com," + addr + ",,,,\n" +
		"c@example.com,unsubscribed,," + addr + ",,\n" // bad date, blocking status
	ij := hs.importNow(csv, "")
	if ij.Report.RejectedCount != 2 || ij.Report.StatusOnly != 1 {
		t.Fatalf("report = %+v", ij.Report)
	}
	var stored string
	if err := hs.d.Read.QueryRow(`SELECT result FROM jobs WHERE id = ?`, ij.JobID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "@") {
		t.Fatalf("stored report repeats a cell: %s", stored)
	}
}

// TestStatusCountsIncludeStatusOnlyContacts: a contact created or changed
// only for its blocking status is counted in status_counts, in the dry run
// and in the real import.
func TestStatusCountsIncludeStatusOnlyContacts(t *testing.T) {
	hs := newHarness(t)
	testutil.InsertContact(t, hs.d, "old@example.com", core.StatusActive)
	testutil.InsertContact(t, hs.d, "gone@example.com", core.StatusComplained)
	csv := "email,status,consent_date\n" +
		"new@example.com,unsubscribed,bad-date-value\n" + // new, status only
		"old@example.com,bounced,bad-date-value\n" + // existing, status only
		"gone@example.com,unsubscribed,bad-date-value\n" + // already blocked: nothing written
		"fine@example.com,,\n"
	want := map[string]int{"active": 1, "unsubscribed": 1, "bounced": 1}
	check := func(label string, rep ImportReport) {
		t.Helper()
		if rep.StatusOnly != 2 || rep.RejectedCount != 3 || rep.Created != 1 {
			t.Fatalf("%s report = %+v", label, rep)
		}
		if len(rep.StatusCounts) != len(want) {
			t.Fatalf("%s status_counts = %v, want %v", label, rep.StatusCounts, want)
		}
		for k, v := range want {
			if rep.StatusCounts[k] != v {
				t.Fatalf("%s status_counts = %v, want %v", label, rep.StatusCounts, want)
			}
		}
	}
	check("dry run", hs.dryRun(csv, ""))
	hs.startJobs()
	check("import", *hs.importNow(csv, "").Report)
	if c := hs.contact("new@example.com"); c.Status != core.StatusUnsubscribed {
		t.Fatalf("new = %s", c.Status)
	}
	if c := hs.contact("old@example.com"); c.Status != core.StatusBounced {
		t.Fatalf("old = %s", c.Status)
	}
}

// TestSuppressedContactIsRejectedNotNotReactivated pins what the report
// docs say: a manually suppressed contact listed as active is rejected as
// suppressed, not counted in not_reactivated.
func TestSuppressedContactIsRejectedNotNotReactivated(t *testing.T) {
	hs := newHarness(t)
	testutil.InsertContact(t, hs.d, "sup@example.com", core.StatusSuppressedManual)
	testutil.Exec(t, hs.d, `INSERT INTO suppressions (email_hash, reason, created_at) VALUES (?, 'manual', ?)`,
		core.EmailHash("sup@example.com"), hs.clock.Now().UTC().Format(time.RFC3339Nano))
	rep := hs.dryRun("email,status\nsup@example.com,active\n", "")
	if rep.NotReactivated != 0 || rep.RejectedCount != 1 || rep.Rejected[0].Reason != ReasonSuppressed {
		t.Fatalf("report = %+v", rep)
	}
}

// TestUploadOutlivesServerReadTimeout: a slow upload is not cut off by the
// server-wide ReadTimeout/WriteTimeout, because create-import extends its
// connection's deadlines.
func TestUploadOutlivesServerReadTimeout(t *testing.T) {
	hs := newHarness(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const serverTimeout = 400 * time.Millisecond
	srv := &http.Server{Handler: hs.h, ReadTimeout: serverTimeout, WriteTimeout: serverTimeout}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		_, _ = io.WriteString(pw, "email,first_name\n")
		for i := range 6 { // about 3 server timeouts in all
			time.Sleep(serverTimeout / 2)
			if _, err := io.WriteString(pw, "slow"+itoa(i)+"@example.com,Slow\n"); err != nil {
				return
			}
		}
	}()
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/api/v1/imports?dry_run=true", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/csv")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slow upload was cut off: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	var out ImportResult
	if err := json.Unmarshal(body, &out); err != nil || out.Report == nil || out.Report.Created != 6 {
		t.Fatalf("answer = %s (%v)", body, err)
	}
}

// timeoutReader fails like a connection whose read deadline has passed.
type timeoutReader struct{ prefix string }

func (r *timeoutReader) Read(p []byte) (int, error) {
	if r.prefix != "" {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
}

// TestUploadTimeoutIs408: an upload that outlasts its deadline answers 408
// in the API's error format, for every body type, instead of a 500.
func TestUploadTimeoutIs408(t *testing.T) {
	multipartHead := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"c.csv\"\r\n\r\nemail\r\na@example.com\r\n"
	tests := []struct {
		name, contentType, prefix string
	}{
		{"text/csv", "text/csv", "email\na@example.com\n"},
		{"json", "application/json", `{"csv": "email\na@exa`},
		{"multipart", "multipart/form-data; boundary=b", multipartHead},
	}
	for _, tt := range tests {
		for _, dry := range []bool{false, true} {
			t.Run(tt.name+map[bool]string{true: " dry run", false: ""}[dry], func(t *testing.T) {
				hs := newHarness(t)
				q := ""
				if dry {
					q = "?dry_run=true"
				}
				r := hs.send(adminToken, http.MethodPost, "/imports"+q, tt.contentType, &timeoutReader{prefix: tt.prefix})
				r.wantError(t, http.StatusRequestTimeout, "validation", "file")
				if n := hs.count(`SELECT count(*) FROM jobs`); n != 0 || len(hs.uploads()) != 0 {
					t.Fatalf("jobs = %d, uploads = %v", n, hs.uploads())
				}
			})
		}
	}
}
