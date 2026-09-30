package app_test

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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/contacts"
	"mailroom/internal/core"
	"mailroom/internal/events"
	"mailroom/internal/imports"
	"mailroom/internal/jobs"
	"mailroom/internal/testutil"
)

// SNS test identity: the topic the app is configured with (testVars) and
// the certificate URL the signed notifications name.
const (
	testRegion   = "eu-west-1"
	testTopicARN = "arn:aws:sns:eu-west-1:123456789012:mailroom-ses-events"
	snsCertURL   = "https://sns.eu-west-1.amazonaws.com/SimpleNotificationService-0123456789abcdef.pem"
	testBaseURL  = "https://newsletter.example.com"
)

// gatedProvider is the email provider of the harness: it sends through
// the FakeProvider and can call a function after the n-th successful send,
// which tests use to stop the sender part-way (a crash or shutdown).
type gatedProvider struct {
	*testutil.FakeProvider
	mu     sync.Mutex
	left   int
	onDone func()
}

var _ core.EmailProvider = (*gatedProvider)(nil)

// Send implements core.EmailProvider.
func (g *gatedProvider) Send(ctx context.Context, m core.OutgoingEmail) (string, error) {
	id, err := g.FakeProvider.Send(ctx, m)
	if err != nil {
		return id, err
	}
	var fire func()
	g.mu.Lock()
	if g.onDone != nil {
		if g.left--; g.left <= 0 {
			fire, g.onDone = g.onDone, nil
		}
	}
	g.mu.Unlock()
	if fire != nil {
		fire()
	}
	return id, nil
}

// after makes the n-th successful send from now call fn (nil clears it).
func (g *gatedProvider) after(n int, fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.left, g.onDone = n, fn
}

// sendSome runs the sender until exactly n more messages went out, then
// stops it the way a shutdown does (its context is cancelled; the message
// in flight completes). Fails unless n were sent.
func (hs *harness) sendSome(n int) {
	hs.t.Helper()
	ctx, cancel := context.WithCancel(hs.ctx)
	defer cancel()
	before := hs.provider.Count()
	hs.gate.after(n, cancel)
	_, err := hs.app.Sending.SendDue(ctx)
	hs.gate.after(0, nil)
	if err != nil && !errors.Is(err, context.Canceled) {
		hs.t.Fatalf("send: %v", err)
	}
	if got := hs.provider.Count() - before; got != n {
		hs.t.Fatalf("sent %d messages, want %d", got, n)
	}
}

// sendAll sends every due message and completes finished broadcasts.
func (hs *harness) sendAll() int {
	hs.t.Helper()
	n, err := hs.app.Sending.SendDue(hs.ctx)
	if err != nil {
		hs.t.Fatalf("send: %v", err)
	}
	return n
}

// schedule runs one sequences scheduler pass (the 60 s worker).
func (hs *harness) schedule() {
	hs.t.Helper()
	if _, err := hs.app.Sequences.RunDue(hs.ctx); err != nil {
		hs.t.Fatalf("scheduler: %v", err)
	}
}

// tick runs one broadcast-starter pass (the 10 s worker): scheduled
// starts, snapshot recovery and the circuit breaker.
func (hs *harness) tick() {
	hs.t.Helper()
	if err := hs.app.Broadcasts.Tick(hs.ctx); err != nil {
		hs.t.Fatalf("broadcast starter: %v", err)
	}
}

// ---------------------------------------------------------------- API helpers

// contactByEmail finds the contact with the address through the API.
func (hs *harness) contactByEmail(email string) (contacts.Contact, bool) {
	hs.t.Helper()
	var list struct{ Items []contacts.Contact }
	hs.api("GET", "/contacts?q="+url.QueryEscape(email), "").want(hs.t, http.StatusOK).decode(hs.t, &list)
	for _, c := range list.Items {
		if c.Email == email {
			return c, true
		}
	}
	return contacts.Contact{}, false
}

// mustContact is contactByEmail that fails when there is none.
func (hs *harness) mustContact(email string) contacts.Contact {
	hs.t.Helper()
	c, ok := hs.contactByEmail(email)
	if !ok {
		hs.t.Fatalf("no contact %s", email)
	}
	return c
}

// createContact creates a contact through the API.
func (hs *harness) createContact(body string) contacts.Contact {
	hs.t.Helper()
	var c contacts.Contact
	hs.api("POST", "/contacts", body).want(hs.t, http.StatusCreated).decode(hs.t, &c)
	return c
}

// contactStatus reads a contact's status through the API.
func (hs *harness) contactStatus(id int64) core.ContactStatus {
	hs.t.Helper()
	var c contacts.Contact
	hs.api("GET", fmt.Sprintf("/contacts/%d", id), "").want(hs.t, http.StatusOK).decode(hs.t, &c)
	return c.Status
}

// broadcast is the part of a broadcast the tests read.
type broadcast struct {
	ID              int64  `json:"id"`
	Status          string `json:"status"`
	PausedReason    string `json:"paused_reason"`
	RecipientsTotal int64  `json:"recipients_total"`
	Version         int64  `json:"version"`
}

// createBroadcast creates a broadcast with a unique subject to audience
// (a JSON audience object, e.g. {"type":"all"}).
func (hs *harness) createBroadcast(name, subject, audience string) broadcast {
	hs.t.Helper()
	var b broadcast
	body := fmt.Sprintf(`{"name":%q,"email":{"subject":%q,"body":"Hello {{first_name | \"there\"}}. [Our site](https://example.org/news)"},"audience":%s}`,
		name, subject, audience)
	hs.api("POST", "/broadcasts", body).want(hs.t, http.StatusCreated).decode(hs.t, &b)
	return b
}

// sendBroadcast starts a broadcast and waits for its snapshot job (the
// jobs runner must be running). It returns the recipients reported.
func (hs *harness) sendBroadcast(id int64) int64 {
	hs.t.Helper()
	var res struct {
		Recipients int64     `json:"recipients"`
		Job        *jobs.Job `json:"job"`
	}
	hs.api("POST", fmt.Sprintf("/broadcasts/%d/send", id), "").want(hs.t, http.StatusAccepted).decode(hs.t, &res)
	if res.Job == nil {
		hs.t.Fatal("send-broadcast returned no snapshot job")
	}
	hs.waitJob(res.Job.ID)
	return res.Recipients
}

// getBroadcast reads a broadcast through the API.
func (hs *harness) getBroadcast(id int64) broadcast {
	hs.t.Helper()
	var b broadcast
	hs.api("GET", fmt.Sprintf("/broadcasts/%d", id), "").want(hs.t, http.StatusOK).decode(hs.t, &b)
	return b
}

// importCSV posts a contacts CSV. With dryRun it returns the report of the
// 200 answer; otherwise it waits for the import job and returns the
// report of the finished import.
func (hs *harness) importCSV(csv string, dryRun bool) imports.ImportReport {
	hs.t.Helper()
	path := api.Prefix + "/imports"
	if dryRun {
		path += "?dry_run=true"
	}
	r := hs.raw("POST", path, "text/csv", csv, "Authorization", "Bearer "+hs.token)
	if dryRun {
		var res imports.ImportResult
		r.want(hs.t, http.StatusOK).decode(hs.t, &res)
		if res.Report == nil || res.Job != nil {
			hs.t.Fatalf("dry-run import = %s", r.body)
		}
		return *res.Report
	}
	var res imports.ImportResult
	r.want(hs.t, http.StatusAccepted).decode(hs.t, &res)
	if res.Job == nil {
		hs.t.Fatalf("import = %s", r.body)
	}
	hs.waitJob(res.Job.ID)
	var job imports.ImportJob
	hs.api("GET", fmt.Sprintf("/imports/%d", res.Job.ID), "").want(hs.t, http.StatusOK).decode(hs.t, &job)
	if job.Report == nil {
		hs.t.Fatalf("finished import has no report: %+v", job)
	}
	return *job.Report
}

// ---------------------------------------------------------------- sent mail

// sentMail is a message the fake provider accepted, parsed.
type sentMail struct {
	To     string
	Header mail.Header
	HTML   string
	Text   string
}

// mailTo returns the parsed messages sent to addr, in send order.
func (hs *harness) mailTo(addr string) []sentMail {
	hs.t.Helper()
	var out []sentMail
	for _, m := range hs.provider.SentTo(addr) {
		out = append(out, parseMail(hs.t, m))
	}
	return out
}

// subjectsTo returns the subjects of the messages sent to addr.
func (hs *harness) subjectsTo(addr string) []string {
	hs.t.Helper()
	var out []string
	for _, m := range hs.mailTo(addr) {
		out = append(out, m.Header.Get("Subject"))
	}
	return out
}

// parseMail decodes the raw MIME message: its headers and its HTML and
// text parts (quoted-printable decoded).
func parseMail(t *testing.T, m core.OutgoingEmail) sentMail {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(m.Raw))
	if err != nil {
		t.Fatalf("parse message to %s: %v", m.To, err)
	}
	out := sentMail{To: m.To, Header: msg.Header}
	dec := new(mime.WordDecoder)
	if s, err := dec.DecodeHeader(msg.Header.Get("Subject")); err == nil {
		out.Header["Subject"] = []string{s}
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		t.Fatalf("message to %s is %q, want multipart (%v)", m.To, mt, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("message to %s: %v", m.To, err)
		}
		var r io.Reader = p
		if strings.EqualFold(p.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			r = quotedprintable.NewReader(p)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("message to %s: %v", m.To, err)
		}
		switch pt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type")); pt {
		case "text/html":
			out.HTML = string(b)
		case "text/plain":
			out.Text = string(b)
		}
	}
	return out
}

var linkPattern = regexp.MustCompile(regexp.QuoteMeta(testBaseURL) + `(/[a-z]+(?:/[a-z]+)?/[A-Za-z0-9._~-]+)`)

// links returns the paths of this install's links under prefix (e.g.
// "/c/", "/f/confirm/") in the message's HTML and text, in order.
func (m sentMail) links(prefix string) []string {
	var out []string
	for _, match := range linkPattern.FindAllStringSubmatch(m.HTML+"\n"+m.Text, -1) {
		if strings.HasPrefix(match[1], prefix) {
			out = append(out, match[1])
		}
	}
	return out
}

// unsubscribePath returns the path of the List-Unsubscribe URL and
// checks the RFC 8058 headers of a marketing message.
func (m sentMail) unsubscribePath(t *testing.T) string {
	t.Helper()
	lu := m.Header.Get("List-Unsubscribe")
	if m.Header.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" {
		t.Fatalf("message to %s: List-Unsubscribe-Post = %q", m.To, m.Header.Get("List-Unsubscribe-Post"))
	}
	first := strings.Trim(strings.Split(lu, ",")[0], "<> ")
	u, err := url.Parse(first)
	if err != nil || !strings.HasPrefix(first, testBaseURL+"/u/") {
		t.Fatalf("message to %s: List-Unsubscribe = %q", m.To, lu)
	}
	return u.Path
}

// ---------------------------------------------------------------- SNS

// snsKey is the RSA key and self-signed certificate that sign the test
// SNS notifications (generated once: key generation is slow).
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

// certFetcher is the webhook's HTTP client: it serves the test signing
// certificate at snsCertURL and 404 for everything else.
type certFetcher struct {
	mu    sync.Mutex
	calls int
}

var _ events.HTTPDoer = (*certFetcher)(nil)

func newCertFetcher() *certFetcher { return &certFetcher{} }

// Do implements events.HTTPDoer.
func (f *certFetcher) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	resp := &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Request: req,
		Body: io.NopCloser(strings.NewReader("not found"))}
	if req.Method == http.MethodGet && req.URL.String() == snsCertURL {
		resp.StatusCode = http.StatusOK
		resp.Body = io.NopCloser(bytes.NewReader(snsKey().certPEM))
	}
	return resp, nil
}

// sesEvent is an SES event published to SNS (the configuration set's
// event destination), as JSON.
type sesEvent map[string]any

// sesMessageID returns the SES MessageId the sender stored for message.
func (hs *harness) sesMessageID(messageID int64) string {
	hs.t.Helper()
	var id string
	if err := hs.app.DB.Read.QueryRow(`SELECT ses_message_id FROM messages WHERE id = ?`, messageID).Scan(&id); err != nil {
		hs.t.Fatalf("ses_message_id of message %d: %v", messageID, err)
	}
	return id
}

// bounceEvent is a Bounce of bounceType ("Permanent" or "Transient").
func (hs *harness) bounceEvent(sesID, bounceType, recipient string) sesEvent {
	ts := hs.clock.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return sesEvent{
		"eventType": "Bounce",
		"bounce": map[string]any{
			"bounceType": bounceType, "bounceSubType": "General", "timestamp": ts, "feedbackId": "fb-" + sesID,
			"bouncedRecipients": []map[string]string{{"emailAddress": recipient, "action": "failed", "status": "5.1.1"}},
		},
		"mail": map[string]any{"messageId": sesID, "destination": []string{recipient}, "timestamp": ts},
	}
}

// complaintEvent is an abuse Complaint.
func (hs *harness) complaintEvent(sesID, recipient string) sesEvent {
	ts := hs.clock.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return sesEvent{
		"eventType": "Complaint",
		"complaint": map[string]any{
			"complaintFeedbackType": "abuse", "timestamp": ts,
			"complainedRecipients": []map[string]string{{"emailAddress": recipient}},
		},
		"mail": map[string]any{"messageId": sesID, "destination": []string{recipient}, "timestamp": ts},
	}
}

// deliveryEvent is a Delivery.
func (hs *harness) deliveryEvent(sesID, recipient string) sesEvent {
	ts := hs.clock.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return sesEvent{
		"eventType": "Delivery",
		"delivery":  map[string]any{"timestamp": ts, "recipients": []string{recipient}, "processingTimeMillis": 500},
		"mail":      map[string]any{"messageId": sesID, "destination": []string{recipient}, "timestamp": ts},
	}
}

// snsEnvelope wraps ev in an SNS Notification from the configured topic,
// signed with SignatureVersion version ("1": SHA1, "2": SHA256) as SNS
// does. snsID is the SNS MessageId (redeliveries repeat it). The mutators
// change the envelope before it is signed (e.g. another TopicArn), so the
// result is validly signed.
func (hs *harness) snsEnvelope(snsID string, ev sesEvent, version string, mutators ...func(map[string]any)) map[string]any {
	hs.t.Helper()
	msg, err := json.Marshal(ev)
	if err != nil {
		hs.t.Fatal(err)
	}
	e := map[string]any{
		"Type":             events.TypeNotification,
		"MessageId":        snsID,
		"TopicArn":         testTopicARN,
		"Message":          string(msg),
		"Timestamp":        hs.clock.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"SignatureVersion": version,
		"SigningCertURL":   snsCertURL,
		"UnsubscribeURL":   "https://sns.eu-west-1.amazonaws.com/?Action=Unsubscribe&SubscriptionArn=" + testTopicARN + ":sub",
	}
	for _, m := range mutators {
		m(e)
	}
	// The canonical string of a Notification (Subject is absent here).
	var b bytes.Buffer
	for _, k := range []string{"Message", "MessageId", "Timestamp", "TopicArn", "Type"} {
		fmt.Fprintf(&b, "%s\n%s\n", k, e[k])
	}
	var (
		hash crypto.Hash
		sum  []byte
	)
	if version == "1" {
		s := sha1.Sum(b.Bytes())
		hash, sum = crypto.SHA1, s[:]
	} else {
		s := sha256.Sum256(b.Bytes())
		hash, sum = crypto.SHA256, s[:]
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, snsKey().key, hash, sum)
	if err != nil {
		hs.t.Fatal(err)
	}
	e["Signature"] = base64.StdEncoding.EncodeToString(sig)
	return e
}

// postSNS posts an SNS envelope to the public webhook, as SNS does.
func (hs *harness) postSNS(e map[string]any) resp {
	hs.t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		hs.t.Fatal(err)
	}
	typ, _ := e["Type"].(string)
	return hs.raw("POST", "/hooks/ses", "text/plain; charset=UTF-8", string(b), "x-amz-sns-message-type", typ)
}

// notify signs ev as SNS notification snsID and posts it; it must be
// accepted.
func (hs *harness) notify(snsID string, ev sesEvent, version string) {
	hs.t.Helper()
	hs.postSNS(hs.snsEnvelope(snsID, ev, version)).want(hs.t, http.StatusOK)
}

// ---------------------------------------------------------------- database

// tablesContaining returns "table.column" for every column of every table
// whose value contains needle (case-insensitively, as text).
func (hs *harness) tablesContaining(needle string) []string {
	hs.t.Helper()
	rd := hs.app.DB.Read
	rows, err := rd.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		hs.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			hs.t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	var found []string
	for _, table := range tables {
		cols, err := rd.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			hs.t.Fatal(err)
		}
		var names []string
		for cols.Next() {
			var n string
			if err := cols.Scan(&n); err != nil {
				hs.t.Fatal(err)
			}
			names = append(names, n)
		}
		cols.Close()
		for _, col := range names {
			var n int
			q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE instr(lower(CAST(%q AS TEXT)), lower(?)) > 0`, table, col)
			if err := rd.QueryRow(q, needle).Scan(&n); err != nil {
				hs.t.Fatalf("scan %s.%s: %v", table, col, err)
			}
			if n > 0 {
				found = append(found, table+"."+col)
			}
		}
	}
	return found
}

// messageIDs returns the IDs of the contact's messages of kind, oldest
// first.
func (hs *harness) messageIDs(contactID int64, kind core.MessageKind) []int64 {
	hs.t.Helper()
	rows, err := hs.app.DB.Read.Query(`SELECT id FROM messages WHERE contact_id = ? AND kind = ? ORDER BY id`, contactID, kind)
	if err != nil {
		hs.t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			hs.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// day is a day of fake time.
const day = 24 * time.Hour
