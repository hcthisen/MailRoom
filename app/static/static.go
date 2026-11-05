// Package static embeds the admin UI's static files (htmx, the style
// sheet and a small script), so the binary serves them without any file
// next to it and without a CDN. internal/ui serves them at /admin/static/
// with a content-hash query string and a long cache lifetime. VENDOR.md
// records where htmx.min.js comes from.
package static

import "embed"

// FS holds the static files, at the root of the file system.
//
//go:embed htmx.min.js app.css app.js editor.js VENDOR.md
var FS embed.FS
