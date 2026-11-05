// Package ui is Mailroom's admin UI (PRD §6.11 A-1..A-4): server-rendered
// html/template pages with htmx (vendored, see static/VENDOR.md), no build
// step and no external assets. It is mounted under /admin/ on the main
// handler.
//
// Every UI action that reads or changes data calls the same service method
// the corresponding API operation calls and is registered with handle,
// naming that operation's ID (PRD G-1). The parity test checks that every
// named operation exists and that every API write has a UI action or a
// justified allowlist entry.
//
// Page groups (contacts, segments, broadcasts, ...) live in their own
// files, internal/ui/<group>_*.go with templates in templates/<group>/,
// and register their actions from an init function:
//
//	func init() {
//		registerGroup("contacts", func(u *UI) {
//			u.handle(Action{Method: http.MethodGet, Path: "/admin/contacts", OperationID: "list-contacts", Handler: u.contactsList})
//		})
//	}
package ui

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"mailroom/internal/audit"
	"mailroom/internal/auth"
	"mailroom/internal/broadcasts"
	"mailroom/internal/config"
	"mailroom/internal/contacts"
	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/forms"
	"mailroom/internal/imports"
	"mailroom/internal/inbound"
	"mailroom/internal/jobs"
	"mailroom/internal/segments"
	"mailroom/internal/sending"
	"mailroom/internal/sequences"
	"mailroom/internal/settings"
	"mailroom/internal/stats"
	"mailroom/internal/system"
)

// Prefix is the path every UI page is under.
const Prefix = "/admin"

// Deps are the services the UI calls. internal/app passes the ones it
// built for the API, so the UI and the API share every rule.
type Deps struct {
	// Config supplies the base URL (secure cookies when https), the
	// trusted proxies (client address for the login limits) and the secret
	// (flash cookie signatures).
	Config  config.Config
	Version string       // shown in the footer (default "dev")
	Clock   core.Clock   // default core.SystemClock
	Logger  *slog.Logger // default slog.Default()

	// DB is only for db.WithTx around service methods that take the
	// caller's transaction (as the API handlers do). UI code never runs
	// SQL itself.
	DB *db.DB

	Auth       *auth.Service
	Settings   *settings.Service
	Audit      *audit.Service
	Jobs       *jobs.Runner
	Contacts   *contacts.Service
	Segments   *segments.Service
	Content    *content.Service
	Sending    *sending.Service
	System     *system.Service
	Sequences  *sequences.Service
	Broadcasts *broadcasts.Service
	Forms      *forms.Service
	Imports    *imports.Service
	Stats      *stats.Service
	// Inbound is the Inbox (received email). Nil hides it.
	Inbound *inbound.Service

	// DoctorInterval is how often DoctorWorker runs the doctor checks
	// (default DefaultDoctorInterval).
	DoctorInterval time.Duration
}

// HandlerFunc is a UI handler. A returned error is rendered as an error
// page (or, for htmx requests, as an error flash) by Error.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Action is one UI route. Every action that reads or changes data names
// the API operation it corresponds to (PRD G-1).
type Action struct {
	Method string // http.MethodGet, http.MethodPost, ...
	// Path is the full ServeMux path pattern, starting with Prefix + "/"
	// (e.g. "/admin/contacts/{id}").
	Path string
	// OperationID is the API operation this action performs (its
	// service method is what the handler calls).
	OperationID string
	// Uses lists further API operations whose service methods the
	// handler also calls (e.g. a page that shows a list and a count).
	Uses []string
	// UIOnly explains why an action has no API operation (login, logout,
	// static files). Required when OperationID is empty.
	UIOnly string
	// Public actions don't need a session (the login page). Everything
	// else redirects to the login page when logged out and checks the
	// CSRF token on every non-GET request.
	Public bool
	// MaxBodyBytes limits the request body (default DefaultMaxBody).
	MaxBodyBytes int64
	Handler      HandlerFunc
}

// DefaultMaxBody is the request body limit of an action that sets none:
// enough for an email body (content.MaxBodyBytes) as a form.
const DefaultMaxBody = 3 << 20

// UI is the admin UI.
type UI struct {
	d      Deps
	log    *slog.Logger
	clock  core.Clock
	mux    *http.ServeMux
	secure bool // the install is served over https: Secure cookies

	actions []Action
	pages   map[string]*pageTemplate
	assets  map[string]asset
	doctor  doctorCache
}

// groups are the page groups registered by registerGroup, in name order
// when New runs.
var (
	groupsMu sync.Mutex
	groups   = map[string]func(*UI){}
)

// registerGroup adds a page group; its function registers the group's
// actions with u.handle when New runs. Call it from an init function.
func registerGroup(name string, fn func(*UI)) {
	groupsMu.Lock()
	defer groupsMu.Unlock()
	if _, dup := groups[name]; dup {
		panic("ui: duplicate page group " + name)
	}
	groups[name] = fn
}

// New builds the UI: it parses the templates, loads the static files and
// registers every action. It panics on a template error or a duplicate
// route, which are programming errors caught by any test.
func New(d Deps) *UI {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Version == "" {
		d.Version = "dev"
	}
	if d.DoctorInterval <= 0 {
		d.DoctorInterval = DefaultDoctorInterval
	}
	u := &UI{d: d, log: d.Logger, clock: d.Clock, mux: http.NewServeMux()}
	if base, err := url.Parse(d.Config.BaseURL); err == nil && base.Scheme == "https" {
		u.secure = true
	}
	u.assets = loadAssets()
	pages, err := parseTemplates(u.funcs())
	if err != nil {
		panic(err)
	}
	u.pages = pages

	u.registerFoundation()
	u.registerEditor()
	groupsMu.Lock()
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	fns := make([]func(*UI), len(names))
	for i, n := range names {
		fns[i] = groups[n]
	}
	groupsMu.Unlock()
	for _, fn := range fns {
		fn(u)
	}
	// Anything else under /admin/ is a 404 page, for signed-in users only:
	// logged out, every address goes to the login page, so the UI doesn't
	// tell strangers which pages exist.
	u.mux.Handle(Prefix+"/", u.base(Action{}, u.requireSession(func(w http.ResponseWriter, r *http.Request) error {
		u.NotFound(w, r)
		return nil
	})))
	return u
}

// Handler returns the UI handler. Mount it at Prefix + "/".
func (u *UI) Handler() http.Handler { return u.mux }

// RootRedirect answers GET / with a redirect to the dashboard.
func RootRedirect() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"/", http.StatusFound)
	})
}

// handle registers a. It panics on an invalid action: a path outside
// Prefix, a missing handler, or neither an OperationID nor a UIOnly
// reason.
func (u *UI) handle(a Action) {
	switch {
	case a.Handler == nil:
		panic("ui: " + a.Method + " " + a.Path + ": no handler")
	case !strings.HasPrefix(a.Path, Prefix+"/"):
		panic("ui: " + a.Path + ": path must start with " + Prefix + "/")
	case a.Method == "":
		panic("ui: " + a.Path + ": method is required")
	case a.OperationID == "" && a.UIOnly == "":
		panic(fmt.Sprintf("ui: %s %s: set OperationID (PRD G-1) or explain UIOnly", a.Method, a.Path))
	}
	h := a.Handler
	if !a.Public {
		h = u.requireSession(h)
	}
	u.mux.Handle(a.Method+" "+a.Path, u.base(a, h))
	u.actions = append(u.actions, a)
}

// Actions returns every registered action, sorted by path then method,
// for the parity test (PRD G-1).
func (u *UI) Actions() []Action {
	out := slices.Clone(u.actions)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// tx runs fn in a write transaction with the request context, for service
// methods that take the caller's transaction (exactly as the API handlers
// call them).
func (u *UI) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return u.d.DB.WithTx(ctx, fn)
}
