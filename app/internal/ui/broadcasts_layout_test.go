package ui_test

import (
	"net/http"
	"net/url"
	"testing"
)

func Test_broadcasts_layoutEditor(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()

	page := c.get("/admin/layout").want(t, http.StatusOK)
	page.contains(t, "<h1>Base layout</h1>", `value="#2563eb"`, "Example Ltd, 1 Street, Town", `hx-post="/admin/layout/preview"`,
		`aria-current="page">Emails`, `aria-current="page">Base layout`)
	ver := page.field(t, "version")

	// Live preview: sample content in the unsaved layout, sandboxed.
	c.htmx(http.MethodPost, "/admin/layout/preview", url.Values{"brand_color": {"#aa0000"}, "logo_url": {"https://cdn.example.com/logo.png"}}).
		want(t, http.StatusOK).contains(t, `<iframe class="preview-frame"`, `sandbox="allow-same-origin"`, "#aa0000", "logo.png", "Your headline").lacks(t, "<html")
	c.htmx(http.MethodPost, "/admin/layout/preview", url.Values{"brand_color": {"red"}}).
		want(t, http.StatusOK).contains(t, "preview-error", "brand_color: must be a colour like #1a2b3c")
	c.htmx(http.MethodPost, "/admin/layout/preview", url.Values{"custom_html": {"<html><body>{{content}}</body></html>"}}).
		want(t, http.StatusOK).contains(t, "preview-error", "unsubscribe_url")

	// Validation errors are shown next to the field, nothing is saved.
	bad := url.Values{"version": {ver}, "brand_color": {"#123456"}, "custom_html": {"<div>{{content}}</div>"}}
	c.post("/admin/layout", bad).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-custom_html-error"`, "every marketing email needs an unsubscribe link", "&lt;div&gt;{{content}}&lt;/div&gt;")
	c.post("/admin/layout", url.Values{"version": {ver}, "logo_url": {"ftp://x"}}).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-logo_url-error"`)
	if l, _ := hs.app.Content.GetLayout(hs.ctx, nil); l.BrandColor != "#2563eb" {
		t.Fatalf("invalid layout saved: %+v", l)
	}

	// A valid layout is saved (update-layout) with an audit record.
	good := url.Values{"version": {ver}, "brand_color": {"#123456"}, "logo_url": {"https://cdn.example.com/logo.png"},
		"custom_html": {`<html><body><p>{{preheader}}</p>{{content}}<p>{{footer}} <a href="{{unsubscribe_url}}">Unsubscribe</a></p></body></html>`}}
	c.post("/admin/layout", good).wantRedirect(t, "/admin/layout")
	l, err := hs.app.Content.GetLayout(hs.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.BrandColor != "#123456" || l.LogoURL != "https://cdn.example.com/logo.png" || l.CustomHTML == "" {
		t.Fatalf("saved %+v", l)
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation = 'layout.update'`); n != 1 {
		t.Fatalf("%d layout.update audit records", n)
	}
	c.get("/admin/layout").want(t, http.StatusOK).contains(t, "Layout saved.", `value="#123456"`)

	// The version the page was loaded at is an If-Match: a stale save is 412.
	good.Set("brand_color", "#654321")
	c.post("/admin/layout", good).want(t, http.StatusPreconditionFailed).contains(t, "Someone else changed this")
	if l, _ := hs.app.Content.GetLayout(hs.ctx, nil); l.BrandColor != "#123456" {
		t.Fatalf("stale layout saved: %+v", l)
	}
}
