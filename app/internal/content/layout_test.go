package content_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

func TestValidateLayout(t *testing.T) {
	const good = `<html><body>{{content}}<p>{{footer}}</p><a href="{{unsubscribe_url}}">x</a></body></html>`
	tests := []struct {
		name  string
		in    content.LayoutInput
		field string
	}{
		{"empty is default", content.LayoutInput{}, ""},
		{"full", content.LayoutInput{LogoURL: "https://x.example.com/l.png", BrandColor: "#AbCdEf", CustomHTML: good}, ""},
		{"all fields", content.LayoutInput{CustomHTML: good + "{{preheader}}{{footer}}{{logo_url}}{{brand_color}}"}, ""},
		{"color name", content.LayoutInput{BrandColor: "red"}, "brand_color"},
		{"color short", content.LayoutInput{BrandColor: "#fff"}, "brand_color"},
		{"color css injection", content.LayoutInput{BrandColor: "#ffffff;background:url(x)"}, "brand_color"},
		{"logo relative", content.LayoutInput{LogoURL: "/media/x.png"}, "logo_url"},
		{"logo javascript", content.LayoutInput{LogoURL: "javascript:alert(1)"}, "logo_url"},
		{"logo too long", content.LayoutInput{LogoURL: "https://x.io/" + strings.Repeat("a", 2048)}, "logo_url"},
		{"no unsubscribe", content.LayoutInput{CustomHTML: "<p>{{content}}</p>"}, "custom_html"},
		{"no content", content.LayoutInput{CustomHTML: `<a href="{{unsubscribe_url}}">x</a>`}, "custom_html"},
		{"unsubscribe only in comment", content.LayoutInput{CustomHTML: `<html><body>{{content}}<!-- {{unsubscribe_url}} --></body></html>`}, "custom_html"},
		{"unsubscribe only as text", content.LayoutInput{CustomHTML: `<p>{{content}}</p><p>Leave: {{unsubscribe_url}}</p>`}, "custom_html"},
		{"unsubscribe only in img", content.LayoutInput{CustomHTML: `{{content}}<img src="{{unsubscribe_url}}">`}, "custom_html"},
		{"unsubscribe only in title attr", content.LayoutInput{CustomHTML: `{{content}}<a href="#" title="{{unsubscribe_url}}">x</a>`}, "custom_html"},
		{"unsubscribe only in mso comment", content.LayoutInput{CustomHTML: `{{content}}<!--[if mso]><a href="{{unsubscribe_url}}">x</a><![endif]-->`}, "custom_html"},
		{"unsubscribe link uppercase, spaced", content.LayoutInput{CustomHTML: `{{content}}{{footer}}<A HREF=' {{unsubscribe_url}} '>x</A>`}, ""},
		// E-1: the company name and postal address are in every marketing email.
		{"no footer", content.LayoutInput{CustomHTML: `<html><body>{{content}}<p><a href="{{unsubscribe_url}}">Unsubscribe</a></p></body></html>`}, "custom_html"},
		{"footer only in comment", content.LayoutInput{CustomHTML: `{{content}}<a href="{{unsubscribe_url}}">x</a><!-- {{footer}} -->`}, "custom_html"},
		// Neither may be hidden (a pasted design with hidden blocks).
		{"both hidden", content.LayoutInput{CustomHTML: `<div style="display:none"><a href="{{unsubscribe_url}}">u</a> {{footer}}</div>{{content}}`}, "custom_html"},
		{"unsubscribe hidden attr", content.LayoutInput{CustomHTML: `{{content}}<p>{{footer}}</p><div hidden><a href="{{unsubscribe_url}}">u</a></div>`}, "custom_html"},
		{"unsubscribe visibility hidden", content.LayoutInput{CustomHTML: `{{content}}<p>{{footer}}</p><p style="color:#999; Visibility: hidden !important"><a href="{{unsubscribe_url}}">u</a></p>`}, "custom_html"},
		{"unsubscribe font-size 0", content.LayoutInput{CustomHTML: `{{content}}<p>{{footer}}</p><a style="font-size:0px" href="{{unsubscribe_url}}">u</a>`}, "custom_html"},
		{"unsubscribe in zero height box", content.LayoutInput{CustomHTML: `{{content}}<p>{{footer}}</p><div style="max-height:0;overflow:hidden"><a href="{{unsubscribe_url}}">u</a></div>`}, "custom_html"},
		{"unsubscribe in head", content.LayoutInput{CustomHTML: `<html><head><title><a href="{{unsubscribe_url}}">u</a></title></head><body>{{content}}<p>{{footer}}</p></body></html>`}, "custom_html"},
		{"footer opacity 0", content.LayoutInput{CustomHTML: `{{content}}<a href="{{unsubscribe_url}}">u</a><p style="opacity:0">{{footer}}</p>`}, "custom_html"},
		{"footer mso-hide", content.LayoutInput{CustomHTML: `{{content}}<a href="{{unsubscribe_url}}">u</a><span style="mso-hide:all">{{footer}}</span>`}, "custom_html"},
		{"hidden copy plus a visible one", content.LayoutInput{CustomHTML: `{{content}}<div style="display:none"><a href="{{unsubscribe_url}}">u</a>{{footer}}</div><p style="font-size:12px;opacity:0.8">{{footer}} <a href="{{unsubscribe_url}}">Unsubscribe</a></p>`}, ""},
		{"hidden preheader is fine", content.LayoutInput{CustomHTML: `<span style="display:none;max-height:0;overflow:hidden">{{preheader}}</span>` + good}, ""},
		{"content twice", content.LayoutInput{CustomHTML: good + "{{content}}"}, "custom_html"},
		{"contact field", content.LayoutInput{CustomHTML: good + "{{first_name}}"}, "custom_html"},
		{"unclosed", content.LayoutInput{CustomHTML: good + "{{footer"}, "custom_html"},
		{"too big", content.LayoutInput{CustomHTML: good + strings.Repeat("x", content.MaxCustomHTMLLength)}, "custom_html"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := content.ValidateLayout(tt.in)
			if tt.field != "" {
				if fieldOf(err) != tt.field {
					t.Fatalf("err = %v, want field %s", err, tt.field)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.BrandColor == "" || got.BrandColor != strings.ToLower(got.BrandColor) {
				t.Fatalf("brand colour not normalised: %q", got.BrandColor)
			}
		})
	}
}

func TestPutLayout(t *testing.T) {
	f := newFixture(t)
	def, err := f.svc.GetLayout(f.ctx, nil)
	if err != nil || def.BrandColor != content.DefaultBrandColor || def.CustomHTML != "" || def.Version <= 0 {
		t.Fatalf("default = %+v %v", def, err)
	}
	put := func(in content.LayoutInput, ifVersion int64) (content.Layout, error) {
		var l content.Layout
		err := f.tx(func(tx *sql.Tx) error {
			var err error
			l, err = f.svc.PutLayout(f.ctx, tx, in, ifVersion)
			return err
		})
		return l, err
	}
	l, err := put(content.LayoutInput{LogoURL: " https://x.example.com/logo.png ", BrandColor: "#FF0000"}, def.Version)
	if err != nil || l.LogoURL != "https://x.example.com/logo.png" || l.BrandColor != "#ff0000" || l.Version == def.Version {
		t.Fatalf("put = %+v %v", l, err)
	}
	got, _ := f.svc.GetLayout(f.ctx, nil)
	if got != l {
		t.Fatalf("get = %+v, want %+v", got, l)
	}
	if _, err := put(content.LayoutInput{BrandColor: "#00ff00"}, def.Version); !errors.Is(err, core.ErrPrecondition) {
		t.Fatalf("stale If-Match = %v", err)
	}
	same, err := put(content.LayoutInput{LogoURL: "https://x.example.com/logo.png", BrandColor: "#ff0000"}, l.Version)
	if err != nil || same.Version != l.Version {
		t.Fatalf("no-op put = %+v %v", same, err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation = 'layout.update'`); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	if _, err := put(content.LayoutInput{CustomHTML: "{{content}}"}, 0); fieldOf(err) != "custom_html" {
		t.Fatalf("E-4 = %v", err)
	}
	var raw string
	if err := f.d.Read.QueryRow(`SELECT value FROM settings WHERE key = 'layout'`).Scan(&raw); err != nil || !strings.Contains(raw, `"brand_color":"#ff0000"`) {
		t.Fatalf("stored = %s %v", raw, err)
	}
}
