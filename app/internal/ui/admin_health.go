package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/jobs"
	"mailroom/internal/sending"
	"mailroom/internal/system"
)

// ---------------------------------------------------------------- overview

// admHealthData is the Health overview's data.
type admHealthData struct {
	Tabs    admTabList
	Health  system.Health
	Version system.VersionInfo

	Doctor *system.Report // last doctor run, nil when none yet

	SES     *system.SESStatus
	SESErr  string
	SESLive bool // the page was asked to read SES live

	Sending    sending.State
	SendingErr string
	PauseForm  *Form
	Uptime     string
}

// admMaxPauseReason is the API's limit on the pause reason (pause-sending).
const admMaxPauseReason = 500

func (u *UI) admLoadHealth(ctx context.Context, sesLive bool) admHealthData {
	d := admHealthData{Tabs: admTabs(admHealthSection, Prefix+"/health"), Health: u.d.System.Health(ctx),
		Version: u.d.System.Version(), SESLive: sesLive, PauseForm: EmptyForm()}
	d.Uptime = admFormatUptime(d.Version.UptimeSeconds)
	if rep, ok := u.LastDoctor(); ok {
		d.Doctor = &rep
	}
	timeout := sesTimeout
	if sesLive {
		timeout = system.DoctorTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	ses, err := u.d.System.SESStatus(sctx, sesLive)
	cancel()
	switch {
	case errors.Is(err, core.ErrConflict):
		d.SESErr = "SES is not configured yet: the sending user's AWS keys are missing. Run `mailroom provision ses` (see the deploy guide)."
	case err != nil:
		u.log.WarnContext(ctx, "ui: SES status", "err", err)
		d.SESErr = "The SES account status could not be read right now. Try again in a minute."
	default:
		d.SES = &ses
	}
	if st, err := u.d.Sending.GetState(ctx, nil); err != nil {
		u.log.ErrorContext(ctx, "ui: sending state", "err", err)
		d.SendingErr = "The sending state could not be read."
	} else {
		d.Sending = st
	}
	return d
}

// admFormatUptime is "3 d 4 h", "2 h 5 min" or "40 s".
func admFormatUptime(sec int64) string {
	switch {
	case sec >= 86400:
		return fmt.Sprintf("%d d %d h", sec/86400, sec%86400/3600)
	case sec >= 3600:
		return fmt.Sprintf("%d h %d min", sec/3600, sec%3600/60)
	case sec >= 60:
		return fmt.Sprintf("%d min", sec/60)
	}
	return fmt.Sprintf("%d s", sec)
}

func admHealthView(d admHealthData) View {
	return View{Title: "Health", Subtitle: "Install checks, Amazon SES, sending, backups, jobs and the audit log.", Nav: "health", Data: d}
}

// admHealthPage is the Health overview (get-system-health, plus the cached
// doctor report, SES status and sending state). ?ses=live reads SES now.
func (u *UI) admHealthPage(w http.ResponseWriter, r *http.Request) error {
	d := u.admLoadHealth(r.Context(), r.URL.Query().Get("ses") == "live")
	return u.Page(w, r, "admin/health", admHealthView(d))
}

// admHealthRunDoctor runs the doctor checks now (run-doctor). htmx swaps the
// checks card; a plain post redirects back to the overview.
func (u *UI) admHealthRunDoctor(w http.ResponseWriter, r *http.Request) error {
	rep := u.RefreshDoctor(r.Context())
	msg, kind := "All checks passed.", FlashSuccess
	switch rep.Status {
	case system.StatusFail:
		msg, kind = "Checks finished: some failed. See the fix hints below.", FlashError
	case system.StatusWarn:
		msg, kind = "Checks finished with warnings. See the fix hints below.", FlashWarning
	}
	u.Flash(r, kind, msg)
	if isHTMX(r) {
		return u.Partial(w, r, "admin/health", "doctor-card", View{Data: admHealthData{Doctor: &rep}})
	}
	u.Redirect(w, r, Prefix+"/health")
	return nil
}

// admSendingPause pauses all sending (pause-sending) with an optional reason.
func (u *UI) admSendingPause(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	reason := f.Get("reason")
	if utf8.RuneCountInString(reason) > admMaxPauseReason {
		f.AddError("reason", fmt.Sprintf("must be at most %d characters", admMaxPauseReason))
		d := u.admLoadHealth(r.Context(), false)
		d.PauseForm = f
		return u.PageStatus(w, r, http.StatusUnprocessableEntity, "admin/health", admHealthView(d))
	}
	st, err := u.d.Sending.Pause(r.Context(), reason, false)
	if err != nil {
		return err
	}
	u.Flash(r, FlashWarning, "All sending is paused: "+st.Reason+". Queued emails wait until you resume.")
	u.Redirect(w, r, Prefix+"/health")
	return nil
}

// admSendingResume lifts the pause (resume-sending) after a confirmation that
// says how many queued emails will then go out.
func (u *UI) admSendingResume(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	cur, err := u.d.Sending.GetState(ctx, nil)
	if err != nil {
		return err
	}
	if !cur.Paused {
		u.Flash(r, FlashInfo, "Sending is not paused.")
		u.Redirect(w, r, Prefix+"/health")
		return nil
	}
	if !Confirmed(r) {
		if _, err := u.d.Sending.Resume(ctx, true); err != nil {
			return err
		}
		h := u.d.System.Health(ctx)
		details := []string{
			fmt.Sprintf("%s queued, %s of them due now: they start sending right away.",
				plural(int(h.QueueDepth), "email is", "emails are"), formatNumber(h.QueueDue)),
		}
		if cur.Reason != "" {
			details = append(details, "Paused because: "+cur.Reason)
		}
		if cur.Since != nil {
			details = append(details, "Paused since "+u.fmtTime(cur.Since)+".")
		}
		if cur.Source == sending.PauseSES {
			details = append(details, "Amazon SES paused sending for the account. If SES still reports sending disabled, "+
				"the next account check (every 5 minutes) pauses again.")
		}
		return u.RenderConfirm(w, r, Confirm{
			Title: "Resume sending?", Message: "Every kind of email (broadcasts, sequence steps, confirmations, tests, alerts) is sent again at the normal rate.",
			Details: details, Action: Prefix + "/health/sending/resume", ConfirmLabel: "Resume sending",
			CancelURL: Prefix + "/health", Nav: "health", Crumbs: []Crumb{{Label: "Health", URL: Prefix + "/health"}},
		})
	}
	if _, err := u.d.Sending.Resume(ctx, false); err != nil {
		return err
	}
	u.Flash(r, FlashSuccess, "Sending resumed.")
	u.Redirect(w, r, Prefix+"/health")
	return nil
}

// ---------------------------------------------------------------- backups

type admBackupsData struct {
	Tabs     admTabList
	Items    []system.BackupInfo
	Schedule system.BackupSchedule
}

// admBackupsPage lists the local backups and the schedule (list-backups).
func (u *UI) admBackupsPage(w http.ResponseWriter, r *http.Request) error {
	list, err := u.d.System.ListBackups()
	if err != nil {
		return err
	}
	return u.Page(w, r, "admin/backups", View{Title: "Backups", Nav: "health",
		Subtitle: "Consistent copies of the database. Download one to keep it somewhere safe.",
		Crumbs:   []Crumb{{Label: "Health", URL: Prefix + "/health"}},
		Data:     admBackupsData{Tabs: admTabs(admHealthSection, Prefix+"/health/backups"), Items: list, Schedule: u.d.System.Schedule(r.Context())}})
}

// admBackupCreate starts a backup job (create-backup) and shows the job.
func (u *UI) admBackupCreate(w http.ResponseWriter, r *http.Request) error {
	job, err := u.d.System.RequestBackup(r.Context(), false)
	if err != nil {
		return err
	}
	u.Flash(r, FlashSuccess, "Backup started. This page updates when it has finished.")
	u.Redirect(w, r, fmt.Sprintf("%s/health/jobs/%d", Prefix, job.ID))
	return nil
}

// admBackupContentType is the media type of a backup file (SQLite).
const admBackupContentType = "application/vnd.sqlite3"

// admBackupDownload streams a backup file (download-backup). The service
// refuses names that aren't backup names.
func (u *UI) admBackupDownload(w http.ResponseWriter, r *http.Request) error {
	f, info, err := u.d.System.OpenBackup(r.PathValue("name"))
	if err != nil {
		return err
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", admBackupContentType)
	h.Set("Content-Length", strconv.FormatInt(info.SizeBytes, 10))
	h.Set("Content-Disposition", `attachment; filename="`+info.Name+`"`)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		u.log.WarnContext(r.Context(), "ui: backup download interrupted", "name", info.Name, "err", err)
	}
	return nil
}

// ---------------------------------------------------------------- jobs

var admJobStatuses = []Option{
	{"", "Any status"}, {jobs.StatusQueued, "Queued"}, {jobs.StatusRunning, "Running"},
	{jobs.StatusSucceeded, "Succeeded"}, {jobs.StatusFailed, "Failed"},
}

type admJobsData struct {
	Tabs     admTabList
	Filter   *Form
	Statuses []Option
	Items    []jobs.Job
	Pager    Pager
}

// admQueryForm is a filter form read from the query string.
func admQueryForm(r *http.Request, names ...string) *Form {
	f := EmptyForm()
	q := r.URL.Query()
	for _, n := range names {
		if v := q.Get(n); v != "" {
			f.Set(n, v)
		}
	}
	return f
}

// admJobsPage lists background jobs, newest first (list-jobs).
func (u *UI) admJobsPage(w http.ResponseWriter, r *http.Request) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	f := admQueryForm(r, "type", "status")
	valid := false
	for _, o := range admJobStatuses {
		valid = valid || o.Value == f.Get("status")
	}
	if !valid {
		return core.Invalid("status", "must be queued, running, succeeded or failed")
	}
	list, err := u.d.Jobs.List(r.Context(), jobs.ListFilter{Type: f.Get("type"), Status: f.Get("status"), BeforeID: p.After, Limit: p.Fetch()})
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, list, func(j jobs.Job) int64 { return j.ID })
	return u.Page(w, r, "admin/jobs", View{Title: "Jobs", Nav: "health",
		Subtitle: "Background work: imports, broadcast snapshots, backups and bulk changes.",
		Crumbs:   []Crumb{{Label: "Health", URL: Prefix + "/health"}},
		Data:     admJobsData{Tabs: admTabs(admHealthSection, Prefix+"/health/jobs"), Filter: f, Statuses: admJobStatuses, Items: items, Pager: pager}})
}

type admJobData struct {
	Job    jobs.Job
	Params string // indented JSON
	Result string
}

// admPrettyJSON indents raw JSON for display ("" for null or empty).
func admPrettyJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return ""
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return string(raw)
	}
	return b.String()
}

// admJobPage shows one job (get-job). While it runs, htmx reloads the job
// card every two seconds.
func (u *UI) admJobPage(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	j, err := u.d.Jobs.Get(r.Context(), id)
	if err != nil {
		return err
	}
	v := View{Title: fmt.Sprintf("Job %d", j.ID), Subtitle: j.Type, Nav: "health",
		Crumbs: []Crumb{{Label: "Health", URL: Prefix + "/health"}, {Label: "Jobs", URL: Prefix + "/health/jobs"}},
		Data:   admJobData{Job: j, Params: admPrettyJSON(j.Params), Result: admPrettyJSON(j.Result)}}
	if isHTMX(r) {
		return u.Partial(w, r, "admin/job", "job-card", v)
	}
	return u.Page(w, r, "admin/job", v)
}

// ---------------------------------------------------------------- audit

type admAuditRow struct {
	audit.Record
	Changes string // compact JSON
}

type admAuditData struct {
	Tabs   admTabList
	Filter *Form
	Items  []admAuditRow
	Pager  Pager
}

// admAuditFilters are the audit page's query parameters (list-audit-log).
var admAuditFilters = []string{"target_type", "target_id", "actor", "operation", "since", "until"}

// admAuditPage lists audit log entries, newest first, with filters
// (list-audit-log). Times are entered in the settings time zone.
func (u *UI) admAuditPage(w http.ResponseWriter, r *http.Request) error {
	f := admQueryForm(r, admAuditFilters...)
	v := View{Title: "Audit log", Nav: "health",
		Subtitle: "Every change made through the UI or the API: who, when, what and which fields.",
		Crumbs:   []Crumb{{Label: "Health", URL: Prefix + "/health"}}}
	d := admAuditData{Tabs: admTabs(admHealthSection, Prefix+"/health/audit"), Filter: f}
	fail := func(err error) error {
		if st := f.Fail(err); st != 0 {
			v.Data = d
			return u.PageStatus(w, r, st, "admin/audit", v)
		}
		return err
	}
	p, err := ParseList(r)
	if err != nil {
		return fail(err)
	}
	since, err := u.ParseInputTime("since", f.Get("since"))
	if err != nil {
		return fail(err)
	}
	until, err := u.ParseInputTime("until", f.Get("until"))
	if err != nil {
		return fail(err)
	}
	for _, n := range []string{"target_type", "target_id", "operation"} {
		if len(f.Get(n)) > 64 {
			return fail(core.Invalid(n, "must be at most 64 characters"))
		}
	}
	if len(f.Get("actor")) > 254 {
		return fail(core.Invalid("actor", "must be at most 254 characters"))
	}
	recs, err := u.d.Audit.List(r.Context(), audit.Filter{
		TargetType: f.Get("target_type"), TargetID: f.Get("target_id"), Actor: f.Get("actor"), Operation: f.Get("operation"),
		Since: since, Until: until, BeforeID: p.After, Limit: p.Fetch(),
	})
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, recs, func(x audit.Record) int64 { return x.ID })
	for _, rec := range items {
		d.Items = append(d.Items, admAuditRow{Record: rec, Changes: admCompactJSON(rec.Changes)})
	}
	d.Pager = pager
	v.Data = d
	return u.Page(w, r, "admin/audit", v)
}

// admCompactJSON is raw JSON on one line ("" for null or empty).
func admCompactJSON(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" || string(t) == "{}" {
		return ""
	}
	var b bytes.Buffer
	if err := json.Compact(&b, t); err != nil {
		return string(t)
	}
	return b.String()
}
