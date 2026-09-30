package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"mailroom/static"
)

// asset is one embedded static file.
type asset struct {
	body  []byte
	hash  string // first 12 hex digits of its SHA-256: the ?v= query
	ctype string
}

// servedTypes are the static file types served (VENDOR.md isn't).
var servedTypes = map[string]string{
	".js":  "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
}

func loadAssets() map[string]asset {
	out := map[string]asset{}
	_ = fs.WalkDir(static.FS, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		ct, ok := servedTypes[path.Ext(name)]
		if !ok {
			return nil
		}
		b, err := static.FS.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[name] = asset{body: b, hash: hex.EncodeToString(sum[:])[:12], ctype: ct}
		return nil
	})
	return out
}

// staticURL returns the URL of a static file with its content hash, so
// it can be cached for a year and still changes with every release.
func (u *UI) staticURL(name string) string {
	a, ok := u.assets[name]
	if !ok {
		return Prefix + "/static/" + name
	}
	return Prefix + "/static/" + name + "?v=" + a.hash
}

// serveStatic serves an embedded static file. With the current content
// hash in ?v= it may be cached for a year (immutable); otherwise briefly.
func (u *UI) serveStatic(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	a, ok := u.assets[name]
	if !ok || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return nil
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	if r.URL.Query().Get("v") == a.hash {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=300")
	}
	h.Set("ETag", `"`+a.hash+`"`)
	if r.Header.Get("If-None-Match") == `"`+a.hash+`"` {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	_, _ = w.Write(a.body)
	return nil
}
