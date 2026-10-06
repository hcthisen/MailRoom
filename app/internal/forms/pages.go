package forms

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"
)

// pageCSP is the Content-Security-Policy of the public pages: no scripts,
// no external assets, inline styles only. form-action is left open so a
// form's redirect_url can follow the POST.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'"

// fieldView is one input of a rendered form.
type fieldView struct {
	Key, ID, Label, Type, Autocomplete, Value, Error string
	Required                                         bool
}

// formView is a rendered form (hosted page and snippet).
type formView struct {
	Action      string
	PublicID    string
	Fields      []fieldView
	ConsentText string
	Honeypot    string
	Errors      []FieldError
}

// pageData is the data of every public page.
type pageData struct {
	Title   string
	Sender  string
	Heading string
	Message string
	Form    *formView
	// ConfirmAction, when set, shows a button posting to it.
	ConfirmAction string
	ConfirmLabel  string
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
*{box-sizing:border-box}
body{margin:0;padding:24px 16px;background:#f3f4f6;color:#111827;font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
main{max-width:480px;margin:0 auto;background:#fff;border-radius:8px;padding:32px 24px;box-shadow:0 1px 3px rgba(0,0,0,.1)}
h1{margin:0 0 8px;font-size:24px;line-height:1.3}
.sender{margin:0 0 24px;color:#4b5563}
p{margin:0 0 16px}
label{display:block;font-weight:600;margin:0 0 4px}
.req{color:#b91c1c}
input[type=text],input[type=email]{display:block;width:100%;padding:10px 12px;font:inherit;border:1px solid #9ca3af;border-radius:4px;background:#fff;color:inherit}
input:focus,button:focus{outline:3px solid #93c5fd;outline-offset:1px}
input[aria-invalid=true]{border-color:#b91c1c}
.field{margin:0 0 16px}
.err{color:#b91c1c;font-size:14px;margin:4px 0 0}
.summary{border:2px solid #b91c1c;border-radius:4px;padding:12px 16px;margin:0 0 24px}
.summary ul{margin:8px 0 0;padding-left:20px}
.summary a{color:#b91c1c}
.consent{font-size:14px;color:#4b5563;white-space:pre-line}
button{display:inline-block;padding:12px 24px;font:inherit;font-weight:600;color:#fff;background:#1d4ed8;border:0;border-radius:4px;cursor:pointer}
button:hover{background:#1e40af}
.hp{position:absolute;left:-10000px;top:auto;width:1px;height:1px;overflow:hidden}
</style>
</head>
<body>
<main>
<h1>{{.Heading}}</h1>
{{- if .Sender}}
<p class="sender">{{.Sender}}</p>
{{- end}}
{{- if .Message}}
<p role="status">{{.Message}}</p>
{{- end}}
{{- with .Form}}
{{- if .Errors}}
<div class="summary" role="alert" tabindex="-1">
<strong>Please correct the following:</strong>
<ul>{{range .Errors}}<li><a href="#f-{{.Field}}">{{.Message}}</a></li>{{end}}</ul>
</div>
{{- end}}
<form action="{{.Action}}" method="post" accept-charset="utf-8" novalidate>
{{- range .Fields}}
<div class="field">
<label for="{{.ID}}">{{.Label}}{{if .Required}} <span class="req" aria-hidden="true">*</span>{{end}}</label>
<input type="{{.Type}}" id="{{.ID}}" name="{{.Key}}" value="{{.Value}}"{{if .Autocomplete}} autocomplete="{{.Autocomplete}}"{{end}}{{if .Required}} required aria-required="true"{{end}}{{if .Error}} aria-invalid="true" aria-describedby="{{.ID}}-err"{{end}}>
{{- if .Error}}
<p class="err" id="{{.ID}}-err">{{.Error}}</p>
{{- end}}
</div>
{{- end}}
<div class="hp" aria-hidden="true">
<label for="f-{{.Honeypot}}">Leave this field empty</label>
<input type="text" id="f-{{.Honeypot}}" name="{{.Honeypot}}" value="" tabindex="-1" autocomplete="off">
</div>
<p class="consent">{{.ConsentText}}</p>
<button type="submit">Sign up</button>
</form>
{{- end}}
{{- if .ConfirmAction}}
<form action="{{.ConfirmAction}}" method="post">
<button type="submit">{{.ConfirmLabel}}</button>
</form>
{{- end}}
</main>
</body>
</html>
`))

// snippetTemplate is the embeddable form (F-4). It has inline styles only
// on the honeypot, so the client's site styles the rest.
var snippetTemplate = template.Must(template.New("snippet").Parse(`<form action="{{.Action}}" method="post" accept-charset="utf-8" class="mailroom-form">
{{- range .Fields}}
  <p>
    <label for="mr-{{$.PublicID}}-{{.Key}}">{{.Label}}</label><br>
    <input type="{{.Type}}" id="mr-{{$.PublicID}}-{{.Key}}" name="{{.Key}}"{{if .Autocomplete}} autocomplete="{{.Autocomplete}}"{{end}}{{if .Required}} required{{end}}>
  </p>
{{- end}}
  <div style="position:absolute;left:-10000px;top:auto;width:1px;height:1px;overflow:hidden" aria-hidden="true">
    <label for="mr-{{.PublicID}}-{{.Honeypot}}">Leave this field empty</label>
    <input type="text" id="mr-{{.PublicID}}-{{.Honeypot}}" name="{{.Honeypot}}" tabindex="-1" autocomplete="off">
  </div>
  <p style="white-space:pre-line"><small>{{.ConsentText}}</small></p>
  <p><button type="submit">Sign up</button></p>
</form>
`))

// newFormView builds the view of form f with submitted values and errors.
func newFormView(f Form, values map[string]string, errs []FieldError) *formView {
	byField := map[string]string{}
	for _, e := range errs {
		if _, dup := byField[e.Field]; !dup {
			byField[e.Field] = e.Message
		}
	}
	view := &formView{
		Action: f.Endpoint, PublicID: f.PublicID, ConsentText: f.ConsentText, Honeypot: HoneypotField, Errors: errs,
	}
	add := func(key, label, typ, autocomplete string, required bool) {
		view.Fields = append(view.Fields, fieldView{
			Key: key, ID: "f-" + key, Label: label, Type: typ, Autocomplete: autocomplete,
			Value: values[key], Error: byField[key], Required: required,
		})
	}
	add(FieldEmail, "Email", "email", "email", true)
	for _, fd := range f.Fields {
		ac := ""
		switch fd.Key {
		case FieldFirstName:
			ac = "given-name"
		case FieldLastName:
			ac = "family-name"
		}
		add(fd.Key, fd.Label, "text", ac, fd.Required)
	}
	return view
}

// renderSnippet returns the embeddable HTML of f.
func renderSnippet(f Form) (string, error) {
	var b strings.Builder
	if err := snippetTemplate.Execute(&b, newFormView(f, nil, nil)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// writePage renders a public page with status.
func (s *Service) writePage(w http.ResponseWriter, r *http.Request, status int, d pageData) {
	if d.Sender == "" {
		d.Sender = s.senderName(r.Context())
	}
	if d.Title == "" {
		d.Title = d.Heading
	}
	var b bytes.Buffer
	if err := pageTemplate.Execute(&b, d); err != nil {
		s.log.ErrorContext(r.Context(), "forms: render page", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b.Bytes())
	}
}

// writeMessage renders a page with a heading and one message.
func (s *Service) writeMessage(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	s.writePage(w, r, status, pageData{Heading: heading, Message: message})
}
