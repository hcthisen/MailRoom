package events_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"mailroom/internal/events"
)

func TestSNSSignatureVersions(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		t.Run("v"+version, func(t *testing.T) {
			h := newHarness(t)
			msg := sesJSON(t, deliveryEvent("ses-unknown", "someone@example.net"))
			rec := h.postSNS(snsMessage(t, events.TypeNotification, "sns-"+version, msg, version))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}
			if n := h.count(`SELECT COUNT(*) FROM events WHERE type = 'delivery'`); n != 1 {
				t.Fatalf("delivery events = %d, want 1", n)
			}
		})
	}
}

func TestSNSSubjectIsSigned(t *testing.T) {
	h := newHarness(t)
	msg := sesJSON(t, deliveryEvent("ses-x", "someone@example.net"))
	withSubject := func(e map[string]any) { e["Subject"] = "Amazon SES Email Event Notification" }
	if rec := h.postSNS(snsMessage(t, events.TypeNotification, "sns-subj", msg, "2", withSubject)); rec.Code != http.StatusOK {
		t.Fatalf("signed subject: status = %d (%s)", rec.Code, rec.Body)
	}
	e := snsMessage(t, events.TypeNotification, "sns-subj-2", msg, "2", withSubject)
	e["Subject"] = "changed"
	if rec := h.postSNS(e); rec.Code != http.StatusForbidden {
		t.Fatalf("altered subject: status = %d, want 403", rec.Code)
	}
}

func TestSNSRejects(t *testing.T) {
	msg := func(t *testing.T) string { return sesJSON(t, deliveryEvent("ses-1", "a@example.net")) }
	tests := []struct {
		name  string
		build func(t *testing.T, h *harness) *httptestResult
		want  int
	}{
		{name: "wrong topic", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "2", func(e map[string]any) {
				e["TopicArn"] = "arn:aws:sns:eu-west-1:999999999999:other"
			}))
		}},
		{name: "tampered message", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			e := snsMessage(t, events.TypeNotification, "m1", msg(t), "2")
			e["Message"] = sesJSON(t, bounceEvent("ses-1", "Permanent", "a@example.net"))
			return post(h, e)
		}},
		{name: "signature not base64", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			e := snsMessage(t, events.TypeNotification, "m1", msg(t), "2")
			e["Signature"] = "!!!"
			return post(h, e)
		}},
		{name: "v1 signature presented as v2", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			e := snsMessage(t, events.TypeNotification, "m1", msg(t), "1")
			e["SignatureVersion"] = "2"
			return post(h, e)
		}},
		{name: "unsupported signature version", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "3"))
		}},
		{name: "cert host not sns", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			const evil = "https://evil.example.com/SimpleNotificationService.pem"
			h.http.set(evil, fakeResponse{status: http.StatusOK, body: string(snsKey().certPEM)})
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "2", func(e map[string]any) { e["SigningCertURL"] = evil }))
		}},
		{name: "cert host suffix trick", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			const evil = "https://sns.eu-west-1.amazonaws.com.evil.com/cert.pem"
			h.http.set(evil, fakeResponse{status: http.StatusOK, body: string(snsKey().certPEM)})
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "2", func(e map[string]any) { e["SigningCertURL"] = evil }))
		}},
		{name: "cert over http", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			const u = "http://sns.eu-west-1.amazonaws.com/cert.pem"
			h.http.set(u, fakeResponse{status: http.StatusOK, body: string(snsKey().certPEM)})
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "2", func(e map[string]any) { e["SigningCertURL"] = u }))
		}},
		{name: "cert path not pem", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			const u = "https://sns.eu-west-1.amazonaws.com/cert.txt"
			h.http.set(u, fakeResponse{status: http.StatusOK, body: string(snsKey().certPEM)})
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "2", func(e map[string]any) { e["SigningCertURL"] = u }))
		}},
		{name: "cert not a certificate", want: http.StatusForbidden, build: func(t *testing.T, h *harness) *httptestResult {
			const u = "https://sns.eu-west-1.amazonaws.com/broken.pem"
			h.http.set(u, fakeResponse{status: http.StatusOK, body: "not a pem"})
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "2", func(e map[string]any) { e["SigningCertURL"] = u }))
		}},
		{name: "cert fetch fails", want: http.StatusServiceUnavailable, build: func(t *testing.T, h *harness) *httptestResult {
			const u = "https://sns.eu-west-1.amazonaws.com/down.pem"
			h.http.set(u, fakeResponse{err: errBoom})
			return post(h, snsMessage(t, events.TypeNotification, "m1", msg(t), "2", func(e map[string]any) { e["SigningCertURL"] = u }))
		}},
		{name: "not json", want: http.StatusBadRequest, build: func(t *testing.T, h *harness) *httptestResult {
			return wrap(h.do(http.MethodPost, "/hooks/ses", "text/plain", "{not json", "x-amz-sns-message-type", "Notification"))
		}},
		{name: "missing fields", want: http.StatusBadRequest, build: func(t *testing.T, h *harness) *httptestResult {
			return wrap(h.do(http.MethodPost, "/hooks/ses", "text/plain", `{"Type":"Notification","TopicArn":"`+testTopicARN+`"}`))
		}},
		{name: "unknown type", want: http.StatusBadRequest, build: func(t *testing.T, h *harness) *httptestResult {
			return post(h, snsMessage(t, "Surprise", "m1", msg(t), "2"))
		}},
		{name: "header disagrees with body", want: http.StatusBadRequest, build: func(t *testing.T, h *harness) *httptestResult {
			e := snsMessage(t, events.TypeNotification, "m1", msg(t), "2")
			b := mustJSON(t, e)
			return wrap(h.do(http.MethodPost, "/hooks/ses", "text/plain", b, "x-amz-sns-message-type", "SubscriptionConfirmation"))
		}},
		{name: "SES event not json", want: http.StatusBadRequest, build: func(t *testing.T, h *harness) *httptestResult {
			return post(h, snsMessage(t, events.TypeNotification, "m1", "not json", "2"))
		}},
		{name: "bounce without messageId", want: http.StatusBadRequest, build: func(t *testing.T, h *harness) *httptestResult {
			return post(h, snsMessage(t, events.TypeNotification, "m1", sesJSON(t, bounceEvent("", "Permanent", "a@example.net")), "2"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			res := tt.build(t, h)
			if res.code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", res.code, tt.want, res.body)
			}
			if n := h.count(`SELECT COUNT(*) FROM events`); n != 0 {
				t.Fatalf("events written for a rejected message: %d", n)
			}
			if n := h.http.calls("GET https://evil") + h.http.calls("GET https://sns.eu-west-1.amazonaws.com.evil") +
				h.http.calls("GET http://") + h.http.calls("GET https://sns.eu-west-1.amazonaws.com/cert.txt"); n != 0 {
				t.Fatalf("fetched a certificate from a rejected URL (%d)", n)
			}
		})
	}
}

func TestSNSCertificateMustBeFromTopicRegion(t *testing.T) {
	h := newHarness(t)
	const other = "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-1.pem"
	h.http.set(other, fakeResponse{status: http.StatusOK, body: string(snsKey().certPEM)})
	rec := h.postSNS(snsMessage(t, events.TypeNotification, "m1", sesJSON(t, deliveryEvent("s", "a@example.net")), "2",
		func(e map[string]any) { e["SigningCertURL"] = other }))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if n := h.http.calls("GET " + other); n != 0 {
		t.Fatalf("fetched a certificate of another region (%d)", n)
	}
}

func TestSNSFailedCertificateURLIsNotRefetched(t *testing.T) {
	h := newHarness(t)
	const missing = "https://sns.eu-west-1.amazonaws.com/does-not-exist.pem"
	for i := range 5 {
		rec := h.postSNS(snsMessage(t, events.TypeNotification, fmt.Sprintf("m%d", i), sesJSON(t, deliveryEvent("s", "a@example.net")), "2",
			func(e map[string]any) { e["SigningCertURL"] = missing }))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	}
	if n := h.http.calls("GET " + missing); n != 1 {
		t.Fatalf("a failing certificate URL was fetched %d times, want 1", n)
	}
	// After badCertTTL it is tried again.
	h.clock.Advance(11 * time.Minute)
	h.postSNS(snsMessage(t, events.TypeNotification, "m9", sesJSON(t, deliveryEvent("s", "a@example.net")), "2",
		func(e map[string]any) { e["SigningCertURL"] = missing }))
	if n := h.http.calls("GET " + missing); n != 2 {
		t.Fatalf("fetches after the refusal expired = %d, want 2", n)
	}
	// A transient failure (5xx) is not remembered.
	const flaky = "https://sns.eu-west-1.amazonaws.com/flaky.pem"
	h.http.set(flaky, fakeResponse{status: http.StatusServiceUnavailable})
	for i := range 2 {
		rec := h.postSNS(snsMessage(t, events.TypeNotification, fmt.Sprintf("f%d", i), sesJSON(t, deliveryEvent("s", "a@example.net")), "2",
			func(e map[string]any) { e["SigningCertURL"] = flaky }))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	}
	if n := h.http.calls("GET " + flaky); n != 2 {
		t.Fatalf("flaky certificate fetched %d times, want 2", n)
	}
}

func TestSNSUnconfiguredTopicRejectsEverything(t *testing.T) {
	h := newHarness(t, func(d *events.Deps) { d.Config.SNSTopicARN = "" })
	rec := h.postSNS(snsMessage(t, events.TypeNotification, "m1", sesJSON(t, deliveryEvent("s", "a@example.net")), "2"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSNSCertificateIsCached(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"m1", "m2", "m3"} {
		rec := h.postSNS(snsMessage(t, events.TypeNotification, id, sesJSON(t, deliveryEvent("s-"+id, "a@example.net")), "2"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
		}
	}
	if n := h.http.calls("GET " + testCertURL); n != 1 {
		t.Fatalf("certificate fetched %d times, want 1", n)
	}
}

func TestSNSIgnoredEventTypes(t *testing.T) {
	h := newHarness(t)
	for i, m := range []string{
		`{"eventType":"Open","mail":{"messageId":"x"},"open":{}}`,
		`{"eventType":"Send","mail":{"messageId":"x"},"send":{}}`,
		`{"notificationType":"AmazonSnsSubscriptionSucceeded","message":"You have successfully subscribed"}`,
	} {
		rec := h.postSNS(snsMessage(t, events.TypeNotification, "ign-"+string(rune('a'+i)), m, "2"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", m, rec.Code, rec.Body)
		}
	}
	if n := h.count(`SELECT COUNT(*) FROM events`); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}
}

func TestSubscriptionConfirmation(t *testing.T) {
	good := "https://sns.eu-west-1.amazonaws.com/?Action=ConfirmSubscription&TopicArn=" + testTopicARN + "&Token=abc"
	tests := []struct {
		name         string
		subscribeURL string
		respond      *fakeResponse
		want         int
		wantFetch    bool
	}{
		{name: "confirmed", subscribeURL: good, respond: &fakeResponse{status: 200, body: "<ConfirmSubscriptionResponse/>"}, want: 200, wantFetch: true},
		{name: "confirmation endpoint fails", subscribeURL: good, respond: &fakeResponse{status: 500}, want: http.StatusBadGateway, wantFetch: true},
		{name: "foreign host", subscribeURL: "https://evil.example.com/?Action=ConfirmSubscription", want: 403},
		{name: "metadata address", subscribeURL: "https://169.254.169.254/latest/meta-data/", want: 403},
		{name: "host suffix trick", subscribeURL: "https://sns.eu-west-1.amazonaws.com.evil.com/?Action=ConfirmSubscription", want: 403},
		{name: "other region", subscribeURL: "https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription", want: 403},
		{name: "plain http", subscribeURL: "http://sns.eu-west-1.amazonaws.com/?Action=ConfirmSubscription", want: 403},
		{name: "explicit port", subscribeURL: "https://sns.eu-west-1.amazonaws.com:8443/?Action=ConfirmSubscription", want: 403},
		{name: "credentials", subscribeURL: "https://user:pw@sns.eu-west-1.amazonaws.com/?Action=ConfirmSubscription", want: 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.respond != nil {
				h.http.set(tt.subscribeURL, *tt.respond)
			} else {
				h.http.set(tt.subscribeURL, fakeResponse{status: 200})
			}
			e := snsMessage(t, events.TypeSubscriptionConfirmation, "conf-1", "You have chosen to subscribe to the topic", "1",
				func(e map[string]any) { e["SubscribeURL"] = tt.subscribeURL })
			rec := h.postSNS(e)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
			fetched := h.http.calls("GET "+tt.subscribeURL) > 0
			if fetched != tt.wantFetch {
				t.Fatalf("SubscribeURL fetched = %v, want %v", fetched, tt.wantFetch)
			}
		})
	}
}

func TestSubscriptionConfirmationForeignTopicNotFetched(t *testing.T) {
	h := newHarness(t)
	foreign := "https://sns.eu-west-1.amazonaws.com/?Action=ConfirmSubscription&TopicArn=arn:aws:sns:eu-west-1:1:x&Token=t"
	h.http.set(foreign, fakeResponse{status: 200})
	e := snsMessage(t, events.TypeSubscriptionConfirmation, "conf-2", "subscribe", "2", func(e map[string]any) {
		e["TopicArn"] = "arn:aws:sns:eu-west-1:1:x"
		e["SubscribeURL"] = foreign
	})
	if rec := h.postSNS(e); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if h.http.calls("GET "+foreign) != 0 || h.http.calls("GET "+testCertURL) != 0 {
		t.Fatal("a confirmation for a foreign topic caused a fetch")
	}
}

func TestUnsubscribeConfirmationAlerts(t *testing.T) {
	h := newHarness(t)
	e := snsMessage(t, events.TypeUnsubscribeConfirmation, "unsub-1", "You have unsubscribed", "2")
	rec := h.postSNS(e)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if got := h.alerts.all(); len(got) != 1 || got[0] != events.AlertSNSUnsubscribed {
		t.Fatalf("alerts = %v", got)
	}
	if h.http.calls("GET https://sns.eu-west-1.amazonaws.com/?Action") != 0 {
		t.Fatal("UnsubscribeConfirmation must not fetch its SubscribeURL")
	}
}

func TestStringToSign(t *testing.T) {
	subject := "Hello"
	n := events.StringToSign(events.Envelope{
		Type: "Notification", MessageID: "id", TopicArn: "arn", Subject: &subject, Message: "m", Timestamp: "ts",
		UnsubscribeURL: "ignored", SigningCertURL: "ignored",
	})
	if want := "Message\nm\nMessageId\nid\nSubject\nHello\nTimestamp\nts\nTopicArn\narn\nType\nNotification\n"; n != want {
		t.Fatalf("notification string = %q", n)
	}
	c := events.StringToSign(events.Envelope{
		Type: "SubscriptionConfirmation", MessageID: "id", TopicArn: "arn", Message: "m", Timestamp: "ts", Token: "tok", SubscribeURL: "url",
	})
	if want := "Message\nm\nMessageId\nid\nSubscribeURL\nurl\nTimestamp\nts\nToken\ntok\nTopicArn\narn\nType\nSubscriptionConfirmation\n"; c != want {
		t.Fatalf("confirmation string = %q", c)
	}
}

func TestValidCertURL(t *testing.T) {
	tests := map[string]bool{
		"https://sns.eu-west-1.amazonaws.com/SimpleNotificationService-1.pem":   true,
		"https://sns.cn-north-1.amazonaws.com.cn/SimpleNotificationService.pem": true,
		"https://sns.us-gov-west-1.amazonaws.com/a.pem":                         true,
		"https://sns.eu-west-1.amazonaws.com/a.pem?x=1":                         false,
		"https://sns.eu-west-1.amazonaws.com:443/a.pem":                         false,
		"https://SNS.EU-WEST-1.amazonaws.com/a.pem":                             false,
		"https://s3.amazonaws.com/a.pem":                                        false,
		"https://sns.amazonaws.com/a.pem":                                       false,
		"https://evil.com/sns.eu-west-1.amazonaws.com/a.pem":                    false,
		"https://sns.eu-west-1.amazonaws.com@evil.com/a.pem":                    false,
		"ftp://sns.eu-west-1.amazonaws.com/a.pem":                               false,
		"": false,
	}
	for u, want := range tests {
		if got := events.ValidCertURL(u); got != want {
			t.Errorf("ValidCertURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// httptestResult is a status and body, for table tests.
type httptestResult struct {
	code int
	body string
}

func post(h *harness, e map[string]any) *httptestResult { return wrap(h.postSNS(e)) }

func wrap(rec interface {
	Result() *http.Response
}) *httptestResult {
	res := rec.Result()
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 512)
	n, _ := res.Body.Read(buf)
	b.Write(buf[:n])
	return &httptestResult{code: res.StatusCode, body: b.String()}
}

func mustJSON(t *testing.T, v any) string { return sesJSON(t, v) }
