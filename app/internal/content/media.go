package content

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// MaxMediaBytes is the largest accepted upload (E-6).
const MaxMediaBytes = 5 << 20

// MaxMediaFilenameLength bounds the stored original file name.
const MaxMediaFilenameLength = 200

// mediaTypes are the accepted image types (by content sniffing) and the
// extension of their stored files. SVG is refused: it can carry scripts.
var mediaTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// rxStoredName matches the random names media files are stored under.
var rxStoredName = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|gif|webp)$`)

// Media is an uploaded image, served publicly at URL.
type Media struct {
	ID          int64     `json:"id" example:"7"`
	Filename    string    `json:"filename" doc:"The uploaded file's original name" example:"logo.png"`
	StoredName  string    `json:"stored_name" doc:"Random name the file is stored and served under" example:"3f9a0c1b2d4e5f60718293a4b5c6d7e8.png"`
	ContentType string    `json:"content_type" enum:"image/png,image/jpeg,image/gif,image/webp" example:"image/png"`
	SizeBytes   int64     `json:"size_bytes" example:"48213"`
	URL         string    `json:"url" doc:"Public URL to use in emails and the layout" example:"https://newsletter.example.com/media/3f9a0c1b2d4e5f60718293a4b5c6d7e8.png"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Service) mediaURL(stored string) string { return s.baseURL + "/media/" + stored }

func (s *Service) scanMedia(sc interface{ Scan(...any) error }) (Media, error) {
	var m Media
	var created string
	if err := sc.Scan(&m.ID, &m.Filename, &m.StoredName, &m.ContentType, &m.SizeBytes, &created); err != nil {
		return Media{}, err
	}
	t, err := db.ParseTime(created)
	if err != nil {
		return Media{}, err
	}
	m.CreatedAt, m.URL = t, s.mediaURL(m.StoredName)
	return m, nil
}

const mediaColumns = `id, filename, stored_name, content_type, size_bytes, created_at`

// UploadMedia stores an image from r under a random name in the media
// directory and records it. Only PNG, JPEG, GIF and WebP images (detected
// from the content, not the name) up to MaxMediaBytes are accepted.
func (s *Service) UploadMedia(ctx context.Context, filename string, r io.Reader) (Media, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxMediaBytes+1))
	if err != nil {
		return Media{}, invalid("file", "could not be read: %v", err)
	}
	switch {
	case len(data) == 0:
		return Media{}, invalid("file", "is empty")
	case len(data) > MaxMediaBytes:
		return Media{}, invalid("file", "is larger than %d MiB", MaxMediaBytes>>20)
	}
	ct, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	ext, ok := mediaTypes[ct]
	if !ok {
		return Media{}, invalid("file", "must be a PNG, JPEG, GIF or WebP image (this looks like %s)", ct)
	}
	if s.mediaDir == "" {
		return Media{}, errors.New("content: media directory is not configured")
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return Media{}, fmt.Errorf("content: random name: %w", err)
	}
	stored := hex.EncodeToString(rnd[:]) + ext
	if err := s.writeMediaFile(stored, data); err != nil {
		return Media{}, err
	}
	now := s.clock.Now()
	m := Media{Filename: cleanFilename(filename, ext), StoredName: stored, ContentType: ct,
		SizeBytes: int64(len(data)), URL: s.mediaURL(stored), CreatedAt: now.UTC()}
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO media (filename, stored_name, content_type, size_bytes, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			m.Filename, m.StoredName, m.ContentType, m.SizeBytes, db.FormatTime(now)).Scan(&m.ID); err != nil {
			return fmt.Errorf("content: insert media: %w", err)
		}
		return audit.Write(ctx, tx, audit.Entry{
			Operation: "media.upload", TargetType: "media", TargetID: strconv.FormatInt(m.ID, 10), At: now,
			Changes: map[string]any{"filename": m.Filename, "content_type": m.ContentType, "size_bytes": m.SizeBytes},
		})
	})
	if err != nil {
		s.removeMediaFile(stored)
		return Media{}, err
	}
	return m, nil
}

// writeMediaFile writes data atomically (temp file + rename) as a
// non-executable, world-readable file.
func (s *Service) writeMediaFile(stored string, data []byte) error {
	if err := os.MkdirAll(s.mediaDir, 0o755); err != nil {
		return fmt.Errorf("content: create media directory: %w", err)
	}
	f, err := os.CreateTemp(s.mediaDir, ".upload-*")
	if err != nil {
		return fmt.Errorf("content: create media file: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(0o644)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(s.mediaDir, stored))
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("content: write media file: %w", werr)
	}
	return nil
}

func (s *Service) removeMediaFile(stored string) {
	if err := os.Remove(filepath.Join(s.mediaDir, stored)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("content: remove media file", "stored_name", stored, "err", err)
	}
}

// cleanFilename keeps the base name of an uploaded file without control
// characters, at most MaxMediaFilenameLength characters.
func cleanFilename(name, ext string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxMediaFilenameLength {
		name = string([]rune(name)[:MaxMediaFilenameLength])
	}
	if name == "" || name == "." || name == ".." {
		name = "image" + ext
	}
	return name
}

// GetMedia returns one media item. Unknown IDs wrap core.ErrNotFound.
func (s *Service) GetMedia(ctx context.Context, id int64) (Media, error) {
	m, err := s.scanMedia(s.db.Read.QueryRowContext(ctx, `SELECT `+mediaColumns+` FROM media WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, fmt.Errorf("media %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return Media{}, fmt.Errorf("content: get media %d: %w", id, err)
	}
	return m, nil
}

// ListMedia returns up to limit media items with IDs after afterID.
func (s *Service) ListMedia(ctx context.Context, afterID int64, limit int) ([]Media, error) {
	rows, err := s.db.Read.QueryContext(ctx,
		`SELECT `+mediaColumns+` FROM media WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("content: list media: %w", err)
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		m, err := s.scanMedia(rows)
		if err != nil {
			return nil, fmt.Errorf("content: list media: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMedia deletes a media item and its file. It refuses
// (core.ErrConflict) while an email or the layout refers to the file.
func (s *Service) DeleteMedia(ctx context.Context, id int64) error {
	var stored string
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		m, err := s.scanMedia(tx.QueryRowContext(ctx, `SELECT `+mediaColumns+` FROM media WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("media %d: %w", id, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("content: delete media %d: %w", id, err)
		}
		var emailID int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM emails WHERE instr(body, ?) > 0 ORDER BY id LIMIT 1`, m.StoredName).Scan(&emailID)
		switch {
		case err == nil:
			return fmt.Errorf("media %d is used by email %d: %w", id, emailID, core.ErrConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("content: media usage: %w", err)
		}
		l, err := s.GetLayout(ctx, tx)
		if err != nil {
			return err
		}
		if strings.Contains(l.LogoURL, m.StoredName) || strings.Contains(l.CustomHTML, m.StoredName) {
			return fmt.Errorf("media %d is used by the layout: %w", id, core.ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE id = ?`, id); err != nil {
			return fmt.Errorf("content: delete media %d: %w", id, err)
		}
		stored = m.StoredName
		return audit.Write(ctx, tx, audit.Entry{
			Operation: "media.delete", TargetType: "media", TargetID: strconv.FormatInt(id, 10), At: s.clock.Now(),
			Changes: map[string]any{"filename": m.Filename},
		})
	})
	if err != nil {
		return err
	}
	s.removeMediaFile(stored)
	return nil
}

// MediaHandler serves uploaded media publicly (GET and HEAD). Mount it at
// "GET /media/{name}" (the name is taken from the {name} path value, or
// else from the last path segment). Files are served with the content
// type recorded at upload, nosniff and a one-year immutable cache (stored
// names are random and never reused).
func (s *Service) MediaHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.PathValue("name")
		if name == "" {
			name = path.Base(r.URL.Path)
		}
		if !rxStoredName.MatchString(name) || s.mediaDir == "" {
			http.NotFound(w, r)
			return
		}
		var ct, created string
		err := s.db.Read.QueryRowContext(r.Context(),
			`SELECT content_type, created_at FROM media WHERE stored_name = ?`, name).Scan(&ct, &created)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				s.log.ErrorContext(r.Context(), "content: media lookup", "err", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(s.mediaDir, name))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.log.ErrorContext(r.Context(), "content: open media file", "stored_name", name, "err", err)
			}
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		modTime, _ := db.ParseTime(created)
		h := w.Header()
		h.Set("Content-Type", ct)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		http.ServeContent(w, r, "", modTime, f)
	})
}
