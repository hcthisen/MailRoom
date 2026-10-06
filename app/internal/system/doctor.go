package system

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"mailroom/internal/settings"
)

// Status is the outcome of one doctor check.
type Status string

// Check statuses, from best to worst. A skipped check didn't apply (e.g.
// https_cert behind a reverse proxy) and doesn't lower the overall status.
const (
	StatusOK      Status = "ok"
	StatusSkipped Status = "skipped"
	StatusWarn    Status = "warn"
	StatusFail    Status = "fail"
)

func (s Status) rank() int {
	switch s {
	case StatusWarn:
		return 1
	case StatusFail:
		return 2
	}
	return 0
}

// worse returns the worse of a and b.
func worse(a, b Status) Status {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Doctor check names, in report order.
const (
	CheckDNSApp          = "dns_app"
	CheckDNSDKIM         = "dns_dkim"
	CheckDNSMailFrom     = "dns_mail_from"
	CheckDNSDMARC        = "dns_dmarc"
	CheckSESIdentity     = "ses_identity"
	CheckSESProduction   = "ses_production"
	CheckSESSending      = "ses_sending"
	CheckSNSSubscription = "sns_subscription"
	CheckHTTPSCert       = "https_cert"
	CheckBackupRecent    = "backup_recent"
	CheckClockSkew       = "clock_skew"
	CheckDiskSpace       = "disk_space"
	CheckConfig          = "config"
)

// Check is the result of one doctor check.
type Check struct {
	Name    string `json:"name" doc:"Check name" example:"dns_dkim"`
	Status  Status `json:"status" enum:"ok,warn,fail,skipped" doc:"ok, warn, fail, or skipped when the check doesn't apply" example:"ok"`
	Message string `json:"message" doc:"What was found" example:"3 of 3 DKIM CNAME records point to SES"`
	FixHint string `json:"fix_hint,omitempty" doc:"How to fix a warn or fail" example:""`
}

// Report is the result of a doctor run (L-4).
type Report struct {
	Status     Status    `json:"status" enum:"ok,warn,fail" doc:"The worst status of all checks (skipped counts as ok)" example:"ok"`
	CheckedAt  time.Time `json:"checked_at" doc:"When the checks started"`
	DurationMS int64     `json:"duration_ms" doc:"How long the checks took, in milliseconds" example:"840"`
	Checks     []Check   `json:"checks" doc:"Every check, in a fixed order"`
}

// Exit codes of `mailroom doctor` (§6.13): 0 ok (warnings included),
// 2 when a check failed.
const (
	ExitOK          = 0
	ExitCheckFailed = 2
)

// ExitCode is the CLI exit code for the report: ExitCheckFailed when any
// check failed, else ExitOK.
func (r Report) ExitCode() int {
	if r.Status == StatusFail {
		return ExitCheckFailed
	}
	return ExitOK
}

// JSON returns the report as indented JSON (the --json output).
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Text returns the report as a plain-text table for the terminal.
func (r Report) Text() string {
	width := 0
	for _, c := range r.Checks {
		width = max(width, len(c.Name))
	}
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-7s  %-*s  %s\n", strings.ToUpper(string(c.Status)), width, c.Name, c.Message)
		if c.FixHint != "" && c.Status != StatusOK && c.Status != StatusSkipped {
			fmt.Fprintf(&b, "%-7s  %-*s  fix: %s\n", "", width, "", c.FixHint)
		}
	}
	fmt.Fprintf(&b, "\noverall: %s (%d checks in %d ms)\n", strings.ToUpper(string(r.Status)), len(r.Checks), r.DurationMS)
	return b.String()
}

// checkFunc runs one check. The runner fills in Name.
type checkFunc func(r *doctorRun, ctx context.Context) Check

type checkDef struct {
	name string
	fn   checkFunc
}

// allChecks is every doctor check in report order.
var allChecks = []checkDef{
	{CheckDNSApp, (*doctorRun).checkDNSApp},
	{CheckDNSDKIM, (*doctorRun).checkDNSDKIM},
	{CheckDNSMailFrom, (*doctorRun).checkDNSMailFrom},
	{CheckDNSDMARC, (*doctorRun).checkDNSDMARC},
	{CheckSESIdentity, (*doctorRun).checkSESIdentity},
	{CheckSESProduction, (*doctorRun).checkSESProduction},
	{CheckSESSending, (*doctorRun).checkSESSending},
	{CheckSNSSubscription, (*doctorRun).checkSNSSubscription},
	{CheckHTTPSCert, (*doctorRun).checkHTTPSCert},
	{CheckBackupRecent, (*doctorRun).checkBackupRecent},
	{CheckClockSkew, (*doctorRun).checkClockSkew},
	{CheckDiskSpace, (*doctorRun).checkDiskSpace},
	{CheckConfig, (*doctorRun).checkConfig},
}

// CheckNames returns the names of all doctor checks in report order.
func CheckNames() []string {
	out := make([]string, len(allChecks))
	for i, c := range allChecks {
		out[i] = c.name
	}
	return out
}

// doctorRun holds what several checks of one run share: the SES identity
// and account are fetched at most once.
type doctorRun struct {
	s        *Service
	now      time.Time
	identity func() (*sesv2.GetEmailIdentityOutput, error)
	account  func() (*sesv2.GetAccountOutput, error)
	settings func() (settings.Settings, error)
}

func (s *Service) newRun(ctx context.Context) *doctorRun {
	r := &doctorRun{s: s, now: s.clock.Now()}
	r.identity = sync.OnceValues(func() (*sesv2.GetEmailIdentityOutput, error) {
		if s.ses == nil {
			return nil, errAWSNotConfigured
		}
		return s.ses.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: &s.cfg.Domain})
	})
	r.account = sync.OnceValues(func() (*sesv2.GetAccountOutput, error) {
		if s.ses == nil {
			return nil, errAWSNotConfigured
		}
		return s.ses.GetAccount(ctx, &sesv2.GetAccountInput{})
	})
	r.settings = sync.OnceValues(func() (settings.Settings, error) {
		if s.settings == nil {
			return settings.Settings{}, fmt.Errorf("settings are not available")
		}
		return s.settings.Get(ctx)
	})
	return r
}

// Doctor runs every check concurrently, within DoctorTimeout overall, and
// returns the report. It never fails: problems are check results.
func (s *Service) Doctor(ctx context.Context) Report {
	return s.runChecks(ctx, allChecks)
}

// runChecks runs defs concurrently. A check that hasn't returned when the
// run's timeout expires is reported as failed.
func (s *Service) runChecks(ctx context.Context, defs []checkDef) Report {
	ctx, cancel := context.WithTimeout(ctx, s.doctorTimeout)
	defer cancel()
	run := s.newRun(ctx)
	start := s.clock.Now()

	type result struct {
		i int
		c Check
	}
	results := make(chan result, len(defs)) // buffered: late checks never block
	for i, d := range defs {
		go func() {
			results <- result{i, safeCheck(run, ctx, d)}
		}()
	}
	checks := make([]Check, len(defs))
	done := make([]bool, len(defs))
collect:
	for n := 0; n < len(defs); n++ {
		select {
		case r := <-results:
			checks[r.i], done[r.i] = r.c, true
		case <-ctx.Done():
			break collect
		}
	}
drain: // keep results that arrived together with the timeout
	for {
		select {
		case r := <-results:
			checks[r.i], done[r.i] = r.c, true
		default:
			break drain
		}
	}
	for i, d := range defs {
		if !done[i] {
			checks[i] = Check{Name: d.name, Status: StatusFail,
				Message: fmt.Sprintf("did not finish within %s", s.doctorTimeout),
				FixHint: "A DNS or AWS request hangs; check the server's network and DNS resolver, then run the doctor again"}
		}
	}
	rep := Report{Status: StatusOK, CheckedAt: start, Checks: checks,
		DurationMS: s.clock.Now().Sub(start).Milliseconds()}
	for _, c := range checks {
		rep.Status = worse(rep.Status, c.Status)
	}
	if rep.Status == StatusSkipped {
		rep.Status = StatusOK
	}
	return rep
}

// safeCheck runs one check, turning a panic into a failed result.
func safeCheck(run *doctorRun, ctx context.Context, d checkDef) (c Check) {
	defer func() {
		if p := recover(); p != nil {
			run.s.log.Error("system: doctor check panicked", "check", d.name, "panic", fmt.Sprint(p))
			c = Check{Status: StatusFail, Message: "the check crashed: " + fmt.Sprint(p)}
		}
		c.Name = d.name
	}()
	return d.fn(run, ctx)
}

func ok(msg string, args ...any) Check {
	return Check{Status: StatusOK, Message: fmt.Sprintf(msg, args...)}
}

func skipped(msg string, args ...any) Check {
	return Check{Status: StatusSkipped, Message: fmt.Sprintf(msg, args...)}
}

func warn(hint, msg string, args ...any) Check {
	return Check{Status: StatusWarn, Message: fmt.Sprintf(msg, args...), FixHint: hint}
}

func fail(hint, msg string, args ...any) Check {
	return Check{Status: StatusFail, Message: fmt.Sprintf(msg, args...), FixHint: hint}
}
