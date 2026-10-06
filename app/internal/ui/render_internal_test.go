package ui

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// TestTemplatesEmbedEveryFile: every file under templates/ is embedded,
// including the group partials whose names start with "_" (a plain
// `//go:embed templates` leaves those out), and a group partial is
// parsed into every page of its group.
func TestTemplatesEmbedEveryFile(t *testing.T) {
	var partials int
	err := fs.WalkDir(os.DirFS("templates"), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, err := fs.Stat(templateFS, "templates/"+p); err != nil {
			t.Errorf("templates/%s is not embedded: %v", p, err)
		}
		if strings.HasPrefix(d.Name(), "_") {
			partials++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if partials == 0 {
		t.Fatal("no templates/<group>/_*.html partial on disk: this test needs one")
	}

	pages, err := parseTemplates((&UI{}).funcs())
	if err != nil {
		t.Fatal(err)
	}
	for page, partial := range map[string]string{
		"broadcasts/index":  "audience",
		"broadcasts/show":   "paused-reason",
		"content/emails":    "content-tabs",
		"content/email":     "email-uses",
		"content/media":     "content-tabs",
		"broadcasts/layout": "broadcast-when",
	} {
		p, ok := pages[page]
		if !ok {
			t.Errorf("no page %s", page)
			continue
		}
		if p.t.Lookup(partial) == nil {
			t.Errorf("page %s lacks the group partial %q", page, partial)
		}
	}
	for name := range pages {
		if strings.Contains(name, "/_") {
			t.Errorf("partial file parsed as page %s", name)
		}
	}
}
