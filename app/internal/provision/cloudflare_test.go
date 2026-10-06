package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testCFToken = "cf-test-token"

// fakeCloudflare is an httptest Cloudflare API. It fails the test on any
// method other than GET or POST, so nothing can ever update or delete.
type fakeCloudflare struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	zones    []Zone
	records  map[string][]DNSRecord // zone ID -> records
	pageSize int
	methods  []string
	posts    []DNSRecord
	failPost string // record name whose creation fails
}

func newFakeCloudflare(t *testing.T) *fakeCloudflare {
	f := &fakeCloudflare{
		t:        t,
		zones:    []Zone{{ID: "zone-1", Name: "example.com"}},
		records:  map[string][]DNSRecord{"zone-1": nil},
		pageSize: 2,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCloudflare) client() *Cloudflare {
	return &Cloudflare{BaseURL: f.srv.URL + "/client/v4", Token: testCFToken, HTTP: f.srv.Client()}
}

func (f *fakeCloudflare) reply(w http.ResponseWriter, status int, result any, info any, errs ...string) {
	type cfErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	body := map[string]any{"success": status < 300, "result": result, "errors": []cfErr{}}
	for i, e := range errs {
		body["errors"] = append(body["errors"].([]cfErr), cfErr{Code: 1000 + i, Message: e})
	}
	if info != nil {
		body["result_info"] = info
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeCloudflare) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods = append(f.methods, r.Method)
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		f.t.Errorf("Cloudflare got a %s %s: only GET and POST are allowed", r.Method, r.URL.Path)
		f.reply(w, http.StatusMethodNotAllowed, nil, nil, "method not allowed")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testCFToken {
		f.reply(w, http.StatusForbidden, nil, nil, "Authentication error")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	switch {
	case path == "/zones" && r.Method == http.MethodGet:
		var out []Zone
		for _, z := range f.zones {
			if z.Name == r.URL.Query().Get("name") {
				out = append(out, z)
			}
		}
		f.reply(w, http.StatusOK, append([]Zone{}, out...), nil)
	case strings.HasPrefix(path, "/zones/") && strings.HasSuffix(path, "/dns_records"):
		zone := strings.TrimSuffix(strings.TrimPrefix(path, "/zones/"), "/dns_records")
		recs, ok := f.records[zone]
		if !ok {
			f.reply(w, http.StatusNotFound, nil, nil, "zone not found")
			return
		}
		if r.Method == http.MethodGet {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 1 {
				page = 1
			}
			total := (len(recs) + f.pageSize - 1) / f.pageSize
			if total == 0 {
				total = 1
			}
			lo := min((page-1)*f.pageSize, len(recs))
			hi := min(lo+f.pageSize, len(recs))
			f.reply(w, http.StatusOK, append([]DNSRecord{}, recs[lo:hi]...), map[string]int{"page": page, "total_pages": total})
			return
		}
		var rec DNSRecord
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			f.reply(w, http.StatusBadRequest, nil, nil, "bad json")
			return
		}
		if rec.Name == f.failPost {
			f.reply(w, http.StatusBadRequest, nil, nil, "Record quota exceeded")
			return
		}
		f.posts = append(f.posts, rec)
		rec.ID = fmt.Sprintf("rec-%d", len(f.posts))
		f.records[zone] = append(f.records[zone], rec)
		f.reply(w, http.StatusOK, rec, nil)
	default:
		f.reply(w, http.StatusNotFound, nil, nil, "no route")
	}
}

func TestCloudflareFindZone(t *testing.T) {
	cf := newFakeCloudflare(t)
	cf.zones = append(cf.zones, Zone{ID: "zone-2", Name: "sub.other.org"})
	c := cf.client()
	tests := []struct{ domain, want string }{
		{"example.com", "zone-1"},
		{"mail.example.com", "zone-1"},
		{"Deep.Sub.Other.org.", "zone-2"},
	}
	for _, tt := range tests {
		z, err := c.FindZone(context.Background(), tt.domain)
		if err != nil || z.ID != tt.want {
			t.Errorf("FindZone(%q) = %+v, %v; want %s", tt.domain, z, err, tt.want)
		}
	}
	if _, err := c.FindZone(context.Background(), "unknown.net"); err == nil || !strings.Contains(err.Error(), "no zone") {
		t.Errorf("unknown zone err = %v", err)
	}
}

func TestCloudflareListRecordsPaginates(t *testing.T) {
	cf := newFakeCloudflare(t)
	for i := range 5 {
		cf.records["zone-1"] = append(cf.records["zone-1"], DNSRecord{ID: strconv.Itoa(i), Type: "A", Name: fmt.Sprintf("h%d.example.com", i), Content: "192.0.2.1"})
	}
	recs, err := cf.client().ListRecords(context.Background(), "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 || recs[4].Name != "h4.example.com" {
		t.Errorf("records = %+v", recs)
	}
}

func TestCloudflareErrors(t *testing.T) {
	cf := newFakeCloudflare(t)
	c := cf.client()
	c.Token = "wrong"
	_, err := c.ListRecords(context.Background(), "zone-1")
	var ce *CloudflareError
	if !errors.As(err, &ce) || ce.Status != http.StatusForbidden || !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("error leaks the token: %v", err)
	}

	if _, err := cf.client().CreateRecord(context.Background(), "zone-1", DNSRecord{Type: "A"}); err == nil {
		t.Error("incomplete record accepted")
	}
}

func TestApplyDNSCreatesOnlyMissingRecords(t *testing.T) {
	cf := newFakeCloudflare(t)
	cf.records["zone-1"] = []DNSRecord{
		{ID: "x1", Type: "A", Name: "newsletter.example.com", Content: "198.51.100.1"},                 // conflict
		{ID: "x2", Type: "CNAME", Name: "t1._domainkey.example.com", Content: "t1.dkim.amazonses.com"}, // present
	}
	in := baseRecordsInput()
	in.IPv4 = "203.0.113.7"
	c := cf.client()
	existing, err := c.ListRecords(context.Background(), "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanDNS(existing, Records(in))
	created, err := ApplyDNS(context.Background(), c, "zone-1", plan)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range cf.posts {
		names = append(names, p.Type+" "+p.Name)
		if (p.Type == "A" || p.Type == "CNAME") && (p.Proxied == nil || *p.Proxied) {
			t.Errorf("%s created proxied", p.Name)
		}
	}
	want := "CNAME t2._domainkey.example.com,CNAME t3._domainkey.example.com,MX bounce.example.com,TXT bounce.example.com"
	if strings.Join(names, ",") != want || len(created) != 4 {
		t.Errorf("created %v (%d), want %s", names, len(created), want)
	}
	// The conflicting A record is untouched.
	if cf.records["zone-1"][0].Content != "198.51.100.1" {
		t.Error("existing record changed")
	}
	for _, m := range cf.methods {
		if m != http.MethodGet && m != http.MethodPost {
			t.Errorf("method %s used", m)
		}
	}
}

func TestApplyDNSStopsOnError(t *testing.T) {
	cf := newFakeCloudflare(t)
	cf.failPost = "t2._domainkey.example.com"
	plan := PlanDNS(nil, Records(baseRecordsInput()))
	created, err := ApplyDNS(context.Background(), cf.client(), "zone-1", plan)
	if err == nil || !strings.Contains(err.Error(), "Record quota exceeded") {
		t.Fatalf("err = %v", err)
	}
	if len(created) != 1 || created[0].Name != "t1._domainkey.example.com" {
		t.Errorf("created = %+v", created)
	}
}
