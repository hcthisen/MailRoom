package ui

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"mailroom/internal/forms"
	"mailroom/internal/sequences"
)

// Sign-up form pages (PRD §6.3 F-1..F-7): list, create and edit (fields,
// tags, sequence, double opt-in, consent text, redirect URL, allowed
// origins), delete, the embed snippet with the hosted URL, and the recent
// submissions.

// webformPath is the UI address of the forms list.
const webformPath = Prefix + "/forms"

// webformSequenceLimit bounds the sequences offered in a form's sequence
// choice.
const webformSequenceLimit = 500

func (u *UI) registerWebforms() {
	u.handle(Action{Method: http.MethodGet, Path: webformPath, OperationID: "list-forms", Uses: []string{"list-sequences"},
		Handler: u.webformList})
	u.handle(Action{Method: http.MethodGet, Path: webformPath + "/new", OperationID: "list-sequences", Uses: []string{"list-tags"},
		Handler: u.webformNew})
	u.handle(Action{Method: http.MethodPost, Path: webformPath, OperationID: "create-form", Uses: []string{"list-sequences", "list-tags"},
		Handler: u.webformCreate})
	u.handle(Action{Method: http.MethodGet, Path: webformPath + "/{id}", OperationID: "get-form", Uses: []string{"list-sequences", "list-tags"},
		Handler: u.webformEdit})
	u.handle(Action{Method: http.MethodPost, Path: webformPath + "/{id}", OperationID: "update-form", Uses: []string{"list-sequences", "list-tags"},
		Handler: u.webformUpdate})
	u.handle(Action{Method: http.MethodPost, Path: webformPath + "/{id}/delete", OperationID: "delete-form", Uses: []string{"get-form"},
		Handler: u.webformDelete})
	u.handle(Action{Method: http.MethodGet, Path: webformPath + "/{id}/snippet", OperationID: "get-form-snippet", Uses: []string{"get-form"},
		Handler: u.webformSnippet})
	u.handle(Action{Method: http.MethodGet, Path: webformPath + "/{id}/submissions", OperationID: "list-form-submissions",
		Uses: []string{"get-form"}, Handler: u.webformSubmissions})
}

// webformURL is the UI address of form id, plus suffix.
func webformURL(id int64, suffix string) string {
	return webformPath + "/" + strconv.FormatInt(id, 10) + suffix
}

func webformCrumbs(f forms.Form) []Crumb {
	return []Crumb{{"Forms", webformPath}, {f.Name, webformURL(f.ID, "")}}
}

// webformSequences lists the sequences for the sequence choice and by ID
// for the list.
func (u *UI) webformSequences(r *http.Request) ([]Option, map[int64]string) {
	opts := []Option{{"0", "None"}}
	names := map[int64]string{}
	list, err := u.d.Sequences.List(r.Context(), 0, webformSequenceLimit)
	if err != nil {
		u.log.WarnContext(r.Context(), "ui: sequences for forms", "err", err)
		return opts, names
	}
	for _, s := range list {
		label := s.Name
		if s.Status == sequences.StatusPaused {
			label += " (paused)"
		}
		opts = append(opts, Option{strconv.FormatInt(s.ID, 10), label})
		names[s.ID] = s.Name
	}
	return opts, names
}

// ---------------------------------------------------------------- list

type webformListItem struct {
	Form     forms.Form
	Sequence string
}

type webformListData struct {
	Items []webformListItem
	Pager Pager
}

// webformList lists the sign-up forms.
func (u *UI) webformList(w http.ResponseWriter, r *http.Request) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	list, err := u.d.Forms.List(r.Context(), p.After, p.Fetch())
	if err != nil {
		return err
	}
	page, pager := Paginate(r, p, list, func(f forms.Form) int64 { return f.ID })
	_, names := u.webformSequences(r)
	d := webformListData{Pager: pager}
	for _, f := range page {
		it := webformListItem{Form: f}
		if f.SequenceID != nil {
			it.Sequence = names[*f.SequenceID]
		}
		d.Items = append(d.Items, it)
	}
	return u.Page(w, r, "automation/form_list", View{Title: "Forms", Nav: "forms",
		Subtitle: "Sign-up forms for your website, or as a hosted page to link to.", Data: d})
}

// ---------------------------------------------------------------- create and edit

// webformField is one row of the fields editor. Kind is first_name,
// last_name, attr (Attr holds the attribute key) or "" (an unused row).
type webformField struct {
	Index    int // 0-based, as in the field names
	Num      int // Index + 1, for people
	Kind     string
	Attr     string
	Label    string
	Required bool
	Err      string
}

type webformData struct {
	Tabs      template.HTML // "" on the new page
	Form      *Form
	Web       *forms.Form // nil on the new page
	Fields    []webformField
	Sequences []Option
	Tags      []string
	Default   string // forms.DefaultConsentText
}

// webformSpareRows is the number of empty field rows under the used ones.
const webformSpareRows = 3

func (u *UI) webformView(w http.ResponseWriter, r *http.Request, status int, f *Form, web *forms.Form) error {
	seqs, _ := u.webformSequences(r)
	d := webformData{Form: f, Web: web, Sequences: seqs, Tags: u.autoTagSuggestions(r.Context()),
		Default: forms.DefaultConsentText, Fields: webformRows(f)}
	v := View{Title: "New form", Nav: "forms", Crumbs: []Crumb{{"Forms", webformPath}}}
	if web != nil {
		v.Title = web.Name
		tabs, err := webformTabs(*web, "settings")
		if err != nil {
			return err
		}
		d.Tabs = tabs
	}
	v.Data = d
	return u.PageStatus(w, r, status, "automation/form_edit", v)
}

// webformRows are the field rows of f (its fields[i].* values) plus spare
// empty rows, up to forms.MaxFields.
func webformRows(f *Form) []webformField {
	var rows []webformField
	for i := 0; i < forms.MaxFields; i++ {
		p := "fields[" + strconv.Itoa(i) + "]."
		if _, ok := f.Values[p+"kind"]; !ok {
			break
		}
		row := webformField{Index: i, Num: i + 1, Kind: f.Get(p + "kind"), Attr: f.Get(p + "attr"), Label: f.Get(p + "label"),
			Required: f.Checked(p + "required")}
		for _, k := range []string{"key", "label", "required"} {
			if e := f.Err(p + k); e != "" && row.Err == "" {
				row.Err = e
			}
		}
		rows = append(rows, row)
	}
	for n := 0; n < webformSpareRows && len(rows) < forms.MaxFields; n++ {
		rows = append(rows, webformField{Index: len(rows), Num: len(rows) + 1})
	}
	return rows
}

// webformSetFields writes fields into f as rows 0..n-1 (and drops other
// rows), so a validation error "fields[i].key" lands on row i.
func webformSetFields(f *Form, fields []forms.FormField) {
	for k := range f.Values {
		if strings.HasPrefix(k, "fields[") {
			delete(f.Values, k)
		}
	}
	for i, fl := range fields {
		p := "fields[" + strconv.Itoa(i) + "]."
		kind, attr := fl.Key, ""
		if a, ok := strings.CutPrefix(fl.Key, forms.AttrPrefix); ok {
			kind, attr = "attr", a
		} else if fl.Key != forms.FieldFirstName && fl.Key != forms.FieldLastName {
			kind, attr = "attr", fl.Key
		}
		f.Set(p+"kind", kind).Set(p+"attr", attr).Set(p+"label", fl.Label)
		if fl.Required {
			f.Set(p+"required", "1")
		}
	}
}

// webformParseFields reads the used field rows in order.
func webformParseFields(f *Form) []forms.FormField {
	out := []forms.FormField{}
	for i := 0; i < forms.MaxFields+webformSpareRows; i++ {
		p := "fields[" + strconv.Itoa(i) + "]."
		if _, ok := f.Values[p+"kind"]; !ok {
			break
		}
		kind := f.Get(p + "kind")
		var key string
		switch kind {
		case "":
			continue
		case "attr":
			key = forms.AttrPrefix + f.Get(p+"attr")
		default:
			key = kind
		}
		out = append(out, forms.FormField{Key: key, Label: f.Get(p + "label"), Required: f.Checked(p + "required")})
	}
	return out
}

// webformInput is what a form page posted.
type webformInput struct {
	in      forms.FormInput
	origins []string
}

// webformParse reads the create/edit form. The field rows are rewritten
// in their compacted order (see webformSetFields).
func webformParse(f *Form) (webformInput, error) {
	var p webformInput
	fields := webformParseFields(f)
	webformSetFields(f, fields)
	seqID, err := strconv.ParseInt(f.Get("sequence_id"), 10, 64)
	if f.Get("sequence_id") == "" {
		seqID, err = 0, nil
	}
	if err != nil {
		return p, fieldError("sequence_id", "choose a sequence")
	}
	p.origins = autoSplitList(f.Get("allowed_origins"))
	p.in = forms.FormInput{
		Name: f.Get("name"), Fields: fields, Tags: ParseTags(f.Get("tags")), SequenceID: &seqID,
		DoubleOptin: f.Checked("double_optin"), ConsentText: f.Get("consent_text"), RedirectURL: f.Get("redirect_url"),
		AllowedOrigins: p.origins,
	}
	if p.in.Tags == nil {
		p.in.Tags = []string{}
	}
	return p, nil
}

// webformFields puts list errors on their inputs.
func webformFields(p webformInput) func(string, string) (string, string) {
	origins := autoFoldIndex("allowed_origins", p.origins)
	tags := autoFoldIndex("tags", p.in.Tags)
	return func(field, msg string) (string, string) {
		if strings.HasPrefix(field, "allowed_origins") {
			return origins(field, msg)
		}
		if strings.HasPrefix(field, "tags") {
			return tags(field, msg)
		}
		return field, msg
	}
}

// webformValues fills the edit form from a stored form. The default
// consent text is shown as the placeholder, not as a value.
func webformValues(web forms.Form) *Form {
	f := EmptyForm().Set("name", web.Name).Set("tags", strings.Join(web.Tags, ", ")).
		Set("redirect_url", web.RedirectURL).Set("allowed_origins", strings.Join(web.AllowedOrigins, "\n")).
		Set("version", strconv.FormatInt(web.Version, 10)).Set("sequence_id", "0")
	if web.SequenceID != nil {
		f.Set("sequence_id", strconv.FormatInt(*web.SequenceID, 10))
	}
	if web.DoubleOptin {
		f.Set("double_optin", "1")
	}
	if web.ConsentText != forms.DefaultConsentText {
		f.Set("consent_text", web.ConsentText)
	}
	webformSetFields(f, web.Fields)
	return f
}

// webformNew is the create form: email plus first name, double opt-in off.
func (u *UI) webformNew(w http.ResponseWriter, r *http.Request) error {
	f := EmptyForm().Set("sequence_id", "0")
	webformSetFields(f, []forms.FormField{{Key: forms.FieldFirstName, Label: "First name"}})
	return u.webformView(w, r, http.StatusOK, f, nil)
}

// webformCreate creates a form (create-form).
func (u *UI) webformCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	p, err := webformParse(f)
	if err == nil {
		var web forms.Form
		web, err = u.d.Forms.Create(r.Context(), p.in, false)
		if err == nil {
			u.Flash(r, FlashSuccess, fmt.Sprintf("Form %q created. Put it on your website with the snippet below, or link to its hosted page.", web.Name))
			u.Redirect(w, r, webformURL(web.ID, "/snippet"))
			return nil
		}
		err = autoRetarget(err, webformFields(p))
	}
	if code := f.Fail(err); code != 0 {
		return u.webformView(w, r, code, f, nil)
	}
	return err
}

// webformGet loads the form of the request's {id}.
func (u *UI) webformGet(r *http.Request) (forms.Form, error) {
	id, err := PathID(r, "id")
	if err != nil {
		return forms.Form{}, err
	}
	return u.d.Forms.Get(r.Context(), id)
}

// webformEdit is a form's settings page.
func (u *UI) webformEdit(w http.ResponseWriter, r *http.Request) error {
	web, err := u.webformGet(r)
	if err != nil {
		return err
	}
	return u.webformView(w, r, http.StatusOK, webformValues(web), &web)
}

// webformUpdate saves a form's settings (update-form, If-Match from the
// form's version). Every member is sent: lists replace the stored ones.
func (u *UI) webformUpdate(w http.ResponseWriter, r *http.Request) error {
	web, err := u.webformGet(r)
	if err != nil {
		return err
	}
	f := NewForm(r)
	p, err := webformParse(f)
	if err == nil {
		in := p.in
		patch := forms.FormPatch{Name: &in.Name, Fields: &in.Fields, Tags: &in.Tags, SequenceID: in.SequenceID,
			DoubleOptin: &in.DoubleOptin, ConsentText: &in.ConsentText, RedirectURL: &in.RedirectURL,
			AllowedOrigins: &in.AllowedOrigins}
		var saved forms.Form
		saved, err = u.d.Forms.Update(r.Context(), web.ID, patch, autoVersion(f), false)
		if err == nil {
			u.Flash(r, FlashSuccess, fmt.Sprintf("Form %q saved. Embedded copies keep working: the address doesn't change.", saved.Name))
			u.Redirect(w, r, webformURL(saved.ID, ""))
			return nil
		}
		err = autoRetarget(err, webformFields(p))
	}
	if code := f.Fail(err); code != 0 {
		return u.webformView(w, r, code, f, &web)
	}
	return err
}

// webformDelete deletes a form after a confirmation; the first step is
// the service's dry run (delete-form).
func (u *UI) webformDelete(w http.ResponseWriter, r *http.Request) error {
	web, err := u.webformGet(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	if !Confirmed(r) {
		if err := u.d.Forms.Delete(ctx, web.ID, autoVersion(f), true); err != nil {
			return err
		}
		fields := f.Values
		fields.Set("version", strconv.FormatInt(web.Version, 10))
		return u.RenderConfirm(w, r, Confirm{
			Title:   fmt.Sprintf("Delete %q?", web.Name),
			Message: "The form and its submission log are deleted. This can't be undone.",
			Details: []string{
				"Its hosted page and endpoint stop working at once: " + web.HostedURL,
				"Copies embedded on a website stop working too; remove them there.",
				"Contacts who signed up, and their consent records, are kept.",
				"Sign-ups still waiting for confirmation can confirm and get the tags and sequence the form had.",
			},
			Action: webformURL(web.ID, "/delete"), Fields: fields, ConfirmLabel: "Delete form",
			CancelURL: webformURL(web.ID, ""), Danger: true, Nav: "forms", Crumbs: webformCrumbs(web),
		})
	}
	if err := u.d.Forms.Delete(ctx, web.ID, autoVersion(f), false); err != nil {
		return err
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Form %q deleted.", web.Name))
	u.Redirect(w, r, webformPath)
	return nil
}

// ---------------------------------------------------------------- snippet and submissions

type webformSnippetData struct {
	Tabs    template.HTML
	Web     forms.Form
	Snippet forms.Snippet
}

// webformSnippet shows the embed HTML and the hosted page's address
// (get-form-snippet).
func (u *UI) webformSnippet(w http.ResponseWriter, r *http.Request) error {
	web, err := u.webformGet(r)
	if err != nil {
		return err
	}
	sn, err := u.d.Forms.Snippet(r.Context(), web.ID)
	if err != nil {
		return err
	}
	tabs, err := webformTabs(web, "snippet")
	if err != nil {
		return err
	}
	return u.Page(w, r, "automation/form_snippet", View{Title: web.Name, Nav: "forms", Crumbs: []Crumb{{"Forms", webformPath}},
		Data: webformSnippetData{Tabs: tabs, Web: web, Snippet: sn}})
}

type webformSubmissionsData struct {
	Tabs  template.HTML
	Web   forms.Form
	Items []webformSubmission
	Pager Pager
}

type webformSubmission struct {
	forms.Submission
	Badge template.HTML
	Note  string
}

// webformOutcomes are the tone and explanation of each outcome.
var webformOutcomes = map[string][2]string{
	forms.OutcomeCreated:            {"ok", "new contact"},
	forms.OutcomeUpdated:            {"ok", "existing contact updated"},
	forms.OutcomeOptinPending:       {"info", "confirmation email sent (double opt-in)"},
	forms.OutcomeResubscribePending: {"info", "was unsubscribed: confirmation email sent"},
	forms.OutcomeRejected:           {"warn", "suppressed, bounced or complained address; the visitor saw the normal answer"},
	forms.OutcomeSpam:               {"muted", "the hidden honeypot field was filled in"},
	forms.OutcomeRateLimited:        {"muted", "too many submissions from one address"},
}

// webformOutcomeBadge is the badge of a submission outcome.
func webformOutcomeBadge(outcome string) (template.HTML, string) {
	o, ok := webformOutcomes[outcome]
	if !ok {
		o = [2]string{"muted", ""}
	}
	return template.HTML(`<span class="badge badge-` + o[0] + `">` + template.HTMLEscapeString(strings.ReplaceAll(outcome, "_", " ")) + `</span>`), o[1]
}

// webformSubmissions lists a form's recent submissions, newest first
// (list-form-submissions).
func (u *UI) webformSubmissions(w http.ResponseWriter, r *http.Request) error {
	web, err := u.webformGet(r)
	if err != nil {
		return err
	}
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	list, err := u.d.Forms.Submissions(r.Context(), web.ID, p.After, p.Fetch())
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, list, func(s forms.Submission) int64 { return s.ID })
	d := webformSubmissionsData{Web: web, Pager: pager}
	if d.Tabs, err = webformTabs(web, "submissions"); err != nil {
		return err
	}
	for _, s := range items {
		b, note := webformOutcomeBadge(s.Outcome)
		d.Items = append(d.Items, webformSubmission{Submission: s, Badge: b, Note: note})
	}
	return u.Page(w, r, "automation/form_submissions", View{Title: web.Name, Nav: "forms", Crumbs: []Crumb{{"Forms", webformPath}}, Data: d})
}
