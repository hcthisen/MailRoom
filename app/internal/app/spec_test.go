package app_test

import (
	"bytes"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// update regenerates the checked-in spec snapshot:
//
//	go test ./internal/app -run TestSpecSnapshot -update
var update = flag.Bool("update", false, "rewrite api/openapi.yaml from the registered operations")

// specSnapshot is the checked-in OpenAPI snapshot (PRD G-2), relative to
// this package.
var specSnapshot = filepath.Join("..", "..", "api", "openapi.yaml")

// TestSpecSnapshot compares the served OpenAPI spec with the checked-in
// api/openapi.yaml, so a change to any operation shows up in review. Run
// it with -update after an intended change.
func TestSpecSnapshot(t *testing.T) {
	hs := newHarness(t, nil)
	r := hs.raw(http.MethodGet, "/api/v1/openapi.yaml", "", "").want(t, http.StatusOK)
	spec := r.body
	if len(spec) == 0 || !bytes.Contains(spec, []byte("openapi: 3.1.0")) {
		t.Fatalf("spec = %.200s", spec)
	}
	// The spec is generated on every request; it must be stable.
	if again := hs.raw(http.MethodGet, "/api/v1/openapi.yaml", "", "").body; !bytes.Equal(spec, again) {
		t.Fatal("the generated spec differs between two requests")
	}
	if *update {
		if err := os.MkdirAll(filepath.Dir(specSnapshot), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(specSnapshot, spec, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", specSnapshot, len(spec))
		return
	}
	want, err := os.ReadFile(specSnapshot)
	if err != nil {
		t.Fatalf("read the snapshot (generate it with -update): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), spec) {
		t.Fatalf("api/openapi.yaml is out of date: run go test ./internal/app -run TestSpecSnapshot -update and review the diff")
	}
}
