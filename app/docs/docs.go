// Package docs embeds Mailroom's Markdown documentation in the binary, so
// the agent guide (api.md, PRD G-3) is served at /api/v1/docs without any
// file next to the binary.
package docs

import "embed"

// files holds the documents. They are named one by one, so a build
// without the agent guide (or a format doc) fails instead of serving a
// stub.
//
//go:embed api.md import-format.md sequence-format.md
var files embed.FS

// GuideFile is the name of the agent guide in this directory.
const GuideFile = "api.md"

// APIGuide returns the agent guide.
func APIGuide() []byte {
	b, err := files.ReadFile(GuideFile)
	if err != nil {
		panic("docs: " + GuideFile + " is not embedded: " + err.Error()) // impossible: the embed names it
	}
	return b
}

// File returns the embedded document name (e.g. "import-format.md").
func File(name string) ([]byte, error) { return files.ReadFile(name) }
