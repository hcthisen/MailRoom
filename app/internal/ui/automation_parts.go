package ui

import (
	"bytes"
	"html/template"

	"mailroom/internal/forms"
	"mailroom/internal/sequences"
)

// Shared pieces of the automation pages (the sub navigation of a
// sequence or form, and a sequence's configuration summary), rendered in
// Go and handed to the page templates as escaped HTML.
//
// They are kept in Go rather than as group partials
// (templates/automation/_*.html, which work since render.go embeds
// all:templates). html/template escapes everything here exactly as in a
// page template.

const autoPartsSrc = `
{{define "seq-tabs"}}<nav class="subnav" aria-label="Sequence pages">
  {{$c := .Current}}{{$id := .Seq.ID}}
  {{range .Tabs}}<a class="btn btn-sm {{if eq $c .Key}}btn-primary{{else}}btn-ghost{{end}}" href="/admin/sequences/{{$id}}{{.Suffix}}"{{if eq $c .Key}} aria-current="page"{{end}}>{{.Label}}</a>
  {{end}}
</nav>
<hr>{{end}}

{{define "webform-tabs"}}<nav class="subnav" aria-label="Form pages">
  {{$c := .Current}}{{$id := .Web.ID}}
  {{range .Tabs}}<a class="btn btn-sm {{if eq $c .Key}}btn-primary{{else}}btn-ghost{{end}}" href="/admin/forms/{{$id}}{{.Suffix}}"{{if eq $c .Key}} aria-current="page"{{end}}>{{.Label}}</a>
  {{end}}
  <span class="spacer"></span>
  <a class="btn btn-sm" href="{{.Web.HostedURL}}" target="_blank" rel="noopener">Open hosted page ↗</a>
</nav>
<hr>{{end}}

{{define "seq-summary"}}<dl class="props">
  <dt>Status</dt><dd>{{if eq .Seq.Status "paused"}}<span class="onoff is-off">Off</span> <span class="muted small">nothing is sent until it is switched on</span>{{else}}<span class="onoff is-on">On</span>{{end}}</dd>
  <dt>Trigger</dt><dd>{{with .Seq.TriggerTag}}Adding the tag <span class="tag">{{.}}</span> to an active contact{{else}}<span class="muted">No tag trigger: forms, imports and manual enrolment only</span>{{end}}</dd>
  <dt>Exit tags</dt><dd>{{range .Seq.ExitTags}}<span class="tag">{{.}}</span>{{else}}<span class="muted">None</span>{{end}}</dd>
  <dt>Tag on exit</dt><dd>{{with .Seq.ExitAddTag}}<span class="tag">{{.}}</span>{{else}}<span class="muted">None</span>{{end}}</dd>
  <dt>Re-entry</dt><dd>{{if eq .Seq.Reentry "never"}}Never: once per contact{{else if eq .Seq.Reentry "after_completion"}}Again after completing it{{else}}Again whenever not enrolled{{end}}</dd>
  <dt>Send window</dt><dd>{{with .Window}}{{.}} <span class="muted small">({{$.Zone}})</span>{{else}}{{with $.Default}}Default from the settings: {{.}} <span class="muted small">({{$.Zone}})</span>{{else}}Any time{{end}}{{end}}</dd>
</dl>{{end}}
`

var autoParts = template.Must(template.New("automation-parts").Funcs(template.FuncMap{"badge": Badge}).Parse(autoPartsSrc))

// autoTab is one link of a sub navigation.
type autoTab struct{ Key, Suffix, Label string }

var (
	seqTabList = []autoTab{
		{"overview", "", "Steps and stats"}, {"enrollments", "/enrollments", "Enrollments"}, {"enroll", "/enroll", "Enrol contacts"},
		{"settings", "/edit", "Settings"}, {"export", "/export", "Export"},
	}
	webformTabList = []autoTab{
		{"settings", "", "Settings"}, {"snippet", "/snippet", "Embed and link"}, {"submissions", "/submissions", "Submissions"},
	}
)

// autoPart renders one of the shared pieces.
func autoPart(name string, data any) (template.HTML, error) {
	var b bytes.Buffer
	if err := autoParts.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return template.HTML(b.String()), nil // escaped by html/template
}

// seqTabs is the sub navigation of a sequence; current is the key of the
// page shown.
func seqTabs(seq sequences.Sequence, current string) (template.HTML, error) {
	return autoPart("seq-tabs", map[string]any{"Seq": seq, "Current": current, "Tabs": seqTabList})
}

// webformTabs is the sub navigation of a form.
func webformTabs(web forms.Form, current string) (template.HTML, error) {
	return autoPart("webform-tabs", map[string]any{"Web": web, "Current": current, "Tabs": webformTabList})
}

// seqSummary describes a sequence's configuration: window is its own send
// window described, def the settings' default (shown when it has none).
func (u *UI) seqSummary(seq sequences.Sequence, window, def string) (template.HTML, error) {
	return autoPart("seq-summary", map[string]any{"Seq": seq, "Window": window, "Default": def, "Zone": u.location().String()})
}
