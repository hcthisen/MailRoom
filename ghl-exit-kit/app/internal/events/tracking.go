package events

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"mailroom/internal/tokens"
)

// recordTimeout bounds recording a click or open, so a busy write
// connection delays the redirect or pixel only briefly.
const recordTimeout = 2 * time.Second

// transparentGIF is a 1x1 transparent GIF89a.
var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00,
	0xff, 0xff, 0xff, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44, 0x01, 0x00, 0x3b,
}

// handleClick serves GET /c/{token} (K-1): it records the click with its
// link, message and contact, then redirects with 302 to the link's URL.
// The target is only ever an http(s) URL from the links table, never
// anything from the request. Recording is best effort (rate-limited per
// IP, skipped for HEAD and when click tracking is off): a known link
// always redirects.
func (s *Service) handleClick(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pl, err := s.signer.Verify(tokens.PurposeClick, r.PathValue("token"))
	if err != nil || pl.LinkID <= 0 {
		s.renderPage(w, http.StatusNotFound, notFoundPage())
		return
	}
	target, err := s.linkURL(ctx, pl.LinkID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Error("events: look up link", "err", err, "link_id", pl.LinkID)
		}
		s.renderPage(w, http.StatusNotFound, notFoundPage())
		return
	}
	if r.Method == http.MethodGet && s.appSettings(ctx).Tracking.Clicks && s.limiter.Allow(s.clientKey(r)).Allowed {
		s.recordClick(ctx, pl)
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// linkURL returns the URL of a links row when it is an absolute http(s)
// URL; anything else is treated like a missing row (sql.ErrNoRows).
func (s *Service) linkURL(ctx context.Context, id int64) (string, error) {
	var raw string
	if err := s.db.Read.QueryRowContext(ctx, `SELECT url FROM links WHERE id = ?`, id).Scan(&raw); err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		s.log.Warn("events: link is not an http(s) URL", "link_id", id)
		return "", sql.ErrNoRows
	}
	return raw, nil
}

// messageContactSQL selects the contact of message ?1 when it is still
// the token's contact ?2. The contact is always taken from the message, so
// an old token of an erased contact is never attributed to a newer
// contact that reused its id.
const messageContactSQL = `(SELECT contact_id FROM messages WHERE id = ?1 AND contact_id = ?2)`

// recordClick inserts the clicks row. The message is stored only while it
// exists and the contact only while it still owns that message (a deleted
// broadcast or an erased contact leaves NULL), so the insert never fails
// on a foreign key.
func (s *Service) recordClick(ctx context.Context, pl tokens.Payload) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	_, err := s.db.Write.ExecContext(ctx, `
		INSERT INTO clicks (link_id, message_id, contact_id, created_at)
		VALUES (?3, (SELECT id FROM messages WHERE id = ?1), `+messageContactSQL+`, ?4)`,
		pl.MessageID, pl.ContactID, pl.LinkID, s.now())
	if err != nil {
		s.log.Warn("events: record click", "err", err, "link_id", pl.LinkID, "message_id", pl.MessageID)
	}
}

// handleOpen serves GET /o/{token} (K-2): a 1x1 transparent GIF that is
// never cached. A valid token records an open (rate-limited per IP, not
// for HEAD, and only while open tracking is on); an invalid one still gets
// the image, so mail clients show no broken picture.
func (s *Service) handleOpen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pl, err := s.signer.Verify(tokens.PurposeOpen, r.PathValue("token"))
	if err == nil && pl.MessageID > 0 && r.Method == http.MethodGet &&
		s.appSettings(ctx).Tracking.Opens && s.limiter.Allow(s.clientKey(r)).Allowed {
		s.recordOpen(ctx, pl)
	}
	h := w.Header()
	h.Set("Content-Type", "image/gif")
	h.Set("Content-Length", strconv.Itoa(len(transparentGIF)))
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(transparentGIF)
	}
}

// recordOpen inserts the opens row while the message exists; the contact
// is stored only while it still owns the message (NULL after an erase).
func (s *Service) recordOpen(ctx context.Context, pl tokens.Payload) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	_, err := s.db.Write.ExecContext(ctx, `
		INSERT INTO opens (message_id, contact_id, created_at)
		SELECT m.id, `+messageContactSQL+`, ?3 FROM messages m WHERE m.id = ?1`,
		pl.MessageID, pl.ContactID, s.now())
	if err != nil {
		s.log.Warn("events: record open", "err", err, "message_id", pl.MessageID)
	}
}
