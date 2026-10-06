package events_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/config"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/events"
	"mailroom/internal/hooks"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
	"mailroom/internal/tokens"
)

const (
	testRegion   = "eu-west-1"
	testTopicARN = "arn:aws:sns:eu-west-1:123456789012:mailroom-ses-events"
	testCertURL  = "https://sns.eu-west-1.amazonaws.com/SimpleNotificationService-0123456789abcdef.pem"
	testBaseURL  = "https://newsletter.example.com"
)

// snsKey is one RSA key and self-signed certificate shared by all tests
// (key generation is slow).
var snsKey = sync.OnceValue(func() struct {
	key     *rsa.PrivateKey
	certPEM []byte
} {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sns.amazonaws.com"},
		NotBefore:    testutil.Epoch.AddDate(-1, 0, 0),
		NotAfter:     testutil.Epoch.AddDate(10, 0, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return struct {
		key     *rsa.PrivateKey
		certPEM []byte
	}{key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
})

// fakeHTTP serves canned responses by URL and records every request.
type fakeHTTP struct {
	mu        sync.Mutex
	responses map[string]fakeResponse
	requests  []string
}

type fakeResponse struct {
	status int
	body   string
	err    error
}

func (f *fakeHTTP) set(url string, r fakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[url] = r
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req.Method+" "+req.URL.String())
	r, ok := f.responses[req.URL.String()]
	if !ok {
		r = fakeResponse{status: http.StatusNotFound, body: "not found"}
	}
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func (f *fakeHTTP) calls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// fakeAlerter records alerts.
type fakeAlerter struct {
	mu   sync.Mutex
	keys []string
}

func (a *fakeAlerter) Alert(_ context.Context, _ *sql.Tx, key, _, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys = append(a.keys, key)
	return nil
}

func (a *fakeAlerter) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.keys...)
}

// harness is a Service over a fresh database.
type harness struct {
	t        *testing.T
	db       *db.DB
	clock    *testutil.Clock
	signer   *tokens.Signer
	store    *contactstore.Store
	settings *settings.Service
	http     *fakeHTTP
	alerts   *fakeAlerter
	sns      *fakeSNS
	svc      *events.Service
	handler  http.Handler

	mu            sync.Mutex
	statusChanges []hooks.StatusChanged
}

type harnessOption func(*events.Deps)

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		db:     testutil.NewDB(t),
		clock:  testutil.NewClock(testutil.Epoch),
		http:   &fakeHTTP{responses: map[string]fakeResponse{}},
		alerts: &fakeAlerter{},
		sns:    &fakeSNS{},
	}
	h.signer = testutil.NewSigner(t, h.clock)
	bus := hooks.New()
	bus.OnStatusChanged(func(_ context.Context, _ *sql.Tx, e hooks.StatusChanged) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.statusChanges = append(h.statusChanges, e)
		return nil
	})
	h.store = &contactstore.Store{DB: h.db, Bus: bus, Clock: h.clock}
	cfg := config.Config{
		Domain:      "example.com",
		BaseURL:     testBaseURL,
		AWSRegion:   testRegion,
		SNSTopicARN: testTopicARN,
		FromName:    "Acme News",
		FromEmail:   "news@example.com",
		OwnerEmail:  "owner@example.org",
		Timezone:    "UTC",
	}
	h.settings = settings.New(settings.Deps{DB: h.db, Clock: h.clock, Domain: cfg.Domain})
	if _, err := h.settings.SeedFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	h.http.set(testCertURL, fakeResponse{status: http.StatusOK, body: string(snsKey().certPEM)})
	deps := events.Deps{
		DB:                  h.db,
		Store:               h.store,
		Settings:            h.settings,
		Signer:              h.signer,
		Config:              cfg,
		SNS:                 h.sns,
		HTTP:                h.http,
		Alerter:             h.alerts,
		Clock:               h.clock,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		SubscribeMinBackoff: time.Millisecond,
		SubscribeMaxBackoff: 4 * time.Millisecond,
	}
	for _, o := range opts {
		o(&deps)
	}
	h.svc = events.New(deps)
	h.handler = h.svc.Handler()
	return h
}

func (h *harness) changes() []hooks.StatusChanged {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hooks.StatusChanged(nil), h.statusChanges...)
}

// do sends a request to the Service's handler.
func (h *harness) do(method, target, contentType string, body string, header ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// postSNS posts an SNS envelope with the matching message-type header.
func (h *harness) postSNS(e map[string]any) *httptest.ResponseRecorder {
	h.t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		h.t.Fatal(err)
	}
	typ, _ := e["Type"].(string)
	return h.do(http.MethodPost, "/hooks/ses", "text/plain; charset=UTF-8", string(b), "x-amz-sns-message-type", typ)
}

// count runs a COUNT(*) query.
func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	return testutil.Count(h.t, h.db.Read, query, args...)
}

func (h *harness) contactStatus(id int64) core.ContactStatus {
	h.t.Helper()
	c, err := h.store.Get(context.Background(), nil, id)
	if err != nil {
		h.t.Fatalf("get contact %d: %v", id, err)
	}
	return c.Status
}

func (h *harness) messageStatus(id int64) string {
	h.t.Helper()
	var st string
	if err := h.db.Read.QueryRow(`SELECT status FROM messages WHERE id = ?`, id).Scan(&st); err != nil {
		h.t.Fatalf("message %d: %v", id, err)
	}
	return st
}

// insertMessage inserts a sent message and returns its id. contactID 0 and
// to "" store NULL.
func (h *harness) insertMessage(kind string, contactID int64, to, sesID string, enrollmentID int64) int64 {
	h.t.Helper()
	now := db.FormatTime(h.clock.Now())
	var sid any
	if sesID != "" {
		sid = sesID
	}
	res, err := h.db.Write.Exec(`
		INSERT INTO messages (kind, enrollment_id, contact_id, to_email, status, next_attempt_at, ses_message_id, queued_at, sent_at, updated_at)
		VALUES (?, ?, ?, ?, 'sent', ?, ?, ?, ?, ?)`,
		kind, db.NullID(enrollmentID), db.NullID(contactID), db.NullString(to), now, sid, now, now, now)
	if err != nil {
		h.t.Fatalf("insert message: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// token signs a public token.
func (h *harness) token(p tokens.Purpose, pl tokens.Payload) string {
	h.t.Helper()
	tok, err := h.signer.Sign(p, pl)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// snsMessage builds a signed SNS envelope. version is "1" or "2".
func snsMessage(t *testing.T, typ, messageID, message, version string, mutate ...func(map[string]any)) map[string]any {
	t.Helper()
	e := map[string]any{
		"Type":             typ,
		"MessageId":        messageID,
		"TopicArn":         testTopicARN,
		"Message":          message,
		"Timestamp":        "2026-01-05T09:00:00.000Z",
		"SignatureVersion": version,
		"SigningCertURL":   testCertURL,
	}
	switch typ {
	case events.TypeNotification:
		e["UnsubscribeURL"] = "https://sns.eu-west-1.amazonaws.com/?Action=Unsubscribe&SubscriptionArn=" + testTopicARN + ":sub"
	default:
		e["Token"] = "2336412f37fb687f5d51e6e2425f004aed7b5ff8"
		e["SubscribeURL"] = "https://sns.eu-west-1.amazonaws.com/?Action=ConfirmSubscription&TopicArn=" + testTopicARN + "&Token=2336412f37fb687f5d51e6e2425f004aed7b5ff8"
	}
	for _, m := range mutate {
		m(e)
	}
	signEnvelope(t, e)
	return e
}

// signEnvelope sets Signature as SNS does: RSA PKCS#1 v1.5 over the
// canonical string (SHA1 for version 1, SHA256 for version 2). The
// canonical string is built here independently of the package.
func signEnvelope(t *testing.T, e map[string]any) {
	t.Helper()
	var keys []string
	if e["Type"] == events.TypeNotification {
		keys = []string{"Message", "MessageId", "Subject", "Timestamp", "TopicArn", "Type"}
	} else {
		keys = []string{"Message", "MessageId", "SubscribeURL", "Timestamp", "Token", "TopicArn", "Type"}
	}
	var b bytes.Buffer
	for _, k := range keys {
		v, ok := e[k]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "%s\n%s\n", k, v)
	}
	var (
		hash crypto.Hash
		sum  []byte
	)
	switch e["SignatureVersion"] {
	case "1":
		s := sha1.Sum(b.Bytes())
		hash, sum = crypto.SHA1, s[:]
	default:
		s := sha256.Sum256(b.Bytes())
		hash, sum = crypto.SHA256, s[:]
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, snsKey().key, hash, sum)
	if err != nil {
		t.Fatal(err)
	}
	e["Signature"] = base64.StdEncoding.EncodeToString(sig)
}

// sesJSON renders an SES event as JSON.
func sesJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func bounceEvent(sesID, bounceType string, recipients ...string) map[string]any {
	rs := make([]map[string]string, 0, len(recipients))
	for _, r := range recipients {
		rs = append(rs, map[string]string{"emailAddress": r, "action": "failed", "status": "5.1.1"})
	}
	return map[string]any{
		"eventType": "Bounce",
		"bounce": map[string]any{
			"bounceType": bounceType, "bounceSubType": "General",
			"bouncedRecipients": rs, "timestamp": "2026-01-05T09:00:00.000Z", "feedbackId": "fb-1",
		},
		"mail": map[string]any{"messageId": sesID, "destination": recipients, "timestamp": "2026-01-05T08:59:00.000Z"},
	}
}

func complaintEvent(sesID string, recipients ...string) map[string]any {
	rs := make([]map[string]string, 0, len(recipients))
	for _, r := range recipients {
		rs = append(rs, map[string]string{"emailAddress": r})
	}
	return map[string]any{
		"notificationType": "Complaint",
		"complaint": map[string]any{
			"complainedRecipients": rs, "complaintFeedbackType": "abuse", "timestamp": "2026-01-05T09:00:00.000Z",
		},
		"mail": map[string]any{"messageId": sesID, "destination": recipients},
	}
}

func notSpamEvent(sesID string, recipients ...string) map[string]any {
	ev := complaintEvent(sesID, recipients...)
	ev["complaint"].(map[string]any)["complaintFeedbackType"] = "not-spam"
	return ev
}

func deliveryEvent(sesID string, recipients ...string) map[string]any {
	return map[string]any{
		"eventType": "Delivery",
		"delivery": map[string]any{
			"timestamp": "2026-01-05T09:00:01.500Z", "recipients": recipients, "processingTimeMillis": 500,
		},
		"mail": map[string]any{"messageId": sesID, "destination": recipients},
	}
}

func rejectEvent(sesID string, recipients ...string) map[string]any {
	return map[string]any{
		"eventType": "Reject",
		"reject":    map[string]any{"reason": "Bad content"},
		"mail":      map[string]any{"messageId": sesID, "destination": recipients},
	}
}

var errBoom = errors.New("boom")
