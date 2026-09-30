package content

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
	"mailroom/internal/tokens"
)

// Deps are the Service's dependencies.
type Deps struct {
	DB       *db.DB
	Settings *settings.Service
	// Signer signs the unsubscribe, click and open tokens.
	Signer *tokens.Signer
	// Contacts reads contacts for previews and test sends. Nil means a
	// store on DB without hooks (only reads are used).
	Contacts *contactstore.Store
	// BaseURL is the public URL of the install (config.Config.BaseURL),
	// used for /u, /c, /o and /media links.
	BaseURL string
	// DataDir is the data directory (config.Config.DataDir); media files
	// are stored in DataDir/media.
	DataDir string
	Clock   core.Clock
	Logger  *slog.Logger
}

// Service manages emails, the layout and media, and renders messages.
type Service struct {
	db       *db.DB
	settings *settings.Service
	signer   *tokens.Signer
	contacts *contactstore.Store
	baseURL  string
	mediaDir string
	clock    core.Clock
	log      *slog.Logger
	md       goldmark.Markdown
	links    *linkCache
}

var _ core.Renderer = (*Service)(nil)

// New returns a content service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Contacts == nil {
		d.Contacts = &contactstore.Store{DB: d.DB, Clock: d.Clock}
	}
	s := &Service{
		db:       d.DB,
		settings: d.Settings,
		signer:   d.Signer,
		contacts: d.Contacts,
		baseURL:  strings.TrimSuffix(d.BaseURL, "/"),
		clock:    d.Clock,
		log:      d.Logger,
		md:       newMarkdown(),
		links:    newLinkCache(linkCacheSize),
	}
	if d.DataDir != "" {
		s.mediaDir = filepath.Join(d.DataDir, "media")
	}
	return s
}

// invalid is core.Invalid; a short local name keeps validation readable.
func invalid(field, format string, args ...any) error {
	return core.Invalid(field, format, args...)
}

// contactValues returns the merge values of c (nil: no values, so every
// field uses its fallback) plus the unsubscribe URL.
func contactValues(c *core.Contact, unsubscribeURL string) values {
	return func(name string) (string, bool) {
		if name == fieldUnsubscribe {
			return unsubscribeURL, unsubscribeURL != ""
		}
		if c == nil {
			return "", false
		}
		switch name {
		case fieldFirstName:
			return c.FirstName, true
		case fieldLastName:
			return c.LastName, true
		case fieldEmail:
			return c.Email, true
		}
		key, ok := strings.CutPrefix(name, attrPrefix)
		if !ok {
			return "", false
		}
		v, ok := c.Attributes[key]
		if !ok || v == nil {
			return "", false
		}
		return formatAttr(v), true
	}
}

// formatAttr formats an attribute value from JSON for display.
func formatAttr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) && x < 1e15 && x > -1e15 {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case fmt.Stringer:
		return x.String()
	}
	s, err := db.MarshalJSON(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return s
}
