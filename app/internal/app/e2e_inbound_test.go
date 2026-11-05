package app_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"mailroom/internal/app"
	"mailroom/internal/contacts"
	"mailroom/internal/inbound"
)

const testInboundBucket = "mailroom-inbound-123456789012-eu-west-1"

// fakeS3 holds the objects SES stored in the inbound bucket.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "no such key"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	delete(f.objects, k)
	f.deleted = append(f.deleted, k)
	return &s3.DeleteObjectOutput{}, nil
}

// receivedEvent is an SES receipt notification for an S3 action.
func (hs *harness) receivedEvent(key, from, subject string) sesEvent {
	ts := hs.clock.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return sesEvent{
		"notificationType": "Received",
		"mail": map[string]any{
			"timestamp": ts, "source": from, "messageId": "ses-" + key, "destination": []string{"hello@example.com"},
			"commonHeaders": map[string]any{"from": []string{"Anna Jensen <" + from + ">"}, "to": []string{"hello@example.com"},
				"subject": subject, "messageId": "<orig-1@mail.example.org>"},
		},
		"receipt": map[string]any{
			"timestamp": ts, "recipients": []string{"hello@example.com"},
			"spamVerdict": map[string]string{"status": "PASS"}, "virusVerdict": map[string]string{"status": "PASS"},
			"spfVerdict": map[string]string{"status": "PASS"}, "dkimVerdict": map[string]string{"status": "PASS"},
			"dmarcVerdict": map[string]string{"status": "PASS"},
			"action":       map[string]any{"type": "S3", "topicArn": testTopicARN, "bucketName": testInboundBucket, "objectKeyPrefix": "inbound/", "objectKey": key},
		},
	}
}

const rawReceived = "From: Anna Jensen <Anna@Example.org>\r\n" +
	"To: hello@example.com\r\n" +
	"Subject: =?UTF-8?Q?Hej_med_dig_=C3=B8?=\r\n" +
	"Message-ID: <orig-1@mail.example.org>\r\n" +
	"References: <older@mail.example.org>\r\n" +
	"Date: Tue, 03 Nov 2026 10:00:00 +0100\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=ISO-8859-1\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"Hej! Jeg vil gerne h=F8re mere.\r\n\r\n> old quote\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
	"<p>Hej! Jeg vil gerne h\xc3\xb8re mere.</p><img src=\"https://tracker.example.net/p.gif\"><script>alert(1)</script>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"tilbud.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"../../tilbud.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\n" +
	"JVBERi0xLjQK\r\nJSVFT0YK\r\n" +
	"--outer--\r\n"

func newInboundHarness(t *testing.T) (*harness, *fakeS3) {
	t.Helper()
	s3c := &fakeS3{objects: map[string][]byte{}}
	hs := newHarness(t, func(o *app.Options) { o.AWS = &app.AWSClients{Inbound: s3c} })
	hs.vars["INBOUND_BUCKET"] = testInboundBucket
	hs.vars["INBOUND_DOMAIN"] = "example.com"
	hs.restart()
	return hs, s3c
}

func TestE2EInboundReceiveAndReply(t *testing.T) {
	hs, s3c := newInboundHarness(t)
	if !hs.app.Inbound.Enabled() || !slicesContains(workerNames(hs.app), "inbound-fetcher") {
		t.Fatalf("receiving not wired: workers %v", workerNames(hs.app))
	}
	anna := hs.createContact(`{"email": "anna@example.org", "first_name": "Anna", "tags": ["customer"]}`)

	key := "inbound/abc123"
	s3c.objects[testInboundBucket+"/"+key] = []byte(rawReceived)
	hs.notify("sns-r1", hs.receivedEvent(key, "anna@example.org", "Hej med dig ø"), "2")
	hs.notify("sns-r1", hs.receivedEvent(key, "anna@example.org", "Hej med dig ø"), "2") // redelivery
	if n := hs.count(`SELECT count(*) FROM inbound_messages WHERE status = 'pending'`); n != 1 {
		t.Fatalf("pending = %d", n)
	}
	// Before the fetch the list shows the notification's headers.
	var list struct{ Items []inbound.Summary }
	hs.api("GET", "/inbound", "").want(t, http.StatusOK).decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].Status != "pending" || list.Items[0].Subject != "Hej med dig ø" {
		t.Fatalf("list = %+v", list.Items)
	}

	res, err := hs.app.Inbound.FetchDue(hs.ctx)
	if err != nil || res.Received != 1 {
		t.Fatalf("FetchDue = %+v, %v", res, err)
	}
	if len(s3c.objects) != 0 || len(s3c.deleted) != 1 {
		t.Errorf("S3 copy not deleted: %v", s3c.deleted)
	}

	var counts inbound.Counts
	hs.api("GET", "/inbound/counts", "").want(t, http.StatusOK).decode(t, &counts)
	if !counts.Enabled || counts.Unread != 1 || counts.Inbox != 1 || counts.Domain != "example.com" {
		t.Errorf("counts = %+v", counts)
	}
	hs.api("GET", "/inbound?box=unread", "").want(t, http.StatusOK).decode(t, &list)
	if len(list.Items) != 1 {
		t.Fatalf("unread = %+v", list.Items)
	}
	id := list.Items[0].ID
	var m inbound.Message
	r := hs.api("GET", fmt.Sprintf("/inbound/%d", id), "").want(t, http.StatusOK)
	r.decode(t, &m)
	if m.ContactID == nil || *m.ContactID != anna.ID || m.FromEmail != "anna@example.org" || m.FromName != "Anna Jensen" ||
		!strings.Contains(m.Text, "høre mere") || !strings.Contains(m.HTML, "<script>") || m.Verdicts.DKIM != "PASS" ||
		len(m.AttachmentsList) != 1 || m.AttachmentsList[0].Filename != "tilbud.pdf" || m.MessageIDHeader != "orig-1@mail.example.org" {
		t.Fatalf("message = %+v", m)
	}
	if m.Snippet != "Hej! Jeg vil gerne høre mere." {
		t.Errorf("snippet = %q", m.Snippet)
	}
	att := hs.api("GET", fmt.Sprintf("/inbound/%d/attachments/1", id), "").want(t, http.StatusOK)
	if string(att.body) != "%PDF-1.4\n%%EOF\n" || !strings.Contains(att.header.Get("Content-Disposition"), "attachment") {
		t.Errorf("attachment = %q %v", string(att.body), att.header)
	}
	d := hs.app.Inbound.DisplayHTML(hs.ctx, m, false)
	if strings.Contains(d.HTML, "<script") || strings.Contains(d.HTML, "tracker.example.net") || d.BlockedImages != 1 {
		t.Errorf("display = %+v", d)
	}

	// The contact's timeline shows the email.
	var tl contacts.TimelinePage
	hs.api("GET", fmt.Sprintf("/contacts/%d/timeline", anna.ID), "").want(t, http.StatusOK).decode(t, &tl)
	if len(tl.Items) == 0 || tl.Items[0].Type != contacts.TimelineReceived {
		t.Errorf("timeline = %+v", tl.Items)
	}

	// Reply: a dry run queues nothing; the reply is threaded and plain.
	var rr inbound.ReplyResult
	hs.api("POST", fmt.Sprintf("/inbound/%d/reply?dry_run=true", id), `{"body": "Hej Anna,\n\nTak! Vi ringer.\n\nBen"}`).
		want(t, http.StatusOK).decode(t, &rr)
	if rr.MessageID != 0 || rr.To != "anna@example.org" || rr.Subject != "Re: Hej med dig ø" {
		t.Errorf("dry run = %+v", rr)
	}
	hs.api("POST", fmt.Sprintf("/inbound/%d/reply", id), `{"body": "Hej Anna,\n\nTak! Vi ringer.\n\nBen"}`, "Idempotency-Key", "reply-1").
		want(t, http.StatusAccepted).decode(t, &rr)
	if rr.MessageID == 0 {
		t.Fatalf("reply = %+v", rr)
	}
	hs.sendAll()
	sent := hs.mailTo("anna@example.org")
	if len(sent) != 1 {
		t.Fatalf("sent = %d", len(sent))
	}
	h := sent[0].Header
	if h.Get("In-Reply-To") != "<orig-1@mail.example.org>" || h.Get("References") != "<older@mail.example.org> <orig-1@mail.example.org>" ||
		h.Get("List-Unsubscribe") != "" || h.Get("Subject") != "Re: Hej med dig ø" || h.Get("Reply-To") != "<hello@example.com>" {
		t.Errorf("reply headers = %v", h)
	}
	if !strings.Contains(sent[0].Text, "Tak! Vi ringer.") || !strings.Contains(sent[0].Text, "> Hej! Jeg vil gerne høre mere.") ||
		strings.Contains(sent[0].HTML, "/u/") {
		t.Errorf("reply text = %q", sent[0].Text)
	}
	hs.api("GET", fmt.Sprintf("/inbound/%d", id), "").want(t, http.StatusOK).decode(t, &m)
	if !m.Read || len(m.RepliesSent) != 1 || m.RepliesSent[0].Status != "sent" || m.Replies != 1 {
		t.Errorf("after reply: read %v replies %+v", m.Read, m.RepliesSent)
	}

	// Archive, then a bounced contact can't be answered.
	hs.api("PATCH", fmt.Sprintf("/inbound/%d", id), `{"archived": true}`).want(t, http.StatusOK)
	hs.api("GET", "/inbound", "").want(t, http.StatusOK).decode(t, &list)
	if len(list.Items) != 0 {
		t.Errorf("inbox after archive = %+v", list.Items)
	}
	hs.app.DB.Write.Exec(`UPDATE contacts SET status = 'bounced' WHERE id = ?`, anna.ID)
	hs.api("POST", fmt.Sprintf("/inbound/%d/reply", id), `{"body": "again"}`).want(t, http.StatusConflict)

	// Erasing the contact deletes the email and its attachment.
	var er contacts.EraseResult
	var ex contacts.Export
	hs.api("GET", fmt.Sprintf("/contacts/%d/export", anna.ID), "").want(t, http.StatusOK).decode(t, &ex)
	if len(ex.Received) != 1 || ex.Received[0].Attachments[0] != "tilbud.pdf" {
		t.Errorf("export received = %+v", ex.Received)
	}
	hs.api("DELETE", fmt.Sprintf("/contacts/%d", anna.ID), "").want(t, http.StatusOK).decode(t, &er)
	if er.Deleted.InboundMessages != 1 || hs.count(`SELECT count(*) FROM inbound_messages`) != 0 ||
		hs.count(`SELECT count(*) FROM inbound_attachments`) != 0 {
		t.Errorf("erase counts = %+v", er.Deleted)
	}
}

func TestE2EInboundFetchFailures(t *testing.T) {
	hs, _ := newInboundHarness(t)
	// The object is missing: a permanent failure.
	hs.notify("sns-x", hs.receivedEvent("inbound/missing", "x@example.org", "Hi"), "2")
	// Another bucket is ignored.
	ev := hs.receivedEvent("inbound/other", "x@example.org", "Hi")
	ev["receipt"].(map[string]any)["action"].(map[string]any)["bucketName"] = "someone-elses"
	hs.notify("sns-y", ev, "2")
	if n := hs.count(`SELECT count(*) FROM inbound_messages`); n != 1 {
		t.Fatalf("rows = %d", n)
	}
	res, err := hs.app.Inbound.FetchDue(hs.ctx)
	if err != nil || res.Failed != 1 {
		t.Fatalf("FetchDue = %+v %v", res, err)
	}
	var c inbound.Counts
	hs.api("GET", "/inbound/counts", "").want(t, http.StatusOK).decode(t, &c)
	if c.Failed != 1 {
		t.Errorf("counts = %+v", c)
	}
	// Malformed receipt notifications are refused.
	hs.postSNS(hs.snsEnvelope("sns-z", sesEvent{"notificationType": "Received", "receipt": "nope"}, "2")).want(t, http.StatusBadRequest)
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
