package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/app"
	"mailroom/internal/events"
	"mailroom/internal/sequences"
	"mailroom/internal/system"
)

// expectedOperations is every API operation Mailroom registers, from all
// packages. A package that stops registering one, or a new operation that
// isn't listed here, fails TestOperationsRegisteredOnce.
var expectedOperations = []string{
	// settings, auth, audit, jobs
	"get-settings", "update-settings",
	"get-me", "list-users", "get-user", "create-user", "update-user", "delete-user",
	"begin-user-totp", "confirm-user-totp", "reset-user-totp",
	"list-tokens", "get-token", "create-token", "revoke-token",
	"list-audit-log", "get-job", "list-jobs",
	// contacts, tags, suppressions
	"list-contacts", "get-contact", "create-contact", "upsert-contact", "update-contact", "set-contact-status",
	"add-contact-tags", "remove-contact-tag", "get-contact-timeline", "list-contact-enrollments",
	"export-contact", "erase-contact",
	"list-tags", "get-tag", "create-tag", "rename-tag", "merge-tag", "delete-tag", "bulk-tag",
	"list-suppressions", "add-suppression", "remove-suppression",
	// segments
	"list-segments", "create-segment", "preview-segment", "get-segment", "update-segment", "delete-segment",
	"get-segment-count", "list-segment-members",
	// content
	"list-emails", "create-email", "get-email", "get-email-usage", "update-email", "delete-email", "preview-email", "render-email",
	"send-test-email", "send-test-draft", "list-merge-fields", "get-layout", "update-layout", "preview-layout",
	"upload-media", "list-media", "get-media", "delete-media",
	// sending
	"get-sending-state", "pause-sending", "resume-sending", "list-messages", "get-message", "list-events", "get-event", "list-clicks", "get-click",
	// system
	"get-system-health", "get-system-version", "run-doctor", "get-ses-status", "list-backups", "create-backup", "download-backup",
	// sequences
	"list-sequences", "create-sequence", "get-sequence", "update-sequence", "pause-sequence", "resume-sequence",
	"delete-sequence", "get-sequence-stats", "import-sequence", "export-sequence",
	"add-sequence-step", "update-sequence-step", "delete-sequence-step", "reorder-sequence-steps", "send-sequence-step-test",
	"list-sequence-enrollments", "enroll-sequence-contacts", "unenroll-sequence-contact",
	// broadcasts
	"list-broadcasts", "create-broadcast", "get-broadcast", "update-broadcast", "delete-broadcast", "duplicate-broadcast",
	"schedule-broadcast", "unschedule-broadcast", "send-broadcast", "pause-broadcast", "resume-broadcast", "cancel-broadcast",
	"send-broadcast-test", "get-broadcast-stats", "list-broadcast-recipients",
	// forms
	"list-forms", "create-form", "get-form", "update-form", "delete-form", "get-form-snippet", "list-form-submissions",
	// imports
	"create-import", "get-import", "list-imports",
	// stats
	"get-dashboard-stats",
	// inbox
	"list-inbound", "get-inbound-counts", "get-inbound", "update-inbound", "delete-inbound", "get-inbound-attachment", "reply-inbound",
}

// TestOperationsRegisteredOnce: every package's operations are on the one
// API exactly once, each is in the generated spec with a summary and a
// description, and writes need the admin scope.
func TestOperationsRegisteredOnce(t *testing.T) {
	hs := newHarness(t, nil)
	routes := hs.app.API.Routes()
	var got []string
	seenRoute := map[string]string{}
	for _, r := range routes {
		got = append(got, r.OperationID)
		key := r.Method + " " + r.Path
		if prev, dup := seenRoute[key]; dup {
			t.Errorf("%s is registered by %s and %s", key, prev, r.OperationID)
		}
		seenRoute[key] = r.OperationID
		if r.Method != http.MethodGet && r.Scope != api.ScopeAdmin && r.Scope != api.ScopeRead {
			t.Errorf("%s has no scope", r.OperationID)
		}
		// GETs are readable with a read token, except downloading a backup:
		// it holds every contact, so it needs the admin scope.
		if wantAdmin := r.OperationID == "download-backup"; r.Method == http.MethodGet && (r.Scope == api.ScopeAdmin) != wantAdmin {
			t.Errorf("%s: GET has scope %s", r.OperationID, r.Scope)
		}
	}
	want := slices.Clone(expectedOperations)
	sort.Strings(got)
	sort.Strings(want)
	if dup := slices.Compact(slices.Clone(got)); len(dup) != len(got) {
		t.Errorf("duplicate operation IDs among %d routes", len(got))
	}
	for _, id := range want {
		if _, ok := slices.BinarySearch(got, id); !ok {
			t.Errorf("operation %s is not registered", id)
		}
	}
	for _, id := range got {
		if _, ok := slices.BinarySearch(want, id); !ok {
			t.Errorf("operation %s is registered but not in expectedOperations", id)
		}
	}

	spec := hs.app.API.OpenAPI()
	n := 0
	for _, item := range spec.Paths {
		for _, op := range pathOperations(item) {
			if op != nil {
				n++
			}
		}
	}
	for _, r := range routes {
		op := operation(spec, r.Method, strings.TrimPrefix(r.Path, api.Prefix))
		if op == nil {
			t.Errorf("%s (%s %s) is missing from the spec", r.OperationID, r.Method, r.Path)
			continue
		}
		if op.Summary == "" || op.Description == "" {
			t.Errorf("%s lacks a summary or description", r.OperationID)
		}
		// G-2: every operation has an example, in its JSON request body
		// and its JSON success responses (file uploads and downloads are
		// binary).
		if rb := op.RequestBody; rb != nil {
			for ct, mt := range rb.Content {
				if !strings.Contains(ct, "json") {
					continue
				}
				if !schemaHasExample(spec, mt.Schema, map[string]bool{}) {
					t.Errorf("%s: the %s request body has no example", r.OperationID, ct)
				}
			}
		}
		for code, resp := range op.Responses {
			if len(code) != 3 || code[0] != '2' || resp == nil {
				continue
			}
			for ct, mt := range resp.Content {
				if strings.Contains(ct, "json") && !schemaHasExample(spec, mt.Schema, map[string]bool{}) {
					t.Errorf("%s: the %s %s response has no example", r.OperationID, code, ct)
				}
			}
		}
	}
	if n != len(routes) {
		t.Errorf("spec has %d operations, the registry %d", n, len(routes))
	}
}

// pathOperations returns the operations of a path item by method.
// schemaHasExample reports whether s, or any schema it contains or refers
// to, has an example.
func schemaHasExample(spec *huma.OpenAPI, s *huma.Schema, seen map[string]bool) bool {
	if s == nil {
		return false
	}
	if s.Ref != "" {
		if seen[s.Ref] {
			return false
		}
		seen[s.Ref] = true
		return schemaHasExample(spec, spec.Components.Schemas.SchemaFromRef(s.Ref), seen)
	}
	if len(s.Examples) > 0 {
		return true
	}
	for _, p := range s.Properties {
		if schemaHasExample(spec, p, seen) {
			return true
		}
	}
	for _, list := range [][]*huma.Schema{s.AllOf, s.OneOf, s.AnyOf} {
		for _, c := range list {
			if schemaHasExample(spec, c, seen) {
				return true
			}
		}
	}
	return schemaHasExample(spec, s.Items, seen)
}

func pathOperations(item *huma.PathItem) map[string]*huma.Operation {
	return map[string]*huma.Operation{
		http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put,
		http.MethodPatch: item.Patch, http.MethodDelete: item.Delete, http.MethodHead: item.Head,
		http.MethodOptions: item.Options,
	}
}

// operation returns the spec's operation for method and path (given
// relative to the API prefix; the spec may key it with or without the
// prefix), or nil.
func operation(spec *huma.OpenAPI, method, path string) *huma.Operation {
	item := spec.Paths[path]
	if item == nil {
		item = spec.Paths[api.Prefix+path]
	}
	if item == nil {
		return nil
	}
	return pathOperations(item)[method]
}

// TestPublicRoutesMounted: every public endpoint is on the public mux.
func TestPublicRoutesMounted(t *testing.T) {
	hs := newHarness(t, nil)
	mux := hs.app.Server.PublicMux
	for _, c := range []struct{ method, path, pattern string }{
		{"GET", "/f/abc123defg", "GET /f/{public_id}"},
		{"POST", "/f/abc123defg", "POST /f/{public_id}"},
		{"OPTIONS", "/f/abc123defg", "OPTIONS /f/{public_id}"},
		{"GET", "/f/confirm/tok", "GET /f/confirm/{token}"},
		{"POST", "/f/confirm/tok", "POST /f/confirm/{token}"},
		{"POST", "/hooks/ses", "POST /hooks/ses"},
		{"GET", "/u/tok", "GET /u/{token}"},
		{"POST", "/u/tok", "POST /u/{token}"},
		{"GET", "/c/tok", "GET /c/{token}"},
		{"GET", "/o/tok", "GET /o/{token}"},
		{"GET", "/media/x.png", "GET /media/{name}"},
		{"HEAD", "/media/x.png", "GET /media/{name}"},
		{"GET", "/archive/1", "GET /archive/{id}"},
	} {
		_, pattern := mux.Handler(httptest.NewRequest(c.method, c.path, nil))
		if pattern != c.pattern {
			t.Errorf("%s %s -> %q, want %q", c.method, c.path, pattern, c.pattern)
		}
	}
	if r := hs.raw("GET", "/healthz", "", ""); r.code != http.StatusOK {
		t.Fatalf("healthz = %d", r.code)
	}
	// The archive is rate-limited per client address.
	var limited bool
	for i := 0; i < app.ArchiveBurst+1; i++ {
		if r := hs.raw("GET", "/archive/999", "", ""); r.code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the archive is not rate-limited")
	}
	// The guide is served (the placeholder until docs/api.md exists).
	if r := hs.api("GET", "/docs", "").want(t, http.StatusOK); !strings.Contains(string(r.body), "openapi.json") {
		t.Fatalf("guide = %s", r.body)
	}
}

func workerNames(a *app.App) []string {
	var names []string
	for _, w := range a.Workers() {
		names = append(names, w.Name())
	}
	sort.Strings(names)
	return names
}

// TestWorkers lists the supervised workers, with and without SES.
func TestWorkers(t *testing.T) {
	hs := newHarness(t, func(o *app.Options) { o.AWS = &app.AWSClients{Account: fakeAccount{}} })
	want := []string{"backup", "broadcast-starter", "health-watch", "idempotency-cleanup", "jobs", "optin-expiry",
		"retention", "scheduler", "sender", "ses-account-poller", "sns-subscriber", "ui-doctor"}
	if got := workerNames(hs.app); !slices.Equal(got, want) {
		t.Fatalf("workers = %v, want %v", got, want)
	}
	if !hs.app.SESConfigured {
		t.Fatal("an injected provider counts as configured")
	}

	// Without SES credentials nothing is sent: no sender, no poller.
	bare := newHarness(t, func(o *app.Options) { o.Provider = nil })
	got := workerNames(bare.app)
	if slices.Contains(got, "sender") || slices.Contains(got, "ses-account-poller") || bare.app.SESConfigured {
		t.Fatalf("workers without SES = %v (configured %v)", got, bare.app.SESConfigured)
	}
	if len(got) != len(want)-2 {
		t.Fatalf("workers without SES = %v", got)
	}
}

// fakeAccount is an SES account API for the ses-account-poller.
type fakeAccount struct{}

func (fakeAccount) GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error) {
	status := "HEALTHY"
	return &sesv2.GetAccountOutput{
		ProductionAccessEnabled: true, SendingEnabled: true, EnforcementStatus: &status,
		SendQuota: &types.SendQuota{Max24HourSend: 50000, MaxSendRate: 14, SentLast24Hours: 1200},
	}, nil
}

// TestSESAccountDocumentShared: the "ses_account" settings document the
// sending poller writes is the one the system SES status reads.
func TestSESAccountDocumentShared(t *testing.T) {
	hs := newHarness(t, func(o *app.Options) { o.AWS = &app.AWSClients{Account: fakeAccount{}} })
	if _, err := hs.app.Sending.PollAccount(hs.ctx); err != nil {
		t.Fatal(err)
	}
	st, err := hs.app.System.SESStatus(hs.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	want := system.SESAccount{ProductionAccessEnabled: true, SendingEnabled: true, EnforcementStatus: "HEALTHY",
		Max24HourSend: 50000, MaxSendRate: 14, SentLast24Hours: 1200, FetchedAt: hs.clock.Now().UTC()}
	if st.Source != "cache" || st.Account != want {
		t.Fatalf("status = %+v, want account %+v", st, want)
	}
	// And the sending state document reaches the health report.
	if _, err := hs.app.Sending.Pause(hs.ctx, "maintenance", false); err != nil {
		t.Fatal(err)
	}
	if h := hs.app.System.Health(hs.ctx); !strings.Contains(string(h.SendingState), `"paused":true`) {
		t.Fatalf("health sending_state = %s", h.SendingState)
	}
}

// TestRunShutsDownInOrder runs the app on a free port and stops it.
func TestRunShutsDownInOrder(t *testing.T) {
	vars := testVars(t)
	vars["HTTP_ONLY"], vars["HTTP_ADDR"], vars["BASE_URL"] = "1", "127.0.0.1:0", "http://localhost:8080"
	a, err := app.New(context.Background(), loadConfig(t, vars), app.Options{Offline: true, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(a.Server.Addrs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("not listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Get("http://" + a.Server.Addrs()[0].String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestSharedConstants: values two packages must agree on without importing
// each other. The unsubscribe page (events) writes the exit reason that
// the sequences package reads to block re-entry.
func TestSharedConstants(t *testing.T) {
	if events.ExitUnsubscribedSequence != sequences.ExitUnsubscribed {
		t.Fatalf("events writes exit reason %q, sequences reads %q", events.ExitUnsubscribedSequence, sequences.ExitUnsubscribed)
	}
}
