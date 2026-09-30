package broadcasts_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
)

// archiveGet requests the public archive page through a mux mounted like
// the server's public mux.
func (hs *harness) archiveGet(method, path string) (*http.Response, string) {
	hs.t.Helper()
	mux := http.NewServeMux()
	mux.Handle(broadcasts.ArchivePattern, hs.svc.ArchiveHandler())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func TestArchivePage(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("ann", 1, core.StatusActive, "")
	body := `{"name":"News","email":{"subject":"Hi {{first_name | \"friend\"}}","preheader":"For {{email}}",` +
		`"body":"Hello {{first_name | \"there\"}} <{{email}}>\n\n[Offer](https://example.com/offer)\n\n[Leave]({{unsubscribe_url}})"}}`
	var b broadcasts.Broadcast
	hs.do("POST", "/broadcasts", body).wantStatus(t, http.StatusCreated).decode(t, &b)
	path := fmt.Sprintf("/archive/%d", b.ID)
	if b.ArchiveURL != "" {
		t.Fatalf("draft has archive_url %q", b.ArchiveURL)
	}
	if res, _ := hs.archiveGet("GET", path); res.StatusCode != http.StatusNotFound {
		t.Fatalf("draft archive = %d", res.StatusCode)
	}

	hs.sendAndSnapshot(b.ID)
	got := hs.get(b.ID)
	if want := "https://newsletter.example.com" + path; got.ArchiveURL != want {
		t.Fatalf("archive_url = %q, want %q", got.ArchiveURL, want)
	}
	res, page := hs.archiveGet("GET", path)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("archive = %d: %s", res.StatusCode, page)
	}
	for h, want := range map[string]string{
		"Content-Type": "text/html; charset=utf-8", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		"X-Robots-Tag": "noindex",
	} {
		if res.Header.Get(h) != want {
			t.Errorf("%s = %q, want %q", h, res.Header.Get(h), want)
		}
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Errorf("CSP = %q", csp)
	}
	// Fallbacks, no personal data, no tracking, inert unsubscribe link.
	for _, want := range []string{"Hello there", "<title>Hi friend</title>", `href="https://example.com/offer"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"ann0@example.net", "/c/", "/o/", "/u/"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	if n := hs.count(`SELECT count(*) FROM links`); n != 0 {
		t.Errorf("rendering the archive wrote %d links rows", n)
	}
	// HEAD has the headers and no body.
	if res, page := hs.archiveGet("HEAD", path); res.StatusCode != http.StatusOK || page != "" || res.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD = %d %q", res.StatusCode, page)
	}
	// Other methods are refused.
	if res, _ := hs.archiveGet("POST", path); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", res.StatusCode)
	}
	// Paused and sent broadcasts keep the page.
	for _, st := range []string{broadcasts.StatusPaused, broadcasts.StatusSent} {
		hs.setStatus(b.ID, st, "")
		if res, _ := hs.archiveGet("GET", path); res.StatusCode != http.StatusOK {
			t.Fatalf("%s archive = %d", st, res.StatusCode)
		}
	}
}

func TestArchiveNotFound(t *testing.T) {
	hs := newHarness(t)
	hs.contacts("vip", 1, core.StatusActive, "vip")
	seg := hs.segment("VIPs", "vip")
	all := hs.create("All", "")
	vip := hs.create("VIP", fmt.Sprintf(`{"type":"segment","segment_id":%d}`, seg))
	hs.setStatus(vip.ID, broadcasts.StatusSent, "")
	if got := hs.get(vip.ID); got.ArchiveURL != "" {
		t.Fatalf("segment broadcast archive_url = %q", got.ArchiveURL)
	}
	tests := []struct {
		name, status, path string
	}{
		{"segment broadcast", "", fmt.Sprintf("/archive/%d", vip.ID)},
		{"scheduled", broadcasts.StatusScheduled, fmt.Sprintf("/archive/%d", all.ID)},
		{"cancelled", broadcasts.StatusCancelled, fmt.Sprintf("/archive/%d", all.ID)},
		{"unknown", "", "/archive/999"},
		{"not a number", "", "/archive/abc"},
		{"leading zero", "", fmt.Sprintf("/archive/0%d", all.ID)},
		{"zero", "", "/archive/0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.status != "" {
				hs.setStatus(all.ID, tc.status, "")
			}
			if res, _ := hs.archiveGet("GET", tc.path); res.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d", res.StatusCode)
			}
		})
	}
	// The list carries archive_url too.
	hs.setStatus(all.ID, broadcasts.StatusSent, "")
	list, err := hs.svc.List(hs.ctx, broadcasts.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		if (b.ID == all.ID) != (b.ArchiveURL != "") {
			t.Fatalf("list %d archive_url = %q", b.ID, b.ArchiveURL)
		}
	}
	// Without a BaseURL there is no archive_url, but the page still works.
	bare := broadcasts.New(broadcasts.Deps{DB: hs.d, Content: hs.content, Segments: hs.segments, Settings: hs.settings, Clock: hs.clock})
	if got, err := bare.Get(hs.ctx, all.ID); err != nil || got.ArchiveURL != "" {
		t.Fatalf("no base URL: %q, %v", got.ArchiveURL, err)
	}
}
