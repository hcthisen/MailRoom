package broadcasts

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"mailroom/internal/content"
	"mailroom/internal/core"
)

// ArchivePattern is the public mux pattern of the web archive page (B-10).
const ArchivePattern = "GET /archive/{id}"

// archivePathPrefix is the URL path of an archive page without the ID.
const archivePathPrefix = "/archive/"

// archiveCSP allows the images, fonts and inline styles an email uses and
// nothing that runs: no scripts, frames, forms or plugins.
const archiveCSP = "default-src 'none'; img-src https: data:; style-src 'unsafe-inline'; font-src https: data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// archived reports whether a broadcast has a public archive page: it went
// out (sending, paused or sent) to everyone. Drafts, scheduled and
// cancelled broadcasts and segment broadcasts (whose content may be meant
// for that group only) have none.
func archived(status, audienceType string) bool {
	switch status {
	case StatusSending, StatusPaused, StatusSent:
		return audienceType == AudienceAll
	}
	return false
}

// withArchiveURL sets b.ArchiveURL when the broadcast has an archive page
// and Deps.BaseURL is set.
func (s *Service) withArchiveURL(b Broadcast) Broadcast {
	b.ArchiveURL = ""
	if s.baseURL != "" && archived(b.Status, b.Audience.Type) {
		b.ArchiveURL = s.baseURL + archivePathPrefix + strconv.FormatInt(b.ID, 10)
	}
	return b
}

// ArchiveHandler serves the public web archive page of a broadcast (B-10)
// for GET and HEAD. Mount it on the public mux at ArchivePattern. The
// email is rendered without a contact, so every merge field shows its
// fallback and no personal data appears; it has no click or open
// tracking and its unsubscribe link is inert. Broadcasts without an
// archive page (see Broadcast.ArchiveURL) are 404.
func (s *Service) ArchiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw := r.PathValue("id")
		if raw == "" {
			raw = strings.TrimPrefix(r.URL.Path, archivePathPrefix)
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
			http.NotFound(w, r)
			return
		}
		page, err := s.archivePage(r.Context(), id)
		if errors.Is(err, core.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.log.ErrorContext(r.Context(), "broadcasts: archive page", "broadcast_id", id, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", archiveCSP)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex")
		h.Set("Cache-Control", "public, max-age=300")
		h.Set("Content-Length", strconv.Itoa(len(page)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(page))
		}
	})
}

// archivePage renders the archive page of broadcast id. Broadcasts without
// one wrap core.ErrNotFound. It must not run inside a write transaction
// (rendering may read the layout and settings).
func (s *Service) archivePage(ctx context.Context, id int64) (string, error) {
	b, err := s.get(ctx, nil, id)
	if err != nil {
		return "", err
	}
	if !archived(b.Status, b.Audience.Type) {
		return "", fmt.Errorf("broadcast %d has no archive page: %w", id, core.ErrNotFound)
	}
	out, err := s.content.RenderDraft(ctx, content.Draft{
		Subject: b.Email.Subject, Preheader: b.Email.Preheader, Format: b.Email.Format, Body: b.Email.Body,
	}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("broadcasts: render archive %d: %w", id, err)
	}
	return withTitle(out.HTML, out.Subject), nil
}

// withTitle makes sure the page has a <title> with the subject.
func withTitle(page, subject string) string {
	lower := strings.ToLower(page)
	if strings.Contains(lower, "<title") {
		return page
	}
	title := "<title>" + html.EscapeString(subject) + "</title>"
	if i := strings.Index(lower, "<head>"); i >= 0 {
		return page[:i+len("<head>")] + title + page[i+len("<head>"):]
	}
	return "<!doctype html><html><head><meta charset=\"utf-8\">" + title + "</head><body>" + page + "</body></html>"
}
