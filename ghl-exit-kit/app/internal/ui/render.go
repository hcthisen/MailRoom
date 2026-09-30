package ui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"

	"mailroom/internal/api"
)

// templateFS holds every template. Layout and components are shared by
// all pages; every other directory is a page group: each file in it is a
// page (named "<dir>/<file without .html>") that defines "content" and
// optionally "actions" (page header buttons) and partial blocks. Files
// starting with "_" are parsed into every page of their group (shared
// partials). The "all:" prefix is what embeds them: a plain directory
// embed leaves out names starting with "_" or ".".
//
//go:embed all:templates
var templateFS embed.FS

// sharedDirs are parsed into every page.
var sharedDirs = []string{"templates/layout", "templates/components"}

type pageTemplate struct {
	name string
	t    *template.Template
}

// parseTemplates parses every page with the shared templates.
func parseTemplates(funcs template.FuncMap) (map[string]*pageTemplate, error) {
	base := template.New("mailroom").Funcs(funcs)
	for _, dir := range sharedDirs {
		files, err := fs.Glob(templateFS, dir+"/*.html")
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if err := parseFile(base, f); err != nil {
				return nil, err
			}
		}
	}
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		return nil, err
	}
	pages := map[string]*pageTemplate{}
	for _, e := range entries {
		dir := "templates/" + e.Name()
		if !e.IsDir() || dir == sharedDirs[0] || dir == sharedDirs[1] {
			continue
		}
		files, err := fs.Glob(templateFS, dir+"/*.html")
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		var shared, own []string
		for _, f := range files {
			if strings.HasPrefix(path.Base(f), "_") {
				shared = append(shared, f)
			} else {
				own = append(own, f)
			}
		}
		for _, f := range own {
			t, err := base.Clone()
			if err != nil {
				return nil, err
			}
			for _, s := range append(shared, f) {
				if err := parseFile(t, s); err != nil {
					return nil, err
				}
			}
			name := e.Name() + "/" + strings.TrimSuffix(path.Base(f), ".html")
			if t.Lookup("content") == nil {
				return nil, fmt.Errorf("ui: template %s defines no \"content\"", name)
			}
			pages[name] = &pageTemplate{name: name, t: t}
		}
	}
	return pages, nil
}

func parseFile(t *template.Template, name string) error {
	b, err := templateFS.ReadFile(name)
	if err != nil {
		return err
	}
	if _, err := t.New(name).Parse(string(b)); err != nil {
		return fmt.Errorf("ui: parse %s: %w", name, err)
	}
	return nil
}

// Crumb is one breadcrumb link above the page title.
type Crumb struct {
	Label string
	URL   string
}

// View is what every page template gets. Handlers set the first group;
// Page and Partial fill in the rest.
type View struct {
	Title    string  // page title and <h1>; empty = no page header
	Subtitle string  // line under the title
	Nav      string  // key of the highlighted nav item (see navItems)
	Crumbs   []Crumb // breadcrumb above the title
	Data     any     // the page's own data
	Bare     bool    // no top bar and navigation (sign-in, logged-out errors)

	Session     *Session
	CSRF        string // the session's CSRF token (template "csrf" writes the hidden field)
	CSRFHeaders string // JSON for hx-headers on <body>
	Company     string
	Version     string
	Path        string
	Flashes     []Flash
}

// fill completes v for r.
func (u *UI) fill(w http.ResponseWriter, r *http.Request, v *View) {
	st := stateOf(r)
	v.Session = st.session
	if v.Session != nil {
		v.CSRF = v.Session.CSRF
		v.CSRFHeaders = csrfHeaders(v.Session.CSRF)
	} else {
		v.CSRFHeaders = "{}"
	}
	v.Version = u.d.Version
	v.Path = r.URL.Path
	v.Company = u.company(r.Context())
	v.Flashes = append(v.Flashes, u.takeFlashes(w, r)...)
}

// company is the company name for the top bar: the first part of the
// footer (before a comma or line break), else the sender name.
func (u *UI) company(ctx context.Context) string {
	if u.d.Settings == nil {
		return ""
	}
	st, err := u.d.Settings.Get(ctx)
	if err != nil {
		return ""
	}
	name := st.CompanyFooter
	if i := strings.IndexAny(name, ",\n"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = st.Sender.FromName
	}
	if name == "" {
		name = st.Domain
	}
	return name
}

// Page renders the page template name with status 200.
func (u *UI) Page(w http.ResponseWriter, r *http.Request, name string, v View) error {
	return u.PageStatus(w, r, http.StatusOK, name, v)
}

// PageStatus renders the page template name in the layout with status
// (e.g. 422 for a form with errors).
func (u *UI) PageStatus(w http.ResponseWriter, r *http.Request, status int, name string, v View) error {
	p, ok := u.pages[name]
	if !ok {
		return fmt.Errorf("ui: no page template %q", name)
	}
	u.fill(w, r, &v)
	layout := "layout"
	if v.Bare || v.Session == nil {
		layout = "bare"
	}
	var buf bytes.Buffer
	if err := p.t.ExecuteTemplate(&buf, layout, v); err != nil {
		return fmt.Errorf("ui: render %s: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
	return nil
}

// Partial renders one block of a page template (an htmx fragment) with
// status 200. Flashes queued during the request are appended as an
// out-of-band swap into the flash area.
func (u *UI) Partial(w http.ResponseWriter, r *http.Request, name, block string, v View) error {
	return u.PartialStatus(w, r, http.StatusOK, name, block, v)
}

// PartialStatus is Partial with a status. htmx swaps only 2xx responses
// by default, so use it for 2xx codes.
func (u *UI) PartialStatus(w http.ResponseWriter, r *http.Request, status int, name, block string, v View) error {
	p, ok := u.pages[name]
	if !ok {
		return fmt.Errorf("ui: no page template %q", name)
	}
	u.fill(w, r, &v)
	var buf bytes.Buffer
	if err := p.t.ExecuteTemplate(&buf, block, v); err != nil {
		return fmt.Errorf("ui: render %s/%s: %w", name, block, err)
	}
	if len(v.Flashes) > 0 {
		if err := p.t.ExecuteTemplate(&buf, "flash-oob", v); err != nil {
			return fmt.Errorf("ui: render flashes: %w", err)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
	return nil
}

// errorView is the data of the error page.
type errorView struct {
	Status  int
	Message string
}

// Error renders err: 4xx service errors (api.MapError, the same mapping as
// the API) show their message; anything else is logged and shown as a
// generic 500. For htmx requests the message becomes an error flash and
// nothing else on the page changes.
func (u *UI) Error(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "Something went wrong on the server. The error has been logged; try again, and check the logs if it persists."
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		status, msg = http.StatusRequestEntityTooLarge, fmt.Sprintf("The request is too large (the limit is %d bytes).", tooBig.Limit)
	case errors.Is(err, context.Canceled):
		return // the client went away
	default:
		se := api.MapError(err)
		if s := se.GetStatus(); s < 500 {
			status = s
			var ae *api.Error
			if errors.As(se, &ae) {
				msg = ae.Body.Message
				if ae.Body.Field != "" {
					msg = ae.Body.Field + ": " + msg
				}
			}
		} else {
			u.log.ErrorContext(r.Context(), "ui: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		}
	}
	if isHTMX(r) {
		u.Flash(r, FlashError, msg)
		w.Header().Set("HX-Reswap", "none")
		if rerr := u.PartialStatus(w, r, http.StatusOK, "errors/error", "none", View{}); rerr != nil {
			http.Error(w, msg, status)
		}
		return
	}
	u.renderError(w, r, status, "", msg)
}

// NotFound renders the 404 page.
func (u *UI) NotFound(w http.ResponseWriter, r *http.Request) {
	u.renderError(w, r, http.StatusNotFound, "Page not found", "There is no page at this address. It may have been deleted, or the link is wrong.")
}

// renderError renders the error page with status. On a public page (the
// login form) it still shows the navigation when the browser has a valid
// session.
func (u *UI) renderError(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	if title == "" {
		title = errorTitle(status)
	}
	st := stateOf(r)
	if st.session == nil {
		if c, err := r.Cookie(api.SessionCookie); err == nil && c.Value != "" {
			if p, csrf, err := u.d.Auth.AuthenticateSession(r.Context(), c.Value); err == nil {
				st.session = &Session{Email: p.Actor.Name, CSRF: csrf, Actor: p.Actor}
			}
		}
	}
	if err := u.PageStatus(w, r, status, "errors/error", View{Title: title, Data: errorView{Status: status, Message: msg}}); err != nil {
		u.log.ErrorContext(r.Context(), "ui: render error page", "err", err)
		http.Error(w, msg, status)
	}
}

func errorTitle(status int) string {
	switch status {
	case http.StatusNotFound:
		return "Page not found"
	case http.StatusForbidden:
		return "Not allowed"
	case http.StatusConflict:
		return "That can't be done right now"
	case http.StatusPreconditionFailed:
		return "Changed by someone else"
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return "Check the input"
	case http.StatusTooManyRequests:
		return "Too many requests"
	case http.StatusRequestEntityTooLarge:
		return "Too large"
	}
	if status >= 500 {
		return "Something went wrong"
	}
	return http.StatusText(status)
}
