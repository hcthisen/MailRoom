package ui

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// ---------------------------------------------------------------- forms

// Form holds submitted values and the errors to show next to each field
// (template "field" and friends read them).
type Form struct {
	Values  url.Values
	Errors  map[string]string // field name -> message
	Message string            // error not tied to a field, shown above the form
}

// EmptyForm returns a form without values (a "new" page).
func EmptyForm() *Form { return &Form{Values: url.Values{}, Errors: map[string]string{}} }

// NewForm returns the posted form of r (r.ParseForm is called; the
// csrf_token and confirm fields are left out of Values).
func NewForm(r *http.Request) *Form {
	_ = r.ParseForm()
	f := EmptyForm()
	for k, v := range r.PostForm {
		if k == CSRFField || k == confirmField {
			continue
		}
		f.Values[k] = v
	}
	return f
}

// Get returns the trimmed value of a field.
func (f *Form) Get(name string) string {
	if f == nil {
		return ""
	}
	return strings.TrimSpace(f.Values.Get(name))
}

// Raw returns a field's value untrimmed (email bodies).
func (f *Form) Raw(name string) string {
	if f == nil {
		return ""
	}
	return f.Values.Get(name)
}

// All returns every value of a multi-value field (checkbox groups).
func (f *Form) All(name string) []string {
	if f == nil {
		return nil
	}
	return f.Values[name]
}

// Has reports whether value is among the values of name (checked boxes,
// selected options).
func (f *Form) Has(name, value string) bool {
	for _, v := range f.All(name) {
		if v == value {
			return true
		}
	}
	return false
}

// Checked reports whether a checkbox was ticked (any non-empty value
// other than "0", "false" or "off").
func (f *Form) Checked(name string) bool {
	switch f.Get(name) {
	case "", "0", "false", "off":
		return false
	}
	return true
}

// Set sets a value (prefilling an edit form) and returns f.
func (f *Form) Set(name, value string) *Form {
	f.Values.Set(name, value)
	return f
}

// Err returns the error message of a field, or "".
func (f *Form) Err(name string) string {
	if f == nil {
		return ""
	}
	return f.Errors[name]
}

// AddError records an error for field ("" = the form as a whole).
func (f *Form) AddError(field, msg string) {
	if field == "" {
		f.Message = msg
		return
	}
	if _, dup := f.Errors[field]; !dup {
		f.Errors[field] = msg
	}
}

// HasErrors reports whether any error was recorded.
func (f *Form) HasErrors() bool { return f != nil && (len(f.Errors) > 0 || f.Message != "") }

// Fail records a service error on the form, mapped like the API maps it
// (api.MapError): a validation error goes next to its field (a "body."
// prefix is dropped; a field the form doesn't show still gets its name in
// the message), conflicts, refusals and If-Match mismatches go above the
// form. It returns the HTTP status to render the form with, or 0 when err
// is not a user error (not found, internal): the handler then returns err.
func (f *Form) Fail(err error) int {
	se := api.MapError(err)
	status := se.GetStatus()
	var ae *api.Error
	if !errors.As(se, &ae) {
		return 0
	}
	switch status {
	case http.StatusUnprocessableEntity:
		field := strings.TrimPrefix(ae.Body.Field, "body.")
		if field == "" {
			f.AddError("", ae.Body.Message)
		} else {
			f.AddError(field, ae.Body.Message)
			if f.Message == "" {
				f.Message = "Please correct the highlighted field."
				if _, shown := f.Values[field]; !shown {
					f.Message = field + ": " + ae.Body.Message
				}
			}
		}
	case http.StatusConflict, http.StatusForbidden:
		f.AddError("", ae.Body.Message)
	case http.StatusPreconditionFailed:
		f.AddError("", "Someone else changed this in the meantime. Reload the page to see their change, then make yours again.")
	default:
		return 0
	}
	return status
}

// fieldError is a validation error for field (core.Invalid).
func fieldError(field, msg string) error { return core.Invalid(field, "%s", msg) }

// Option is one <option> of template "select-field".
type Option struct {
	Value string
	Label string
}

// ParseTags splits a tag input value (comma or line separated) into
// trimmed, de-duplicated (case-insensitive) tag names. The services
// validate the names themselves.
func ParseTags(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

// PathID parses the integer path value name (404 when it isn't one).
func PathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, core.ErrNotFound
	}
	return id, nil
}

// ---------------------------------------------------------------- lists

// UI page sizes.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ListParams is the pagination of a list page: ?limit= and ?cursor= (the
// same opaque cursor as the API, api.EncodeCursor of the last ID).
type ListParams struct {
	Limit int
	// After is the last ID of the previous page (0 on the first page):
	// pass it as the service's AfterID or BeforeID, whichever way it
	// pages.
	After int64
}

// ParseList reads the list parameters of r. A malformed cursor is a
// validation error (422 page).
func ParseList(r *http.Request) (ListParams, error) {
	p := ListParams{Limit: DefaultPageSize}
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return p, core.Invalid("limit", "must be a positive number")
		}
		p.Limit = min(n, MaxPageSize)
	}
	after, err := api.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return p, err
	}
	p.After = after
	return p, nil
}

// Fetch is the number of rows to ask the service for: one more than the
// page, so Paginate knows whether there is a next page.
func (p ListParams) Fetch() int { return p.Limit + 1 }

// Pager is the pagination bar of a table (template "pager").
type Pager struct {
	Count    int    // items on this page
	NextURL  string // "" on the last page
	FirstURL string // "" on the first page
}

// Paginate trims items (fetched with p.Fetch()) to the page and builds
// the pager; the URLs keep r's other query parameters (filters). id
// returns an item's ID.
func Paginate[T any](r *http.Request, p ListParams, items []T, id func(T) int64) ([]T, Pager) {
	body := api.NewList(items, p.Limit, id)
	pg := Pager{Count: len(body.Items)}
	q := r.URL.Query()
	if body.NextCursor != "" {
		q.Set("cursor", body.NextCursor)
		pg.NextURL = r.URL.Path + "?" + q.Encode()
	}
	if p.After > 0 {
		q.Del("cursor")
		pg.FirstURL = r.URL.Path
		if enc := q.Encode(); enc != "" {
			pg.FirstURL += "?" + enc
		}
	}
	return body.Items, pg
}

// ---------------------------------------------------------------- confirm

// confirmField marks the second step of a confirmed action.
const confirmField = "confirm"

// Confirm is the confirmation step of a destructive action (erase,
// cancel, send, delete): a page that says what will happen and re-posts
// the original form with confirm=yes.
type Confirm struct {
	Title   string   // page title, e.g. "Erase this contact?"
	Message string   // what will happen
	Details []string // facts from the dry run, e.g. "1,204 recipients"
	// Action is the URL the confirmation posts to (usually the same
	// action). Fields are re-posted as hidden inputs (csrf_token and
	// confirm are added).
	Action       string
	Fields       url.Values
	ConfirmLabel string // button text (default "Confirm")
	CancelURL    string // where Cancel goes (default: the dashboard)
	Danger       bool   // red button and border
	Nav          string // highlighted nav item
	Crumbs       []Crumb
}

// Confirmed reports whether r is the confirmed second step.
func Confirmed(r *http.Request) bool { return r.PostFormValue(confirmField) == "yes" }

// RenderConfirm renders the confirmation page.
func (u *UI) RenderConfirm(w http.ResponseWriter, r *http.Request, c Confirm) error {
	if c.ConfirmLabel == "" {
		c.ConfirmLabel = "Confirm"
	}
	if c.CancelURL == "" {
		c.CancelURL = Prefix + "/"
	}
	fields := url.Values{}
	for k, v := range c.Fields {
		if k != CSRFField && k != confirmField {
			fields[k] = v
		}
	}
	c.Fields = fields
	return u.Page(w, r, "shared/confirm", View{Title: c.Title, Nav: c.Nav, Crumbs: c.Crumbs, Data: c})
}
