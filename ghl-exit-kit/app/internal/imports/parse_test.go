package imports

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"mailroom/internal/core"
)

var parseNow = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

func mustParse(t *testing.T, csv string) *parsedFile {
	t.Helper()
	pf, err := parseFile(strings.NewReader(csv), parseNow)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	return pf
}

func TestParseFileFormats(t *testing.T) {
	tests := []struct {
		name  string
		csv   string
		check func(t *testing.T, pf *parsedFile)
	}{
		{
			name: "byte order mark",
			csv:  "\xEF\xBB\xBFemail,first_name\nA@Example.com,Ann\n",
			check: func(t *testing.T, pf *parsedFile) {
				if len(pf.records) != 1 || pf.records[0].email != "a@example.com" || pf.records[0].firstName != "Ann" {
					t.Fatalf("records = %+v", pf.records)
				}
			},
		},
		{
			name: "semicolon delimiter with quoted tags",
			csv:  "email;first_name;tags;attr.city\na@example.com;Ann;\"vip;news\";Aarhus\n",
			check: func(t *testing.T, pf *parsedFile) {
				r := pf.records[0]
				if r.firstName != "Ann" || !slices.Equal(r.tags, []string{"vip", "news"}) || r.attrs["city"] != "Aarhus" {
					t.Fatalf("record = %+v", r)
				}
			},
		},
		{
			name: "quoted fields with delimiter, quotes and line breaks",
			csv:  "email,first_name,last_name,attr.note\n\"a@example.com\",\"Ann, Jr\",\"O\"\"Brien\",\"line one\nline two\"\nb@example.com,Bob,,x\n",
			check: func(t *testing.T, pf *parsedFile) {
				r := pf.records[0]
				if r.firstName != "Ann, Jr" || r.attrs["note"] != "line one\nline two" {
					t.Fatalf("record = %+v", r)
				}
				if r.lastName != `O"Brien` {
					t.Fatalf("last name = %q", r.lastName)
				}
				// The multi-line field spans lines 2-3, so bob is on line 4.
				if pf.records[1].line != 4 {
					t.Fatalf("second record line = %d", pf.records[1].line)
				}
			},
		},
		{
			name: "CRLF line endings",
			csv:  "email,first_name\r\na@example.com,Ann\r\nb@example.com,Bob\r\n",
			check: func(t *testing.T, pf *parsedFile) {
				if len(pf.records) != 2 || pf.records[1].firstName != "Bob" || pf.records[1].line != 3 {
					t.Fatalf("records = %+v", pf.records)
				}
			},
		},
		{
			name: "no trailing newline, header in any case and order, spaces trimmed",
			csv:  " Tags , EMAIL ,Status\n a ; b , a@example.com , Unsubscribed ",
			check: func(t *testing.T, pf *parsedFile) {
				r := pf.records[0]
				if r.email != "a@example.com" || r.status != core.StatusUnsubscribed || !slices.Equal(r.tags, []string{"a", "b"}) {
					t.Fatalf("record = %+v", r)
				}
			},
		},
		{
			name: "blank rows are skipped",
			csv:  "email,first_name\n\na@example.com,Ann\n , \n\nb@example.com,\n",
			check: func(t *testing.T, pf *parsedFile) {
				if pf.rows != 2 || len(pf.records) != 2 || pf.records[1].line != 6 {
					t.Fatalf("rows = %d records = %+v", pf.rows, pf.records)
				}
			},
		},
		{
			name: "header only",
			csv:  "email\n",
			check: func(t *testing.T, pf *parsedFile) {
				if pf.rows != 0 || len(pf.records) != 0 {
					t.Fatalf("pf = %+v", pf)
				}
			},
		},
		{
			name: "unknown and invalid columns become warnings",
			csv:  "email,phone,attr.bad-key,,attr.ok_Key\na@example.com,123,x,y,z\n",
			check: func(t *testing.T, pf *parsedFile) {
				if len(pf.warnings) != 3 {
					t.Fatalf("warnings = %q", pf.warnings)
				}
				for _, want := range []string{`"phone"`, `"attr.bad-key"`, "column 4 has no name"} {
					if !strings.Contains(strings.Join(pf.warnings, "|"), want) {
						t.Errorf("warnings %q lack %s", pf.warnings, want)
					}
				}
				r := pf.records[0]
				if len(r.attrs) != 1 || r.attrs["ok_Key"] != "z" {
					t.Fatalf("attrs = %v", r.attrs)
				}
			},
		},
		{
			name: "short rows and empty trailing fields are fine",
			csv:  "email,first_name,last_name\na@example.com\nb@example.com,Bob,Builder,,\n",
			check: func(t *testing.T, pf *parsedFile) {
				if len(pf.records) != 2 || pf.rejections != 0 || pf.records[1].lastName != "Builder" {
					t.Fatalf("pf = %+v", pf)
				}
			},
		},
		{
			name: "consent dates",
			csv: "email,consent_source,consent_date\n" +
				"a@example.com,form,2024-03-01\n" +
				"b@example.com,,2024-01-09T14:05:00+01:00\n" +
				"c@example.com,,2024-01-09T14:05:00.123Z\n",
			check: func(t *testing.T, pf *parsedFile) {
				want := []time.Time{
					time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2024, 1, 9, 13, 5, 0, 0, time.UTC),
					time.Date(2024, 1, 9, 14, 5, 0, 123e6, time.UTC),
				}
				for i, w := range want {
					if got := pf.records[i].consentDate; !got.Equal(w) || got.Location() != time.UTC {
						t.Errorf("record %d consent date = %v, want %v", i, got, w)
					}
				}
				if pf.records[0].consentSource != "form" {
					t.Fatalf("source = %q", pf.records[0].consentSource)
				}
			},
		},
		{
			name: "tags are spelled as they first appear in the file",
			csv:  "email,tags\na@example.com,VIP;News\nb@example.com,vip;news;Other\n",
			check: func(t *testing.T, pf *parsedFile) {
				if !slices.Equal(pf.records[1].tags, []string{"VIP", "News", "Other"}) {
					t.Fatalf("tags = %v", pf.records[1].tags)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, mustParse(t, tt.csv))
		})
	}
}

func TestParseFileErrors(t *testing.T) {
	tests := []struct {
		name string
		csv  string
		want string
	}{
		{"empty file", "", "the file is empty"},
		{"only a byte order mark", "\xEF\xBB\xBF", "the file is empty"},
		{"missing email column", "first_name,last_name\nAnn,Smith\n", `no "email" column`},
		{"no header row", "a@example.com,Ann\n", `no "email" column`},
		{"duplicate column", "email,Email\na@example.com,b@example.com\n", `"email" appears more than once`},
		{"duplicate attribute column", "email,attr.x,attr.x\n", `"attr.x" appears more than once`},
		{"header not UTF-8", "email,\xff\n", "not valid UTF-8"},
		{"too many columns", "email" + strings.Repeat(",x", MaxColumns) + "\n", "at most 500"},
		{"broken quotes in the header", "email,\"first_name\na@example.com,Ann\n", "not valid CSV"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFile(strings.NewReader(tt.csv), parseNow)
			var ve *core.ValidationError
			if !errors.As(err, &ve) || ve.Field != "file" || !strings.Contains(ve.Message, tt.want) {
				t.Fatalf("err = %v, want file error containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRowRejections(t *testing.T) {
	header := "email,first_name,last_name,status,tags,consent_source,consent_date,attr.x"
	tests := []struct {
		name string
		row  string
		want string
	}{
		{"missing email", ",Ann,,,,,,", "email is missing"},
		{"invalid email", "not-an-email,,,,,,,", "email must contain exactly one @"},
		{"display name", "Ann <a@example.com>,,,,,,,", "email is not a valid email address"},
		{"no domain dot", "a@localhost,,,,,,,", "email has an invalid domain"},
		{"unknown status", "a@example.com,,,deleted,,,,", "status must be active, unsubscribed, bounced or complained"},
		{"pending is not importable", "a@example.com,,,pending,,,,", "status must be"},
		{"long first name", "a@example.com," + strings.Repeat("n", 201) + ",,,,,,", "first_name is longer than 200 characters"},
		{"control character in name", "a@example.com,,\"A\x01\",,,,,", "last_name contains control characters"},
		{"long tag", "a@example.com,,,," + strings.Repeat("t", 65) + ",,,", ReasonBadTag},
		{"long consent source", "a@example.com,,,,," + strings.Repeat("s", 201) + ",,", "consent_source is longer than 200 characters"},
		{"bad consent date", "a@example.com,,,,,,01/02/2024,", "consent_date must be RFC 3339"},
		{"future consent date", "a@example.com,,,,,,2025-06-03,", "consent_date is in the future"},
		{"too many fields", "a@example.com,,,,,,,x,extra", "the row has 9 fields but the header has 8"},
		{"invalid UTF-8", "a@example.com,\xffAnn,,,,,,", ReasonNotUTF8},
		{"attributes too large", "a@example.com,,,,,,," + strings.Repeat("v", 17<<10), "attributes must be at most"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pf := mustParse(t, header+"\n"+tt.row+"\n")
			if len(pf.records) != 0 || pf.rejections != 1 || len(pf.rejected) != 1 {
				t.Fatalf("pf = %+v", pf)
			}
			rej := pf.rejected[0]
			if rej.Line != 2 || !strings.Contains(rej.Reason, tt.want) {
				t.Fatalf("rejection = %+v, want %q", rej, tt.want)
			}
			if strings.Contains(rej.Reason, "example.com") {
				t.Fatalf("reason %q leaks the address", rej.Reason)
			}
		})
	}
}

func TestParseKeepsBlockingStatus(t *testing.T) {
	header := "email,first_name,last_name,status,tags,consent_source,consent_date,attr.x"
	tests := []struct {
		name       string
		row        string
		wantStatus core.ContactStatus // "" = no record
		want       string
	}{
		{"bad consent date", "x@example.com,,,unsubscribed,,,2024-03-01 10:00:00,", core.StatusUnsubscribed, "consent_date must be RFC 3339"},
		{"future consent date", "x@example.com,,,bounced,,,2099-01-01,", core.StatusBounced, "consent_date is in the future"},
		{"long tag", "x@example.com,,,complained," + strings.Repeat("t", 65) + ",,,", core.StatusComplained, ReasonBadTag},
		{"long name", "x@example.com," + strings.Repeat("n", 201) + ",,unsubscribed,,,,", core.StatusUnsubscribed, "first_name is longer"},
		{"control character", "x@example.com,,\"A\x01\",Unsubscribed,,,,", core.StatusUnsubscribed, "last_name contains control characters"},
		{"invalid UTF-8 elsewhere", "x@example.com,\xffAnn,,unsubscribed,,,,", core.StatusUnsubscribed, ReasonNotUTF8},
		{"long consent source", "x@example.com,,,unsubscribed,," + strings.Repeat("s", 201) + ",,", core.StatusUnsubscribed, "consent_source is longer"},
		{"attributes too large", "x@example.com,,,unsubscribed,,,," + strings.Repeat("v", 17<<10), core.StatusUnsubscribed, "attributes must be at most"},
		{"active is not kept", "x@example.com,,,active,,,2024-03-01 10:00:00,", "", "consent_date must be RFC 3339"},
		{"no status is not kept", "x@example.com,,,,,,2024-03-01 10:00:00,", "", "consent_date must be RFC 3339"},
		{"invalid status", "x@example.com,,,deleted,,,,", "", "status must be"},
		{"too many fields", "x@example.com,,,unsubscribed,,,,x,extra", "", "the row has 9 fields"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pf := mustParse(t, header+"\n"+tt.row+"\n")
			if pf.rejections != 1 || pf.rows != 1 || !strings.Contains(pf.rejected[0].Reason, tt.want) {
				t.Fatalf("rejected = %+v", pf.rejected)
			}
			if tt.wantStatus == "" {
				if len(pf.records) != 0 {
					t.Fatalf("records = %+v", pf.records)
				}
				return
			}
			if len(pf.records) != 1 {
				t.Fatalf("records = %+v", pf.records)
			}
			r := pf.records[0]
			if !r.statusOnly || r.status != tt.wantStatus || r.email != "x@example.com" || r.line != 2 ||
				r.firstName != "" || r.lastName != "" || len(r.tags) != 0 || len(r.attrs) != 0 || r.hasConsent() {
				t.Fatalf("record = %+v", r)
			}
			wantSuffix := "; only the row's status (" + string(tt.wantStatus) + ") was imported"
			if !strings.HasSuffix(pf.rejected[0].Reason, wantSuffix) || strings.Contains(pf.rejected[0].Reason, "example.com") {
				t.Fatalf("reason = %q", pf.rejected[0].Reason)
			}
		})
	}
}

func TestParseStatusOnlyRowsAndDuplicates(t *testing.T) {
	csv := "email,first_name,status,consent_date\n" +
		"a@example.com,Ann,active,\n" + // 2 valid
		"a@example.com,,unsubscribed,bad\n" + // 3 status-only: raises a's status
		"b@example.com,,bounced,bad\n" + // 4 status-only placeholder
		"b@example.com,Bob,active,\n" + // 5 valid: replaces the placeholder, keeps bounced
		"b@example.com,,,\n" + // 6 duplicate of 5
		"c@example.com,,unsubscribed,bad\n" + // 7 status-only
		"c@example.com,,complained,bad\n" // 8 status-only: raises c
	pf := mustParse(t, csv)
	if pf.rows != 7 || pf.rejections != 4 || pf.duplicates != 1 || len(pf.records) != 3 {
		t.Fatalf("rows=%d rejections=%d duplicates=%d records=%d", pf.rows, pf.rejections, pf.duplicates, len(pf.records))
	}
	a, b, c := pf.records[0], pf.records[1], pf.records[2]
	if a.statusOnly || a.firstName != "Ann" || a.status != core.StatusUnsubscribed || a.line != 2 {
		t.Fatalf("a = %+v", a)
	}
	if b.statusOnly || b.firstName != "Bob" || b.status != core.StatusBounced || b.line != 5 || !b.merged {
		t.Fatalf("b = %+v", b)
	}
	if !c.statusOnly || c.status != core.StatusComplained || c.line != 7 {
		t.Fatalf("c = %+v", c)
	}
	var lines []int
	for _, r := range pf.rejected {
		lines = append(lines, r.Line)
	}
	if !slices.Equal(lines, []int{3, 4, 7, 8}) {
		t.Fatalf("rejected lines = %v", lines)
	}
}

func TestParseMergedAttributesTooLargeKeepsBlockingStatus(t *testing.T) {
	big := strings.Repeat("v", 9<<10)
	csv := "email,status,attr.a,attr.b\n" + "x@example.com,," + big + ",\n" + "x@example.com,unsubscribed,," + big + "\n"
	pf := mustParse(t, csv)
	if len(pf.records) != 1 || pf.rejections != 1 || pf.duplicates != 1 {
		t.Fatalf("records = %+v rejected = %+v", pf.records, pf.rejected)
	}
	r := pf.records[0]
	if !r.statusOnly || r.status != core.StatusUnsubscribed || len(r.attrs) != 0 || r.line != 2 {
		t.Fatalf("record = %+v", r)
	}
	if !strings.Contains(pf.rejected[0].Reason, "only the row's status (unsubscribed)") {
		t.Fatalf("reason = %q", pf.rejected[0].Reason)
	}
}

func TestParseMergesDuplicates(t *testing.T) {
	csv := "email,first_name,last_name,status,tags,consent_source,consent_date,attr.a,attr.b\n" +
		"a@example.com,Ann,Old,active,one;Two,src1,2020-01-01,1,keep\n" +
		"b@example.com,Bob,,,,,,,\n" +
		"A@EXAMPLE.COM,,New,unsubscribed,two;three,,2021-02-02,2,\n" +
		"a@example.com,Anna,,active,four,src3,,,\n" +
		"bad-row,,,,,,,,\n" +
		"b@example.com,,,complained,,,,,\n" +
		"b@example.com,,,bounced,,,,,\n"
	pf := mustParse(t, csv)
	if pf.rows != 7 || pf.duplicates != 4 || pf.rejections != 1 || len(pf.records) != 2 {
		t.Fatalf("rows=%d duplicates=%d rejections=%d records=%d", pf.rows, pf.duplicates, pf.rejections, len(pf.records))
	}
	a, b := pf.records[0], pf.records[1]
	if a.line != 2 || a.firstName != "Anna" || a.lastName != "New" || a.status != core.StatusUnsubscribed {
		t.Fatalf("a = %+v", a)
	}
	if !slices.Equal(a.tags, []string{"one", "Two", "three", "four"}) {
		t.Fatalf("a tags = %v", a.tags)
	}
	if a.consentSource != "src3" || !a.consentDate.Equal(time.Date(2021, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a consent = %q %v", a.consentSource, a.consentDate)
	}
	if a.attrs["a"] != "2" || a.attrs["b"] != "keep" {
		t.Fatalf("a attrs = %v", a.attrs)
	}
	if b.line != 3 || b.firstName != "Bob" || b.status != core.StatusComplained {
		t.Fatalf("b = %+v", b)
	}
	if pf.rows != len(pf.records)+pf.duplicates+pf.rejections {
		t.Fatal("rows must equal records + duplicates + rejections")
	}
}

func TestParseMergedAttributesTooLarge(t *testing.T) {
	big := strings.Repeat("v", 9<<10)
	csv := "email,attr.a,attr.b\n" + "x@example.com," + big + ",\n" + "y@example.com,1,2\n" + "x@example.com,," + big + "\n"
	pf := mustParse(t, csv)
	if len(pf.records) != 1 || pf.records[0].email != "y@example.com" {
		t.Fatalf("records = %+v", pf.records)
	}
	if pf.rejections != 1 || pf.rejected[0].Line != 2 || !strings.Contains(pf.rejected[0].Reason, "attributes") {
		t.Fatalf("rejected = %+v", pf.rejected)
	}
}

func TestParseRejectedListIsCapped(t *testing.T) {
	csv := csvRows("email", MaxRejectedListed+50, func(i int) string { return "bad" })
	pf := mustParse(t, csv)
	if pf.rejections != MaxRejectedListed+50 || len(pf.rejected) != MaxRejectedListed {
		t.Fatalf("rejections = %d listed = %d", pf.rejections, len(pf.rejected))
	}
}

func TestDetectDelimiter(t *testing.T) {
	tests := []struct {
		in   string
		want rune
	}{
		{"email,first_name", ','},
		{"email;first_name;tags", ';'},
		{"email", ','},
		{`"a;b;c",email`, ','},
		{`email;"x,y,z"`, ';'},
		{"email;name\na,b,c,d,e", ';'},
		{"email,name\r\na;b;c;d", ','},
	}
	for _, tt := range tests {
		if got := detectDelimiter([]byte(tt.in)); got != tt.want {
			t.Errorf("detectDelimiter(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseFileTooLarge(t *testing.T) {
	_, err := parseFile(limitFile(bigCSV(MaxFileBytes+100)), parseNow)
	if !errors.Is(err, errFileTooLarge) {
		t.Fatalf("err = %v, want errFileTooLarge", err)
	}
}

// bigCSV returns a CSV of n bytes: a header and one address repeated.
func bigCSV(n int64) io.Reader {
	return io.LimitReader(io.MultiReader(strings.NewReader("email\n"), &repeater{line: "someone@example.com\n"}), n)
}

// repeater repeats line forever.
type repeater struct {
	line string
	pos  int
}

func (r *repeater) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.line[r.pos:])
		n += c
		r.pos = (r.pos + c) % len(r.line)
	}
	return n, nil
}

func TestParseBadQuotesRejectOnlyTheRow(t *testing.T) {
	csv := "email,first_name\na@example.com,An\"n\nb@example.com,\"Bob\"x\nc@example.com,Cy\n\"d@example.com,Dee\n"
	pf := mustParse(t, csv)
	if len(pf.records) != 1 || pf.records[0].email != "c@example.com" {
		t.Fatalf("records = %+v", pf.records)
	}
	if pf.rows != 4 || pf.rejections != 3 {
		t.Fatalf("rows = %d rejections = %d", pf.rows, pf.rejections)
	}
	lines := []int{pf.rejected[0].Line, pf.rejected[1].Line, pf.rejected[2].Line}
	if !slices.Equal(lines, []int{2, 3, 5}) || !strings.Contains(pf.rejected[0].Reason, "not valid CSV") {
		t.Fatalf("rejected = %+v", pf.rejected)
	}
}
