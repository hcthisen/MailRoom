package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestE2ETestDraft: the editor's Send test works before the content is
// saved (send-test-draft). The sender renders the content from the
// message data with the chosen contact's data and the broadcast-style
// sender override, and the test has no working unsubscribe link.
func TestE2ETestDraft(t *testing.T) {
	hs := newHarness(t, nil)
	ann := hs.createContact(`{"email":"ann@example.org","first_name":"Ann","attributes":{"company":"Acme"}}`)

	var fields struct {
		Items []struct{ Name string } `json:"items"`
	}
	hs.api("GET", "/emails/merge-fields", "").want(t, http.StatusOK).decode(t, &fields)
	var names []string
	for _, f := range fields.Items {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "first_name,last_name,email,attr.company,unsubscribe_url" {
		t.Fatalf("merge fields = %s", got)
	}

	body := fmt.Sprintf(`{"subject":"Hi {{first_name | \"there\"}}","format":"html",
		"body":"<!-- mailroom:visual -->\n<p>News for {{attr.company}}</p>","to":["tester@example.org"],"contact_id":%d,"from_name":"Ann's shop"}`, ann.ID)
	hs.api("POST", "/emails/test", body).want(t, http.StatusAccepted)
	hs.api("POST", "/emails/test", `{"subject":"x","body":"{{nope}}"}`).want(t, http.StatusUnprocessableEntity)
	hs.sendAll()

	got := hs.mailTo("tester@example.org")
	if len(got) != 1 {
		t.Fatalf("%d mails to the tester", len(got))
	}
	m := got[0]
	if m.Header.Get("Subject") != "Hi Ann" || !strings.Contains(m.HTML, "News for Acme") || !strings.Contains(m.Header.Get("From"), "Ann's shop") {
		t.Fatalf("subject %q from %q:\n%s", m.Header.Get("Subject"), m.Header.Get("From"), m.HTML)
	}
	if strings.Contains(m.HTML, "/u/") {
		t.Fatal("a test must not carry a working unsubscribe link")
	}
}
