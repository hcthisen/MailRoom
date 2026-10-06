package docs

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestEmbedded: the agent guide and the format docs exist next to this
// file, are not empty, and are embedded as they are on disk.
func TestEmbedded(t *testing.T) {
	for _, name := range []string{GuideFile, "import-format.md", "sequence-format.md"} {
		onDisk, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s is missing: %v", name, err)
		}
		if len(bytes.TrimSpace(onDisk)) == 0 {
			t.Fatalf("%s is empty", name)
		}
		b, err := File(name)
		if err != nil || !bytes.Equal(b, onDisk) {
			t.Fatalf("%s: embedded copy differs from the file (%v)", name, err)
		}
	}
	if g := string(APIGuide()); !strings.HasPrefix(g, "# ") || !strings.Contains(g, "/api/v1") {
		t.Fatalf("api.md doesn't look like the agent guide: %.200s", g)
	}
}
