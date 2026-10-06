package system

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mailroom/internal/config"
)

// Thresholds of the local checks.
const (
	CertWarnBefore   = 14 * 24 * time.Hour // autocert renews 30 days ahead, so < 14 days means renewal is failing
	CertFailBefore   = 3 * 24 * time.Hour
	BackupMaxAge     = 26 * time.Hour
	ClockSkewWarn    = 30 * time.Second
	ClockSkewFail    = 5 * time.Minute
	DiskWarnBelow    = 1 << 30   // 1 GiB
	DiskFailBelow    = 200 << 20 // 200 MiB
	clockSkewTimeout = 5 * time.Second
)

func (r *doctorRun) checkHTTPSCert(ctx context.Context) Check {
	s := r.s
	host := s.cfg.BaseHost()
	switch {
	case s.cfg.HTTPOnly:
		return skipped("HTTP_ONLY is set: HTTPS is terminated by the reverse proxy")
	case isLocalHost(host):
		return skipped("BASE_URL points to %s; no public certificate", host)
	case s.certs == nil:
		return skipped("no certificate checker configured")
	}
	notAfter, err := s.certs(ctx, host)
	if err != nil {
		return fail(fmt.Sprintf("Check that ports 80 and 443 are open, that %s points to this server, and the autocert errors in the logs (Let's Encrypt issues the certificate on the first HTTPS request)", host),
			"no valid certificate on %s:443: %s", host, err)
	}
	left := notAfter.Sub(r.now)
	days := int(left.Hours() / 24)
	msg := fmt.Sprintf("the certificate of %s is valid until %s (%d days)", host, notAfter.UTC().Format(time.DateOnly), days)
	hint := "Certificates renew automatically 30 days before expiry; check the autocert errors in the logs and that port 80/443 are reachable"
	switch {
	case left < CertFailBefore:
		return fail(hint, "%s", msg)
	case left < CertWarnBefore:
		return warn(hint, "%s", msg)
	}
	return ok("%s", msg)
}

// checkBackupRecent looks at the newest scheduled backup (BackupPrefix):
// pre-restore- and pre-migrate- safety copies don't count, and a stamp in
// the future is ignored with a warning, so neither can hide missing
// nightly backups.
func (r *doctorRun) checkBackupRecent(context.Context) Check {
	s := r.s
	list, err := s.ListBackups()
	if err != nil {
		return fail("Check that the data directory is readable", "can't list backups: %s", err)
	}
	last, found, future := latestScheduled(list, r.now)
	var futureNote, futureHint string
	if len(future) > 0 {
		futureNote = fmt.Sprintf("; ignored %s dated in the future (the server clock was ahead, or the file was copied in)", strings.Join(future, ", "))
		futureHint = "Check the server clock (`timedatectl`) and delete backups dated in the future unless you need them"
	}
	takeOne := "Take one with POST /api/v1/system/backups or `mailroom backup`; the nightly backup runs at 02:30"
	if !found {
		if len(future) > 0 {
			return fail(takeOne+". "+futureHint, "no backup with a plausible date in %s%s", s.cfg.BackupsDir(), futureNote)
		}
		return warn(takeOne, "no backup in %s yet", s.cfg.BackupsDir())
	}
	age := r.now.Sub(last.CreatedAt)
	where := ""
	if s.cfg.BackupBucket != "" {
		where = fmt.Sprintf("; nightly backups are also uploaded to s3://%s", s.cfg.BackupBucket)
	}
	if age > BackupMaxAge {
		hint := "Check the logs for backup errors (disk space, permissions) and run `mailroom backup` or POST /api/v1/system/backups"
		if futureHint != "" {
			hint += ". " + futureHint
		}
		return fail(hint, "the last backup %s is %s old (limit %s)%s", last.Name, roundAge(age), roundAge(BackupMaxAge), futureNote)
	}
	if len(future) > 0 {
		return warn(futureHint, "the last backup %s is %s old%s%s", last.Name, roundAge(age), where, futureNote)
	}
	return ok("the last backup %s is %s old%s", last.Name, roundAge(age), where)
}

// roundAge formats a duration for messages: "3h12m", "2d4h".
func roundAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 48*time.Hour {
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd%dh", days, int((d-time.Duration(days)*24*time.Hour)/time.Hour))
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Truncate(time.Minute).String(), "0s")
}

// sesEndpoint is the HTTPS endpoint whose Date header the clock is compared with.
func sesEndpoint(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return "https://email." + region + ".amazonaws.com/"
}

func (r *doctorRun) checkClockSkew(ctx context.Context) Check {
	s := r.s
	if s.http == nil {
		return skipped("no HTTP client configured")
	}
	url := sesEndpoint(s.cfg.AWSRegion)
	ctx, cancel := context.WithTimeout(ctx, clockSkewTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fail("", "build request: %s", err)
	}
	hint := "Enable time synchronisation on the server (`timedatectl set-ntp true`); SES rejects requests signed with a clock more than 5 minutes off"
	before := s.clock.Now()
	resp, err := s.http.Do(req)
	after := s.clock.Now()
	if err != nil {
		return warn("Check the server's outbound HTTPS access to AWS", "couldn't reach %s to compare clocks: %s", url, err)
	}
	resp.Body.Close()
	remote, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return warn("", "%s sent no usable Date header", url)
	}
	local := before.Add(after.Sub(before) / 2)
	skew := local.Sub(remote)
	abs := skew.Abs()
	// The Date header has one-second resolution.
	msg := fmt.Sprintf("the server clock is %s %s AWS (%s)", abs.Round(time.Second), aheadBehind(skew), url)
	switch {
	case abs > ClockSkewFail:
		return fail(hint, "%s", msg)
	case abs > ClockSkewWarn:
		return warn(hint, "%s", msg)
	}
	return ok("%s", msg)
}

func aheadBehind(skew time.Duration) string {
	if skew >= 0 {
		return "ahead of"
	}
	return "behind"
}

func (r *doctorRun) checkDiskSpace(context.Context) Check {
	s := r.s
	dir := s.cfg.DataDir
	free, err := s.diskFree(dir)
	if errors.Is(err, errDiskUnsupported) {
		return skipped("free disk space can't be measured on this platform")
	}
	if err != nil {
		return warn("Check that the data directory exists and is readable", "can't read the free space of %s: %s", dir, err)
	}
	msg := fmt.Sprintf("%s free in %s", formatBytes(free), dir)
	hint := "Free up disk space or grow the volume: the database, its WAL file and 14 days of backups live in the data directory"
	switch {
	case free < DiskFailBelow:
		return fail(hint, "%s (below %s)", msg, formatBytes(DiskFailBelow))
	case free < DiskWarnBelow:
		return warn(hint, "%s (below %s)", msg, formatBytes(DiskWarnBelow))
	}
	return ok("%s", msg)
}

// formatBytes formats n with binary units: "1.5 GiB".
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func (r *doctorRun) checkConfig(context.Context) Check {
	s := r.s
	domain := s.cfg.Domain
	status := StatusOK
	var problems, hints []string
	add := func(st Status, problem, hint string) {
		status = worse(status, st)
		problems = append(problems, problem)
		hints = append(hints, hint)
	}
	if len(s.cfg.Secret) < config.MinSecretLength {
		add(StatusFail, "the HMAC secret is missing or too short",
			fmt.Sprintf("Set MAILROOM_SECRET (at least %d characters) or let Mailroom create %s", config.MinSecretLength, s.cfg.SecretKeyPath()))
	}
	st, err := r.settings()
	if err != nil {
		add(StatusFail, "can't read the settings: "+err.Error(), "Check the database")
	} else {
		switch from := st.Sender.FromEmail; {
		case from == "":
			add(StatusFail, "no From address is set", "PATCH /api/v1/settings with sender.from_email on "+domain)
		case !strings.HasSuffix(from, "@"+domain):
			add(StatusFail, fmt.Sprintf("the From address is not on the sending domain %s", domain), "PATCH /api/v1/settings with sender.from_email on "+domain)
		}
		if rt := st.Sender.ReplyTo; rt != "" && !strings.HasSuffix(rt, "@"+domain) {
			add(StatusFail, fmt.Sprintf("the Reply-To address is not on the sending domain %s", domain), "PATCH /api/v1/settings with sender.reply_to on "+domain)
		}
		if st.AlertAddress() == "" {
			add(StatusWarn, "no owner or alert address is set, so alerts can't be delivered", "PATCH /api/v1/settings with owner_email or alert_email")
		}
		if strings.TrimSpace(st.CompanyFooter) == "" {
			add(StatusFail, "the company footer is empty: marketing emails must show the sender's name and postal address, so broadcasts can't be sent and sequence emails wait until it is set", "PATCH /api/v1/settings with company_footer")
		}
	}
	if status == StatusOK {
		return ok("secret present, sender addresses on %s, alert address set", domain)
	}
	return Check{Status: status, Message: strings.Join(problems, "; "), FixHint: strings.Join(hints, "; ")}
}
