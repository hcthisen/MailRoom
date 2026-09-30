package content

import (
	"testing"

	"mailroom/internal/core"
)

func TestSubstituteHTML(t *testing.T) {
	c := &core.Contact{Email: "a+b@example.com", FirstName: `<Al & "Bo">`, Attributes: map[string]any{
		"site":  "https://al.example.com/x?y=1",
		"evil":  "javascript:alert(document.cookie)",
		"evil2": " JaVaScRiPt:alert(1)",
		"evil3": "java\tscript:alert(1)",
		"data":  "data:text/html;base64,PHNjcmlwdD4=",
		"path":  "a b/c?d#e",
		"tel":   "tel:+441234",
		"code":  "x&y=z",
	}}
	unsub := "https://news.example.com/u/tok"
	tests := []struct {
		name, src, want string
	}{
		{"text escaped", `<p>Hi {{first_name}}</p>`, `<p>Hi &lt;Al &amp; &#34;Bo&#34;&gt;</p>`},
		{"plain attribute raw then escaped once", `<img alt="{{first_name}}">`, `<img alt="&lt;Al &amp; &#34;Bo&#34;&gt;">`},
		{"whole href safe", `<a href="{{attr.site}}">s</a>`, `<a href="https://al.example.com/x?y=1" data-mr-notrack="">s</a>`},
		{"javascript href", `<a href="{{attr.evil}}">s</a>`, `<a href="#" data-mr-notrack="">s</a>`},
		{"mixed-case javascript", `<a href="{{attr.evil2}}">s</a>`, `<a href="#" data-mr-notrack="">s</a>`},
		{"control char javascript", `<a href="{{attr.evil3}}">s</a>`, `<a href="#" data-mr-notrack="">s</a>`},
		{"unsafe uses safe fallback", `<a href="{{attr.evil | "https://ok.example.com"}}">s</a>`, `<a href="https://ok.example.com" data-mr-notrack="">s</a>`},
		{"unsafe fallback not used", `<a href="{{attr.missing | "javascript:x"}}">s</a>`, `<a href="#" data-mr-notrack="">s</a>`},
		{"data image src", `<img src="{{attr.data}}">`, `<img src="#">`},
		{"tel allowed", `<a href="{{attr.tel}}">t</a>`, `<a href="tel:+441234" data-mr-notrack="">t</a>`},
		{"query escaped", `<a href="https://x.example.com/?e={{email}}&c={{attr.code}}">q</a>`,
			`<a href="https://x.example.com/?e=a%2Bb%40example.com&amp;c=x%26y%3Dz" data-mr-notrack="">q</a>`},
		{"path escaped keeps slash", `<a href="https://x.example.com/{{attr.path}}">p</a>`,
			`<a href="https://x.example.com/a%20b/c%3Fd%23e" data-mr-notrack="">p</a>`},
		{"prefix scheme checked", `<a href="{{attr.evil}}//x">p</a>`, `<a href="#//x" data-mr-notrack="">p</a>`},
		{"unsubscribe trusted", `<a href="{{unsubscribe_url}}">u</a>`, `<a href="https://news.example.com/u/tok" data-mr-notrack="">u</a>`},
		{"unsubscribe in query escaped", `<a href="https://p.example.com/?u={{unsubscribe_url}}">u</a>`,
			`<a href="https://p.example.com/?u=https%3A%2F%2Fnews.example.com%2Fu%2Ftok" data-mr-notrack="">u</a>`},
		{"existing notrack kept once", `<a data-mr-notrack href="{{attr.site}}">s</a>`, `<a data-mr-notrack="" href="https://al.example.com/x?y=1">s</a>`},
		{"autolink href and text", `<a href="https://x.example.com/{{email}}">https://x.example.com/{{email}}</a>`,
			`<a href="https://x.example.com/a+b@example.com" data-mr-notrack="">https://x.example.com/a+b@example.com</a>`},
		{"no fields untouched", `<A HREF='https://x.example.com'>x</A>`, `<A HREF='https://x.example.com'>x</A>`},
		{"comment escaped", `<!-- {{first_name}} -->`, `<!-- &lt;Al &amp; &#34;Bo&#34;&gt; -->`},
		{"style text", `<style>.a{content:"{{email}}"}</style>`, `<style>.a{content:"a+b@example.com"}</style>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := parseMerge("body", tt.src, emailVocabulary)
			if err != nil {
				t.Fatal(err)
			}
			src, ph := tmpl.placeholders(tt.src)
			if got := ph.substituteHTML(src, contactValues(c, unsub)); got != tt.want {
				t.Fatalf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestSubstituteHTMLValueLooksLikePlaceholder(t *testing.T) {
	src := `<a href="https://x.example.com/?n={{first_name}}" title="{{first_name}}">{{last_name}}</a>`
	tmpl, err := parseMerge("body", src, emailVocabulary)
	if err != nil {
		t.Fatal(err)
	}
	withPH, ph := tmpl.placeholders(src)
	// A value that looks like a placeholder must not be substituted again.
	c := &core.Contact{FirstName: ph.prefix + "2z", LastName: "L"}
	got := ph.substituteHTML(withPH, contactValues(c, ""))
	want := `<a href="https://x.example.com/?n=` + ph.prefix + `2z" title="` + ph.prefix + `2z" data-mr-notrack="">L</a>`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestSafeURL(t *testing.T) {
	for s, want := range map[string]bool{
		"": true, "https://x.example.com": true, "HTTP://x": true, "mailto:a@b.c": true, "tel:+1": true,
		"/relative": true, "#top": true, "javascript:alert(1)": false, "vbscript:x": false,
		"data:text/html,x": false, "java\nscript:x": false, "\x00javascript:x": false,
	} {
		if got := safeURL(s); got != want {
			t.Errorf("safeURL(%q) = %v, want %v", s, got, want)
		}
	}
}
