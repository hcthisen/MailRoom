package content

import (
	"errors"
	"strings"
	"testing"

	"mailroom/internal/core"
)

func TestParseMerge(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string // substring of the error; "" = valid
	}{
		{"plain text", "Hello world", ""},
		{"empty", "", ""},
		{"first name", "Hi {{first_name}}!", ""},
		{"spaces", "Hi {{  first_name  }}!", ""},
		{"fallback", `Hi {{first_name | "there"}}!`, ""},
		{"fallback no spaces", `Hi {{first_name|"there"}}!`, ""},
		{"fallback escapes", `{{first_name | "say \"hi\" \\ o/"}}`, ""},
		{"empty fallback", `{{last_name | ""}}`, ""},
		{"email", "{{email}}", ""},
		{"attr", "{{attr.company}} {{attr.plan_2}}", ""},
		{"unsubscribe", `<a href="{{unsubscribe_url}}">x</a>`, ""},
		{"single braces ignored", "{ first_name } and }} alone", ""},
		{"unknown", "{{nickname}}", `unknown merge field "nickname"`},
		{"ghl syntax", "{{contact.first_name}}", "GoHighLevel"},
		{"bad attr key", "{{attr.my-key}}", "invalid merge field"},
		{"long attr key", "{{attr." + strings.Repeat("k", 65) + "}}", "attribute key"},
		{"empty attr key", "{{attr.}}", "attribute key"},
		{"empty field", "{{ }}", "empty merge field"},
		{"unclosed", "Hi {{first_name", "unclosed"},
		{"fallback unquoted", "{{first_name | there}}", "invalid fallback"},
		{"fallback single quotes", "{{first_name | 'there'}}", "invalid fallback"},
		{"fallback stray quote", `{{first_name | "a"b"}}`, "invalid fallback"},
		{"fallback bad escape", `{{first_name | "a\n"}}`, "invalid fallback"},
		{"template syntax", "{{ .FirstName }}", "unknown merge field"},
		{"function call", "{{printf \"%s\" first_name}}", "invalid merge field"},
		{"line number", "line one\nline two {{bogus}}", "line 2:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMerge("body", tt.src, emailVocabulary)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parseMerge(%q) = %v", tt.src, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseMerge(%q) = %v, want error containing %q", tt.src, err, tt.wantErr)
			}
			var ve *core.ValidationError
			if !errors.As(err, &ve) || ve.Field != "body" {
				t.Fatalf("error %v is not a validation error for body", err)
			}
		})
	}
}

func TestExecuteMerge(t *testing.T) {
	c := &core.Contact{Email: "al@example.com", FirstName: `<b>Al & "Co"</b>`, LastName: "",
		Attributes: map[string]any{"company": "ACME", "seats": float64(42), "ratio": 0.5, "vip": true, "nothing": nil}}
	vals := contactValues(c, "https://x.example.com/u/tok")
	tests := []struct {
		src, raw, escaped string
	}{
		{"Hi {{first_name}}", `Hi <b>Al & "Co"</b>`, "Hi &lt;b&gt;Al &amp; &#34;Co&#34;&lt;/b&gt;"},
		{`{{last_name | "Doe"}}`, "Doe", "Doe"},
		{"[{{last_name}}]", "[]", "[]"},
		{"{{email}}", "al@example.com", "al@example.com"},
		{"{{attr.company}} {{attr.seats}} {{attr.ratio}} {{attr.vip}}", "ACME 42 0.5 true", "ACME 42 0.5 true"},
		{`{{attr.nothing | "n/a"}} {{attr.missing | "?"}}`, "n/a ?", "n/a ?"},
		{`{{unsubscribe_url}}`, "https://x.example.com/u/tok", "https://x.example.com/u/tok"},
		{`{{last_name | "<none>"}}`, "<none>", "&lt;none&gt;"},
	}
	for _, tt := range tests {
		tmpl, err := parseMerge("body", tt.src, emailVocabulary)
		if err != nil {
			t.Fatal(err)
		}
		if got := tmpl.execute(vals, false); got != tt.raw {
			t.Errorf("execute(%q, raw) = %q, want %q", tt.src, got, tt.raw)
		}
		if got := tmpl.execute(vals, true); got != tt.escaped {
			t.Errorf("execute(%q, escaped) = %q, want %q", tt.src, got, tt.escaped)
		}
	}

	// Without a contact every field uses its fallback (or "").
	tmpl, _ := parseMerge("body", `Hi {{first_name | "there"}}{{last_name}}, {{email | "you"}}`, emailVocabulary)
	if got := tmpl.execute(contactValues(nil, ""), false); got != "Hi there, you" {
		t.Fatalf("nil contact = %q", got)
	}
}

func TestPlaceholders(t *testing.T) {
	src := `Hi {{first_name | "there"}}, see [link](https://example.com/?e={{email}}) mrfieldq`
	tmpl, err := parseMerge("body", src, emailVocabulary)
	if err != nil {
		t.Fatal(err)
	}
	withPH, ph := tmpl.placeholders(src)
	if strings.Contains(withPH, "{{") || !strings.Contains(withPH, "mrfieldqq0z") || len(ph.fields) != 2 {
		t.Fatalf("placeholders = %q (prefix must avoid text already containing mrfieldq)", withPH)
	}
	c := &core.Contact{Email: "a+b@example.com", FirstName: "<Al>"}
	got := ph.substituteHTML(withPH, contactValues(c, ""))
	want := `Hi &lt;Al&gt;, see [link](https://example.com/?e=a+b@example.com) mrfieldq`
	if got != want {
		t.Fatalf("subst = %q, want %q", got, want)
	}
}

func TestLayoutVocabulary(t *testing.T) {
	if _, err := parseMerge("custom_html", "{{content}}{{unsubscribe_url}}{{preheader}}{{footer}}{{logo_url}}{{brand_color}}", layoutVocabulary); err != nil {
		t.Fatal(err)
	}
	if _, err := parseMerge("custom_html", "{{first_name}}", layoutVocabulary); err == nil {
		t.Fatal("contact fields must not be allowed in layouts")
	}
}

// TestWrapCustomAddsMissingFooter: a custom layout saved before {{footer}}
// was required still gets the company footer (E-1), after the content;
// one that places {{footer}} gets it only there.
func TestWrapCustomAddsMissingFooter(t *testing.T) {
	p := wrapParams{Content: "<p>Body</p>", Footer: "Example Ltd\n1 Street, Town", UnsubscribeURL: "https://x.example.com/u/t"}
	out, err := wrapCustom(LayoutInput{CustomHTML: `<html><body>{{content}}<a href="{{unsubscribe_url}}">u</a></body></html>`}, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Example Ltd<br>1 Street, Town") || strings.Index(out, "Example Ltd") < strings.Index(out, "Body") {
		t.Fatalf("footer missing or before the content: %s", out)
	}
	out, err = wrapCustom(LayoutInput{CustomHTML: `<html><body>{{content}}<footer>{{footer}}</footer><a href="{{unsubscribe_url}}">u</a></body></html>`}, p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "Example Ltd") != 1 || !strings.Contains(out, "<footer>Example Ltd") {
		t.Fatalf("footer placed = %s", out)
	}
}

// TestWrapShowsHiddenFooterAndUnsubscribe: a custom layout saved before
// hidden unsubscribe links and footers were refused still shows both: a
// visible block with them is added at the end of the body. A layout that
// shows them gets nothing added.
func TestWrapShowsHiddenFooterAndUnsubscribe(t *testing.T) {
	p := wrapParams{Content: "<p>Body</p>", Footer: "Example Ltd, 1 Street", UnsubscribeURL: "https://x.example.com/u/t"}
	hiddenBoth := LayoutInput{CustomHTML: `<html><body><div style="display:none"><a href="{{unsubscribe_url}}">u</a>{{footer}}</div>{{content}}</body></html>`}
	out, err := wrap(hiddenBoth, p)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := placement(LayoutInput{CustomHTML: strings.ReplaceAll(out, "https://x.example.com/u/t", "{{unsubscribe_url}}")})
	if err != nil {
		t.Fatal(err)
	}
	if !pl.unsubVisible || strings.Count(out, "Example Ltd") != 2 || !strings.HasSuffix(out, "</p></body></html>") {
		t.Fatalf("hidden layout rendered as %s", out)
	}
	if text := HTMLToText(out); !strings.Contains(text, "Example Ltd") || !strings.Contains(text, "https://x.example.com/u/t") {
		t.Fatalf("text part = %s", text)
	}
	hiddenFooter := LayoutInput{CustomHTML: `{{content}}<a href="{{unsubscribe_url}}">u</a><span hidden>{{footer}}</span>`}
	if out, err = wrap(hiddenFooter, p); err != nil || strings.Count(out, "https://x.example.com/u/t") != 1 || strings.Count(out, "Example Ltd") != 2 {
		t.Fatalf("hidden footer rendered as %s (%v)", out, err)
	}
	visible := LayoutInput{CustomHTML: `<html><body>{{content}}<p>{{footer}}</p><a href="{{unsubscribe_url}}">u</a></body></html>`}
	if out, err = wrap(visible, p); err != nil || strings.Count(out, "Example Ltd") != 1 || strings.Count(out, "x.example.com/u/t") != 1 {
		t.Fatalf("visible layout rendered as %s (%v)", out, err)
	}
}
