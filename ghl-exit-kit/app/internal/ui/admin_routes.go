package ui

import (
	"html/template"
	"net/http"
	"strings"
)

// The "admin" page group: Settings, Users, Health (doctor, SES, sending
// state, backups, jobs, audit log) and API tokens. Every action calls the
// service method behind the API operation it names.
func init() {
	registerGroup("admin", func(u *UI) {
		// Settings (settings.Service: get-settings, update-settings).
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/settings", OperationID: "get-settings", Handler: u.admSettingsPage})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/settings", OperationID: "update-settings",
			MaxBodyBytes: 64 << 10, Handler: u.admSettingsSave})

		// Users (auth.Service).
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/users", OperationID: "list-users", Handler: u.admUsersList})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/users", OperationID: "create-user",
			MaxBodyBytes: 16 << 10, Handler: u.admUserCreate})
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/users/{id}", OperationID: "get-user", Handler: u.admUserPage})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/users/{id}", OperationID: "update-user",
			MaxBodyBytes: 16 << 10, Handler: u.admUserUpdate})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/users/{id}/delete", OperationID: "delete-user",
			Uses: []string{"get-user", "list-tokens"}, MaxBodyBytes: 16 << 10, Handler: u.admUserDelete})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/users/{id}/totp", OperationID: "begin-user-totp",
			MaxBodyBytes: 16 << 10, Handler: u.admUserTOTPBegin})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/users/{id}/totp/confirm", OperationID: "confirm-user-totp",
			MaxBodyBytes: 16 << 10, Handler: u.admUserTOTPConfirm})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/users/{id}/totp/reset", OperationID: "reset-user-totp",
			Uses: []string{"get-user"}, MaxBodyBytes: 16 << 10, Handler: u.admUserTOTPReset})

		// Health (system, sending, jobs, audit).
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/health", OperationID: "get-system-health",
			Uses: []string{"run-doctor", "get-ses-status", "get-sending-state", "get-system-version"}, Handler: u.admHealthPage})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/health/doctor", OperationID: "run-doctor",
			MaxBodyBytes: 16 << 10, Handler: u.admHealthRunDoctor})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/health/sending/pause", OperationID: "pause-sending",
			MaxBodyBytes: 16 << 10, Handler: u.admSendingPause})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/health/sending/resume", OperationID: "resume-sending",
			Uses: []string{"get-sending-state", "get-system-health"}, MaxBodyBytes: 16 << 10, Handler: u.admSendingResume})
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/health/backups", OperationID: "list-backups", Handler: u.admBackupsPage})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/health/backups", OperationID: "create-backup",
			MaxBodyBytes: 16 << 10, Handler: u.admBackupCreate})
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/health/backups/{name}", OperationID: "download-backup",
			Handler: u.admBackupDownload})
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/health/jobs", OperationID: "list-jobs", Handler: u.admJobsPage})
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/health/jobs/{id}", OperationID: "get-job", Handler: u.admJobPage})
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/health/audit", OperationID: "list-audit-log", Handler: u.admAuditPage})

		// API tokens (auth.Service).
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/tokens", OperationID: "list-tokens", Handler: u.admTokensList})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/tokens", OperationID: "create-token",
			MaxBodyBytes: 16 << 10, Handler: u.admTokenCreate})
		u.handle(Action{Method: http.MethodPost, Path: Prefix + "/tokens/{id}/revoke", OperationID: "revoke-token",
			Uses: []string{"get-token"}, MaxBodyBytes: 16 << 10, Handler: u.admTokenRevoke})
	})
}

// adminTab is one link of a page group's section tabs.
type adminTab struct {
	Label   string
	URL     string
	Current bool
}

// Section tabs: Settings and Users share the Settings nav item; Health
// has its overview, backups, jobs and the audit log.
var (
	admSettingsSection = []adminTab{{Label: "Settings", URL: Prefix + "/settings"}, {Label: "Users", URL: Prefix + "/users"}}
	admHealthSection   = []adminTab{
		{Label: "Overview", URL: Prefix + "/health"},
		{Label: "Backups", URL: Prefix + "/health/backups"},
		{Label: "Jobs", URL: Prefix + "/health/jobs"},
		{Label: "Audit log", URL: Prefix + "/health/audit"},
	}
)

// admTabs returns section with the tab at url marked current.
func admTabs(section []adminTab, url string) admTabList {
	out := make(admTabList, len(section))
	for i, t := range section {
		t.Current = t.URL == url
		out[i] = t
	}
	return out
}

// admTabList is a section's tabs; HTML renders them (a Go component, as
// the group has no shared template partial for them).
type admTabList []adminTab

var admComponents = template.Must(template.New("admin").Parse(
	`{{define "tabs"}}<nav class="subnav" aria-label="Section">{{range .}}<a class="btn btn-sm{{if .Current}} btn-primary{{end}}" href="{{.URL}}"{{if .Current}} aria-current="page"{{end}}>{{.Label}}</a>{{end}}</nav>{{end}}` +
		`{{define "secret"}}<div class="field">
  <label for="{{.ID}}">{{.Label}}</label>
  <div class="row">
    <input id="{{.ID}}" class="mono" type="text" readonly value="{{.Value}}" autocomplete="off" spellcheck="false" aria-describedby="{{.ID}}-hint">
    <span data-copy-wrap hidden><button class="btn" type="button" data-copy="{{.ID}}">Copy</button></span>
  </div>
  <p class="hint" id="{{.ID}}-hint">Copy it now. It is shown only on this page and can't be shown again.</p>
</div>{{end}}`))

func admRender(name string, data any) template.HTML {
	var b strings.Builder
	if err := admComponents.ExecuteTemplate(&b, name, data); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(b.String()) // produced by html/template, so escaped
}

// HTML renders the tabs.
func (l admTabList) HTML() template.HTML { return admRender("tabs", l) }

// admSecret is a one-time secret (token, generated password, TOTP key) in
// a read-only field with a copy button. The button sits in a hidden
// [data-copy-wrap] span that app.js reveals (its generic [data-copy]
// handler); without the script the field is selected by hand.
type admSecret struct {
	ID, Label, Value string
}

// HTML renders the field.
func (s admSecret) HTML() template.HTML { return admRender("secret", s) }
