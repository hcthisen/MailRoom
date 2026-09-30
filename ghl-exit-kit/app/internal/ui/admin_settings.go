package ui

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"mailroom/internal/settings"
)

// admSettingsData is the settings page's data.
type admSettingsData struct {
	Tabs      admTabList
	Form      *Form
	Domain    string
	Weekdays  []Option // ISO weekday number -> name
	Timezones []string // suggestions for the time zone field
}

// admWeekdays are the ISO admWeekdays of the send window.
var admWeekdays = []Option{
	{"1", "Mon"}, {"2", "Tue"}, {"3", "Wed"}, {"4", "Thu"}, {"5", "Fri"}, {"6", "Sat"}, {"7", "Sun"},
}

// admCommonTimezones are suggestions; any IANA zone is accepted.
var admCommonTimezones = []string{
	"UTC", "Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Copenhagen", "Europe/Stockholm",
	"Europe/Oslo", "Europe/Helsinki", "Europe/Berlin", "Europe/Amsterdam", "Europe/Brussels", "Europe/Paris",
	"Europe/Zurich", "Europe/Vienna", "Europe/Madrid", "Europe/Rome", "Europe/Warsaw", "Europe/Prague",
	"Europe/Athens", "America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
	"America/Toronto", "America/Sao_Paulo", "Asia/Dubai", "Asia/Kolkata", "Asia/Singapore", "Asia/Tokyo",
	"Australia/Sydney", "Pacific/Auckland",
}

// Settings form field names: the service's field names, so validation
// errors land next to the right input.
const (
	admFFromName        = "sender.from_name"
	admFFromEmail       = "sender.from_email"
	admFReplyTo         = "sender.reply_to"
	admFFooter          = "company_footer"
	admFOwnerEmail      = "owner_email"
	admFAlertEmail      = "alert_email"
	admFTimezone        = "timezone"
	admFClicks          = "tracking.clicks"
	admFOpens           = "tracking.opens"
	admFRateOverride    = "sending.rate_override_per_sec"
	admFRateFraction    = "sending.rate_fraction"
	admFMinMessages     = "circuit_breaker.min_messages"
	admFBounceRate      = "circuit_breaker.hard_bounce_rate"
	admFComplaintRate   = "circuit_breaker.complaint_rate"
	admFWindowOn        = "default_send_window.enabled"
	admFWindowDays      = "default_send_window.days"
	admFWindowStart     = "default_send_window.start"
	admFWindowEnd       = "default_send_window.end"
	admFSettingsVersion = "version"
)

// admSettingsForm fills the form from the stored settings.
func admSettingsForm(st settings.Settings) *Form {
	f := EmptyForm()
	f.Set(admFFromName, st.Sender.FromName).Set(admFFromEmail, st.Sender.FromEmail).Set(admFReplyTo, st.Sender.ReplyTo)
	f.Set(admFFooter, st.CompanyFooter).Set(admFOwnerEmail, st.OwnerEmail).Set(admFAlertEmail, st.AlertEmail)
	f.Set(admFTimezone, st.Timezone)
	f.Set(admFClicks, admBoolValue(st.Tracking.Clicks)).Set(admFOpens, admBoolValue(st.Tracking.Opens))
	f.Set(admFRateOverride, admFormatFloat(st.Sending.RateOverridePerSec)).Set(admFRateFraction, admFormatFloat(st.Sending.RateFraction))
	f.Set(admFMinMessages, strconv.Itoa(st.CircuitBreaker.MinMessages))
	f.Set(admFBounceRate, admFormatFloat(st.CircuitBreaker.HardBounceRate)).Set(admFComplaintRate, admFormatFloat(st.CircuitBreaker.ComplaintRate))
	if w := st.DefaultSendWindow; w != nil {
		f.Set(admFWindowOn, "1").Set(admFWindowStart, w.Start).Set(admFWindowEnd, w.End)
		for _, d := range w.Days {
			f.Values.Add(admFWindowDays, strconv.Itoa(d))
		}
	} else {
		// Suggested window when it is switched on.
		f.Set(admFWindowStart, "08:00").Set(admFWindowEnd, "20:00")
		for _, d := range []int{1, 2, 3, 4, 5} {
			f.Values.Add(admFWindowDays, strconv.Itoa(d))
		}
	}
	f.Set(admFSettingsVersion, strconv.FormatInt(st.Version, 10))
	return f
}

func admBoolValue(b bool) string {
	if b {
		return "1"
	}
	return ""
}

func admFormatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// admParseNumber reads a decimal form field ("0,8" is read as 0.8).
func admParseNumber(f *Form, field string) (float64, error) {
	s := strings.ReplaceAll(f.Get(field), ",", ".")
	if s == "" {
		return 0, fieldError(field, "is required")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fieldError(field, "must be a number")
	}
	return v, nil
}

// admSettingsPatch turns the submitted form into a full settings patch and
// the If-Match version the form was loaded with. The settings service
// validates every value.
func admSettingsPatch(f *Form) (settings.Patch, int64, error) {
	str := func(name string) *string { v := f.Get(name); return &v }
	footer := strings.TrimSpace(strings.ReplaceAll(f.Raw(admFFooter), "\r\n", "\n"))
	clicks, opens := f.Checked(admFClicks), f.Checked(admFOpens)
	p := settings.Patch{
		Sender:        &settings.SenderPatch{FromName: str(admFFromName), FromEmail: str(admFFromEmail), ReplyTo: str(admFReplyTo)},
		CompanyFooter: &footer,
		OwnerEmail:    str(admFOwnerEmail),
		AlertEmail:    str(admFAlertEmail),
		Timezone:      str(admFTimezone),
		Tracking:      &settings.TrackingPatch{Clicks: &clicks, Opens: &opens},
	}
	override, err := admParseNumber(f, admFRateOverride)
	if err != nil {
		return p, 0, err
	}
	fraction, err := admParseNumber(f, admFRateFraction)
	if err != nil {
		return p, 0, err
	}
	p.Sending = &settings.SendingPatch{RateOverridePerSec: &override, RateFraction: &fraction}
	minMsgs, err := strconv.Atoi(f.Get(admFMinMessages))
	if err != nil {
		return p, 0, fieldError(admFMinMessages, "must be a whole number")
	}
	bounce, err := admParseNumber(f, admFBounceRate)
	if err != nil {
		return p, 0, err
	}
	complaint, err := admParseNumber(f, admFComplaintRate)
	if err != nil {
		return p, 0, err
	}
	p.CircuitBreaker = &settings.CircuitBreakerPatch{MinMessages: &minMsgs, HardBounceRate: &bounce, ComplaintRate: &complaint}
	p.DefaultSendWindow.Set = true
	if f.Checked(admFWindowOn) {
		w := &settings.SendWindow{Start: f.Get(admFWindowStart), End: f.Get(admFWindowEnd), Days: []int{}}
		for _, s := range f.All(admFWindowDays) {
			d, err := strconv.Atoi(s)
			if err != nil {
				return p, 0, fieldError(admFWindowDays, "must be admWeekdays")
			}
			w.Days = append(w.Days, d)
		}
		slices.Sort(w.Days)
		p.DefaultSendWindow.Value = w
	}
	version, err := strconv.ParseInt(f.Get(admFSettingsVersion), 10, 64)
	if err != nil || version < 0 {
		return p, 0, fieldError("", "the form is incomplete: reload the page and try again")
	}
	return p, version, nil
}

func (u *UI) admSettingsView(f *Form, domain string) View {
	return View{Title: "Settings", Subtitle: "Sender identity, footer, tracking, send rate and safety limits.", Nav: "settings",
		Data: admSettingsData{Tabs: admTabs(admSettingsSection, Prefix+"/settings"), Form: f, Domain: domain,
			Weekdays: admWeekdays, Timezones: admCommonTimezones}}
}

// admSettingsPage shows the settings form (get-settings).
func (u *UI) admSettingsPage(w http.ResponseWriter, r *http.Request) error {
	st, err := u.d.Settings.Get(r.Context())
	if err != nil {
		return err
	}
	return u.Page(w, r, "admin/settings", u.admSettingsView(admSettingsForm(st), st.Domain))
}

// admSettingsSave stores the form (update-settings) with the version it was
// loaded with as If-Match, so a concurrent change is never overwritten.
func (u *UI) admSettingsSave(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	domain := u.d.Settings.Domain()
	p, version, err := admSettingsPatch(f)
	if err == nil {
		_, err = u.d.Settings.Update(r.Context(), p, version)
	}
	if err != nil {
		st := f.Fail(err)
		if st == 0 {
			return err
		}
		if st == http.StatusPreconditionFailed {
			f.Message = "Someone else changed the settings while you were editing them. Your values are still in the form " +
				"but were not saved. Reload the page to see the current settings, then make your change again."
		}
		return u.PageStatus(w, r, st, "admin/settings", u.admSettingsView(f, domain))
	}
	u.Flash(r, FlashSuccess, "Settings saved.")
	u.Redirect(w, r, Prefix+"/settings")
	return nil
}
