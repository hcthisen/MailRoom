package ui_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// contentPNG is a small valid PNG image.
func contentPNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestContentMedia(t *testing.T) {
	hs := newHarness(t)
	c := hs.session()

	c.get("/admin/media").want(t, http.StatusOK).contains(t, "<h1>Images</h1>", `enctype="multipart/form-data"`,
		`aria-current="page">Images`, "No images yet")

	// CSRF is checked on the multipart upload too; nothing is stored.
	c.audMultipart("/admin/media", nil, "logo.png", contentPNG(t), false).want(t, http.StatusForbidden)
	c.postNoCSRF("/admin/media/1/delete", url.Values{"confirm": {"yes"}}).want(t, http.StatusForbidden)

	// Validation errors are shown next to the field.
	c.audMultipart("/admin/media", nil, "", "", true).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-file-error"`, "choose an image file")
	c.audMultipart("/admin/media", nil, "notes.txt", "just text", true).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-file-error"`, "must be a PNG, JPEG, GIF or WebP image")
	big := string(bytes.Repeat([]byte{0}, 5<<20+10))
	c.audMultipart("/admin/media", nil, "huge.png", "\x89PNG\r\n\x1a\n"+big, true).want(t, http.StatusUnprocessableEntity).
		contains(t, `id="f-file-error"`, "larger than 5 MiB")
	if n := hs.count(`SELECT count(*) FROM media`); n != 0 {
		t.Fatalf("%d media rows after invalid uploads", n)
	}

	// Upload (upload-media): listed with its public address and a copy button.
	c.audMultipart("/admin/media", nil, "logo.png", contentPNG(t), true).wantRedirect(t, "/admin/media")
	ms, err := hs.app.Content.ListMedia(hs.ctx, 0, 10)
	if err != nil || len(ms) != 1 {
		t.Fatalf("media = %v, %v", ms, err)
	}
	m := ms[0]
	if _, err := os.Stat(filepath.Join(hs.app.Config.DataDir, "media", m.StoredName)); err != nil {
		t.Fatalf("stored file: %v", err)
	}
	c.get("/admin/media").want(t, http.StatusOK).contains(t, "logo.png uploaded.", m.URL,
		fmt.Sprintf(`data-copy="#media-url-%d"`, m.ID), `class="media-thumb"`)

	// Delete (delete-media): a confirmation first; refused while the
	// layout uses the image.
	path := fmt.Sprintf("/admin/media/%d/delete", m.ID)
	c.post(path, nil).want(t, http.StatusOK).contains(t, "Delete logo.png?", "broken image", m.URL, `name="confirm" value="yes"`)
	l, err := hs.app.Content.GetLayout(hs.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ver := fmt.Sprint(l.Version)
	c.post("/admin/layout", url.Values{"version": {ver}, "logo_url": {m.URL}}).wantRedirect(t, "/admin/layout")
	c.post(path, url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/media")
	c.get("/admin/media").want(t, http.StatusOK).contains(t, "logo.png was not deleted", "used by the layout")
	if n := hs.count(`SELECT count(*) FROM media`); n != 1 {
		t.Fatal("an image in use was deleted")
	}
	l, _ = hs.app.Content.GetLayout(hs.ctx, nil)
	c.post("/admin/layout", url.Values{"version": {fmt.Sprint(l.Version)}, "logo_url": {""}}).wantRedirect(t, "/admin/layout")
	c.post(path, url.Values{"confirm": {"yes"}}).wantRedirect(t, "/admin/media")
	if n := hs.count(`SELECT count(*) FROM media`); n != 0 {
		t.Fatal("the image was not deleted")
	}
	if n := hs.count(`SELECT count(*) FROM audit_log WHERE operation IN ('media.upload', 'media.delete')`); n != 2 {
		t.Fatalf("%d media audit rows, want 2", n)
	}
}
