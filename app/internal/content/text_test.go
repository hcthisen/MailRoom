package content

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{"paragraphs", "<p>One\n   two</p><p>Three</p>", "One two\n\nThree\n"},
		{"headings", "<h1>Title</h1><h2>Sub</h2><h3>Minor</h3><p>x</p>", "Title\n=====\n\nSub\n---\n\nMinor\n\nx\n"},
		{"list", "<ul><li>One</li><li>Two</li></ul><p>after</p>", "- One\n- Two\n\nafter\n"},
		{"ordered list", "<ol><li>One</li><li>Two</li></ol>", "1. One\n2. Two\n"},
		{"nested list", "<ul><li>A<ul><li>B</li></ul></li><li>C</li></ul>", "- A\n  - B\n- C\n"},
		{"link", `<p>See <a href="https://example.com/x">the docs</a>.</p>`, "See the docs (https://example.com/x).\n"},
		{"link equal text", `<p><a href="https://example.com">https://example.com</a></p>`, "https://example.com\n"},
		{"mailto", `<p><a href="mailto:hi@example.com">Write us</a></p>`, "Write us (hi@example.com)\n"},
		{"anchor link", `<p><a href="#preview">Unsubscribe</a></p>`, "Unsubscribe\n"},
		{"image alt", `<p><img src="a.png" alt="Logo"> <img src="p.gif" alt=""></p>`, "Logo\n"},
		{"br", "<p>a<br>b<br/>c</p>", "a\nb\nc\n"},
		{"entities", "<p>Fish &amp; chips&nbsp;&nbsp;now</p>", "Fish & chips now\n"},
		{"skip head style script", "<html><head><title>T</title><style>p{}</style></head><body><script>x()</script><p>Body</p></body></html>", "Body\n"},
		{"skip preheader", `<div data-mr-preheader="" style="display:none">Preview</div><p>Body</p>`, "Body\n"},
		{"skip display none", `<p style="display: none">Hidden</p><p>Shown</p>`, "Shown\n"},
		{"data table", "<table><thead><tr><th>A</th><th>B</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table>", "A | B\n1 | 2\n"},
		{"layout table", `<table role="presentation"><tr><td>Left</td><td>Right</td></tr></table>`, "Left\n\nRight\n"},
		{"pre", "<pre>a  b\n  c</pre>", "a  b\n  c\n"},
		{"hr", "<p>a</p><hr><p>b</p>", "a\n\n--------------------\n\nb\n"},
		{"strong inline", "<p>This is <strong>bold</strong> and <em>it</em>.</p>", "This is bold and it.\n"},
		{"empty", "", "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTMLToText(tt.html); got != tt.want {
				t.Fatalf("HTMLToText(%q)\n got: %q\nwant: %q", tt.html, got, tt.want)
			}
		})
	}
}

func TestHTMLToTextWraps(t *testing.T) {
	long := strings.Repeat("word ", 40)
	url := "https://example.com/" + strings.Repeat("x", 100)
	got := HTMLToText("<p>" + long + `<a href="` + url + `">link</a></p><ul><li>` + long + "</li></ul>")
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if utf8.RuneCountInString(line) > TextWidth && !strings.Contains(line, url) {
			t.Fatalf("line longer than %d: %q", TextWidth, line)
		}
	}
	if !strings.Contains(got, "("+url+")") {
		t.Fatalf("URL must not be broken:\n%s", got)
	}
	// List continuation lines are indented under the text.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	var inItem bool
	for _, l := range lines {
		if strings.HasPrefix(l, "- ") {
			inItem = true
			continue
		}
		if inItem && !strings.HasPrefix(l, "  word") {
			t.Fatalf("list continuation not indented: %q", l)
		}
	}
}
