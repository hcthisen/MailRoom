package content_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/testutil"
)

// Minimal file headers recognised by content sniffing.
var (
	pngData  = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{1}, 64)...)
	jpegData = append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), bytes.Repeat([]byte{2}, 64)...)
	gifData  = append([]byte("GIF89a"), bytes.Repeat([]byte{3}, 64)...)
	webpData = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{4}, 64)...)
)

func TestUploadMedia(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, filename string
		data           []byte
		ct, ext        string
		err            bool
	}{
		{"png", "logo.png", pngData, "image/png", ".png", false},
		{"jpeg misnamed", "photo.gif", jpegData, "image/jpeg", ".jpg", false},
		{"gif", `C:\Users\x\anim.gif`, gifData, "image/gif", ".gif", false},
		{"webp", "../../pic.webp", webpData, "image/webp", ".webp", false},
		{"svg", "x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "", "", true},
		{"html as png", "x.png", []byte("<html><script>alert(1)</script></html>"), "", "", true},
		{"empty", "x.png", nil, "", "", true},
		{"too big", "x.png", append(append([]byte{}, pngData...), make([]byte, content.MaxMediaBytes)...), "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := f.svc.UploadMedia(f.ctx, tt.filename, bytes.NewReader(tt.data))
			if tt.err {
				if fieldOf(err) != "file" {
					t.Fatalf("err = %v, want validation of file", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.ContentType != tt.ct || !strings.HasSuffix(m.StoredName, tt.ext) || len(m.StoredName) != 32+len(tt.ext) ||
				m.URL != testBaseURL+"/media/"+m.StoredName || m.SizeBytes != int64(len(tt.data)) || strings.ContainsAny(m.Filename, `/\`) {
				t.Fatalf("media = %+v", m)
			}
			b, err := os.ReadFile(filepath.Join(f.dataDir, "media", m.StoredName))
			if err != nil || !bytes.Equal(b, tt.data) {
				t.Fatalf("stored file: %v", err)
			}
		})
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM media`); n != 4 {
		t.Fatalf("media rows = %d", n)
	}
	entries, _ := os.ReadDir(filepath.Join(f.dataDir, "media"))
	if len(entries) != 4 {
		t.Fatalf("files = %d (temp files must not remain)", len(entries))
	}
	list, err := f.svc.ListMedia(f.ctx, 0, 10)
	if err != nil || len(list) != 4 || list[0].Filename != "logo.png" || list[2].Filename != "anim.gif" || list[3].Filename != "pic.webp" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if got, err := f.svc.GetMedia(f.ctx, list[1].ID); err != nil || got != list[1] {
		t.Fatalf("get = %+v %v", got, err)
	}
}

func TestMediaHandler(t *testing.T) {
	f := newFixture(t)
	m, err := f.svc.UploadMedia(f.ctx, "logo.png", bytes.NewReader(pngData))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /media/{name}", f.svc.MediaHandler())
	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	rec := get("GET", "/media/"+m.StoredName)
	h := rec.Header()
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), pngData) || h.Get("Content-Type") != "image/png" ||
		h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Cache-Control"), "max-age=31536000") {
		t.Fatalf("get = %d %v", rec.Code, h)
	}
	if rec := get("HEAD", "/media/"+m.StoredName); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("head = %d", rec.Code)
	}
	for _, p := range []string{"/media/nothere.png", "/media/" + strings.Repeat("0", 32) + ".png", "/media/..%2fmailroom.db"} {
		if rec := get("GET", p); rec.Code != 404 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	// Without a {name} pattern the last path segment is used.
	rec = httptest.NewRecorder()
	f.svc.MediaHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/media/"+m.StoredName, nil))
	if rec.Code != 200 {
		t.Fatalf("plain handler = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	f.svc.MediaHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/media/"+m.StoredName, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d", rec.Code)
	}
}

func TestDeleteMedia(t *testing.T) {
	f := newFixture(t)
	used, _ := f.svc.UploadMedia(f.ctx, "a.png", bytes.NewReader(pngData))
	logo, _ := f.svc.UploadMedia(f.ctx, "b.png", bytes.NewReader(pngData))
	free, _ := f.svc.UploadMedia(f.ctx, "c.png", bytes.NewReader(pngData))
	em := f.createEmail(content.EmailInput{Subject: "S", Body: "![x](" + used.URL + ")"})
	f.putLayout(content.LayoutInput{LogoURL: logo.URL})

	if err := f.svc.DeleteMedia(f.ctx, used.ID); !errors.Is(err, core.ErrConflict) || !strings.Contains(err.Error(), "email") {
		t.Fatalf("used by email %d = %v", em.ID, err)
	}
	if err := f.svc.DeleteMedia(f.ctx, logo.ID); !errors.Is(err, core.ErrConflict) || !strings.Contains(err.Error(), "layout") {
		t.Fatalf("used by layout = %v", err)
	}
	if err := f.svc.DeleteMedia(f.ctx, free.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "media", free.StoredName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file must be removed: %v", err)
	}
	if err := f.svc.DeleteMedia(f.ctx, free.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if n := testutil.Count(t, f.d.Read, `SELECT count(*) FROM audit_log WHERE operation IN ('media.upload','media.delete')`); n != 4 {
		t.Fatalf("audit rows = %d", n)
	}
}

func TestMediaWithoutDataDir(t *testing.T) {
	f := newFixture(t)
	svc := content.New(content.Deps{DB: f.d, Settings: f.settings, Signer: f.signer, BaseURL: testBaseURL, Clock: f.clock})
	if _, err := svc.UploadMedia(f.ctx, "a.png", bytes.NewReader(pngData)); err == nil || errors.Is(err, core.ErrValidation) {
		t.Fatalf("upload without data dir = %v, want an internal error", err)
	}
}
