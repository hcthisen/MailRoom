package system

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"

	"mailroom/internal/testutil"
)

const day = 24 * time.Hour

// healthyFixture is a fixture with a recent backup, so every check is ok.
func healthyFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.writeBackupFile(BackupPrefix, f.clock.Now().Add(-time.Hour))
	return f
}

func TestDoctorAllHealthy(t *testing.T) {
	f := healthyFixture(t)
	rep := f.service().Doctor(f.ctx)
	if rep.Status != StatusOK || rep.ExitCode() != ExitOK {
		t.Fatalf("status = %s\n%s", rep.Status, rep.Text())
	}
	if len(rep.Checks) != len(CheckNames()) {
		t.Fatalf("got %d checks, want %d", len(rep.Checks), len(CheckNames()))
	}
	for i, c := range rep.Checks {
		if c.Name != CheckNames()[i] {
			t.Errorf("check %d = %s, want %s (fixed order)", i, c.Name, CheckNames()[i])
		}
		if c.Status != StatusOK {
			t.Errorf("%s = %s: %s", c.Name, c.Status, c.Message)
		}
		if c.Message == "" {
			t.Errorf("%s has no message", c.Name)
		}
	}
	// SES is called once per run, although several checks need it.
	if n := f.ses.callCount("GetAccount"); n != 1 {
		t.Errorf("GetAccount called %d times", n)
	}
	if n := f.ses.callCount("GetEmailIdentity"); n != 1 {
		t.Errorf("GetEmailIdentity called %d times", n)
	}
	if got := f.sns.topics; len(got) != 1 || got[0] != f.cfg.SNSTopicARN {
		t.Errorf("SNS topics listed = %v", got)
	}
	if len(f.http.reqs) != 1 || f.http.reqs[0].URL.String() != "https://email.eu-west-1.amazonaws.com/" {
		t.Errorf("clock-skew requests = %v", f.http.reqs)
	}
}

func TestDoctorChecks(t *testing.T) {
	mailFromOnly := func(f *fixture, st sestypes.MailFromDomainStatus) {
		f.ses.identity.MailFromAttributes.MailFromDomainStatus = st
	}
	cases := []struct {
		name   string
		check  string
		mutate func(f *fixture)
		want   Status
		msg    string // substring of the message
		hint   string // substring of the fix hint
	}{
		// dns_app
		{"app ok", CheckDNSApp, nil, StatusOK, "203.0.113.10", ""},
		{"app missing", CheckDNSApp, func(f *fixture) { delete(f.resolver.hosts, "newsletter.example.com") }, StatusFail, "no A/AAAA record", "A (and AAAA) record"},
		{"app localhost", CheckDNSApp, func(f *fixture) { f.cfg.BaseURL = "http://localhost:8080" }, StatusSkipped, "localhost", ""},
		{"app no resolver", CheckDNSApp, func(f *fixture) { f.resolver = nil }, StatusSkipped, "resolver", ""},

		// dns_dkim
		{"dkim ok", CheckDNSDKIM, nil, StatusOK, "3 of 3", ""},
		{"dkim wrong target", CheckDNSDKIM, func(f *fixture) { f.resolver.cnames["tok2._domainkey.example.com"] = "evil.example.net." }, StatusFail, "evil.example.net", "mailroom dns records"},
		// Regional-DKIM regions: the records point into SES's signing zone,
		// as provision creates them.
		{"dkim regional zone", CheckDNSDKIM, func(f *fixture) {
			f.ses.identity.DkimAttributes.SigningHostedZone = aws.String("dkim.eu-south-1.amazonses.com")
			for _, tok := range []string{"tok1", "tok2", "tok3"} {
				f.resolver.cnames[tok+"._domainkey.example.com"] = tok + ".dkim.eu-south-1.amazonses.com."
			}
		}, StatusOK, "3 of 3", ""},
		{"dkim global target in a regional zone", CheckDNSDKIM, func(f *fixture) {
			f.ses.identity.DkimAttributes.SigningHostedZone = aws.String("dkim.eu-south-1.amazonses.com")
		}, StatusFail, "not tok1.dkim.eu-south-1.amazonses.com", ""},
		{"dkim missing", CheckDNSDKIM, func(f *fixture) { delete(f.resolver.cnames, "tok1._domainkey.example.com") }, StatusFail, "tok1._domainkey.example.com: missing", ""},
		{"dkim no tokens", CheckDNSDKIM, func(f *fixture) { f.ses.identity.DkimAttributes.Tokens = nil }, StatusFail, "no DKIM tokens", "provision ses"},
		{"dkim identity unreadable", CheckDNSDKIM, func(f *fixture) { f.ses.identityErr = errBoom }, StatusSkipped, "ses_identity", ""},

		// dns_mail_from
		{"mail from ok", CheckDNSMailFrom, nil, StatusOK, "bounce.example.com", ""},
		{"mail from wrong mx", CheckDNSMailFrom, func(f *fixture) {
			f.resolver.mx["bounce.example.com"] = []*net.MX{{Host: "mx.example.net.", Pref: 10}}
		}, StatusFail, "not feedback-smtp.eu-west-1.amazonses.com", "MX 10 feedback-smtp.eu-west-1.amazonses.com"},
		{"mail from other region", CheckDNSMailFrom, func(f *fixture) {
			f.resolver.mx["bounce.example.com"] = []*net.MX{{Host: "feedback-smtp.us-east-1.amazonses.com.", Pref: 10}}
		}, StatusFail, "MX on bounce.example.com", ""},
		{"mail from no mx", CheckDNSMailFrom, func(f *fixture) { delete(f.resolver.mx, "bounce.example.com") }, StatusFail, "no MX record", ""},
		{"mail from no spf", CheckDNSMailFrom, func(f *fixture) { delete(f.resolver.txt, "bounce.example.com") }, StatusFail, "no SPF TXT record", ""},
		{"mail from spf without ses", CheckDNSMailFrom, func(f *fixture) {
			f.resolver.txt["bounce.example.com"] = []string{"v=spf1 include:_spf.google.com ~all"}
		}, StatusFail, "doesn't include amazonses.com", ""},
		{"mail from any region without AWS_REGION", CheckDNSMailFrom, func(f *fixture) { f.cfg.AWSRegion = "" }, StatusOK, "", ""},
		{"mail from missing without AWS_REGION", CheckDNSMailFrom, func(f *fixture) {
			f.cfg.AWSRegion = ""
			delete(f.resolver.mx, "bounce.example.com")
		}, StatusFail, "no MX record", "AWS_REGION is not set"},

		// dns_dmarc
		{"dmarc ok", CheckDNSDMARC, nil, StatusOK, "v=DMARC1", ""},
		{"dmarc missing", CheckDNSDMARC, func(f *fixture) { delete(f.resolver.txt, "_dmarc.example.com") }, StatusWarn, "no DMARC record", "v=DMARC1"},
		{"dmarc other txt", CheckDNSDMARC, func(f *fixture) { f.resolver.txt["_dmarc.example.com"] = []string{"google-site-verification=x"} }, StatusWarn, "", ""},

		// ses_identity
		{"identity ok", CheckSESIdentity, nil, StatusOK, "verified", ""},
		{"identity not verified", CheckSESIdentity, func(f *fixture) { f.ses.identity.VerifiedForSendingStatus = false }, StatusFail, "not verified for sending", ""},
		{"identity dkim pending", CheckSESIdentity, func(f *fixture) { f.ses.identity.DkimAttributes.Status = sestypes.DkimStatusPending }, StatusWarn, "DKIM verification is pending", ""},
		{"identity dkim failed", CheckSESIdentity, func(f *fixture) { f.ses.identity.DkimAttributes.Status = sestypes.DkimStatusFailed }, StatusFail, "DKIM status is FAILED", "mailroom dns records"},
		{"identity mail from pending", CheckSESIdentity, func(f *fixture) { mailFromOnly(f, sestypes.MailFromDomainStatusPending) }, StatusWarn, "MAIL FROM verification is pending", ""},
		{"identity mail from failed", CheckSESIdentity, func(f *fixture) { mailFromOnly(f, sestypes.MailFromDomainStatusFailed) }, StatusFail, "MAIL FROM status is FAILED", ""},
		{"identity mail from other domain", CheckSESIdentity, func(f *fixture) {
			f.ses.identity.MailFromAttributes.MailFromDomain = aws.String("mail.example.com")
		}, StatusFail, "not bounce.example.com", "provision ses"},
		{"identity api error", CheckSESIdentity, func(f *fixture) { f.ses.identityErr = errBoom }, StatusFail, "boom", "ses:GetEmailIdentity"},
		{"identity no aws", CheckSESIdentity, func(f *fixture) { f.ses = nil }, StatusFail, "AWS is not configured", "sudo docker compose up -d` (a plain restart keeps the old environment)"},

		// ses_production
		{"production ok", CheckSESProduction, nil, StatusOK, "production access", ""},
		{"sandbox", CheckSESProduction, func(f *fixture) { f.ses.account.ProductionAccessEnabled = false }, StatusWarn, "sandbox", "--request-production"},
		{"sandbox pending review", CheckSESProduction, func(f *fixture) {
			f.ses.account.ProductionAccessEnabled = false
			f.ses.account.Details = &sestypes.AccountDetails{ReviewDetails: &sestypes.ReviewDetails{Status: sestypes.ReviewStatusPending}}
		}, StatusWarn, "request: PENDING", ""},
		{"production api error", CheckSESProduction, func(f *fixture) { f.ses.accountErr = errBoom }, StatusFail, "GetAccount failed", "ses:GetAccount"},

		// ses_sending
		{"sending ok", CheckSESSending, nil, StatusOK, "1200 of 50000", ""},
		{"sending paused", CheckSESSending, func(f *fixture) { f.ses.account.SendingEnabled = false }, StatusFail, "paused", "SES console"},
		{"probation", CheckSESSending, func(f *fixture) { f.ses.account.EnforcementStatus = aws.String("PROBATION") }, StatusWarn, "probation", ""},
		{"shutdown", CheckSESSending, func(f *fixture) { f.ses.account.EnforcementStatus = aws.String("SHUTDOWN") }, StatusFail, "SHUTDOWN", ""},
		{"quota 95%", CheckSESSending, func(f *fixture) { f.ses.account.SendQuota.SentLast24Hours = 47500 }, StatusWarn, "95.0 %", "higher sending quota"},
		{"quota used up", CheckSESSending, func(f *fixture) { f.ses.account.SendQuota.SentLast24Hours = 50000 }, StatusFail, "100.0 %", ""},
		{"quota unlimited", CheckSESSending, func(f *fixture) { f.ses.account.SendQuota.Max24HourSend = -1 }, StatusOK, "no daily limit", ""},
		{"sending no aws", CheckSESSending, func(f *fixture) { f.ses = nil }, StatusFail, "AWS is not configured", "sudo docker compose up -d"},

		// sns_subscription
		{"sns ok", CheckSNSSubscription, nil, StatusOK, "is subscribed", ""},
		{"sns ok on second page", CheckSNSSubscription, func(f *fixture) {
			f.sns.pages = []*sns.ListSubscriptionsByTopicOutput{
				{Subscriptions: []snstypes.Subscription{}},
				{Subscriptions: []snstypes.Subscription{subscription("https://newsletter.example.com/hooks/ses", "arn:y")}},
			}
		}, StatusOK, "nothing else", ""},
		// Another subscription gets every recipient address, and survives
		// rotating a leaked sending key: doctor names it.
		{"sns other subscriptions", CheckSNSSubscription, func(f *fixture) {
			evil := snstypes.Subscription{Endpoint: aws.String("spy@evil.example"), SubscriptionArn: aws.String("arn:z"), Protocol: aws.String("email")}
			f.sns.pages = []*sns.ListSubscriptionsByTopicOutput{
				{Subscriptions: []snstypes.Subscription{subscription("https://other.example.com/hooks/ses", "arn:x"), evil}},
				{Subscriptions: []snstypes.Subscription{subscription("https://newsletter.example.com/hooks/ses", "arn:y")}},
			}
		}, StatusWarn, "2 other subscription(s) also receive every event with its recipient addresses: https https://other.example.com/hooks/ses, email spy@evil.example", "rotate the sending key"},
		{"sns pending", CheckSNSSubscription, func(f *fixture) {
			f.sns.pages[0].Subscriptions[0].SubscriptionArn = aws.String("PendingConfirmation")
		}, StatusWarn, "pending confirmation", "reachable"},
		{"sns not subscribed", CheckSNSSubscription, func(f *fixture) {
			f.sns.pages[0].Subscriptions[0].Endpoint = aws.String("https://old.example.com/hooks/ses")
		}, StatusFail, "is not subscribed", "restart Mailroom"},
		{"sns no topic", CheckSNSSubscription, func(f *fixture) { f.cfg.SNSTopicARN = "" }, StatusFail, "SNS_TOPIC_ARN is not set", "provision ses"},
		{"sns api error", CheckSNSSubscription, func(f *fixture) { f.sns.err = errBoom }, StatusFail, "boom", "sns:ListSubscriptionsByTopic"},
		{"sns no aws", CheckSNSSubscription, func(f *fixture) { f.sns = nil }, StatusFail, "AWS is not configured", "sudo docker compose up -d"},

		// https_cert
		{"cert ok", CheckHTTPSCert, nil, StatusOK, "60 days", ""},
		{"cert expiring", CheckHTTPSCert, func(f *fixture) { f.certExp = f.clock.Now().Add(10 * day) }, StatusWarn, "10 days", "renew automatically"},
		{"cert nearly expired", CheckHTTPSCert, func(f *fixture) { f.certExp = f.clock.Now().Add(2 * day) }, StatusFail, "2 days", ""},
		{"cert invalid", CheckHTTPSCert, func(f *fixture) { f.certErr = errors.New("x509: certificate signed by unknown authority") }, StatusFail, "unknown authority", "ports 80 and 443"},
		{"cert http only", CheckHTTPSCert, func(f *fixture) { f.cfg.HTTPOnly = true }, StatusSkipped, "HTTP_ONLY", ""},

		// backup_recent
		{"backup ok", CheckBackupRecent, nil, StatusOK, "1h0m old", ""},
		{"backup old", CheckBackupRecent, func(f *fixture) { f.clock.Advance(26 * time.Hour) }, StatusFail, "27h0m old", "mailroom backup"},
		{"backup none", CheckBackupRecent, func(f *fixture) { f.cfg.DataDir = f.t.TempDir() }, StatusWarn, "no backup", "POST /api/v1/system/backups"},
		{"backup mentions bucket", CheckBackupRecent, func(f *fixture) { f.cfg.BackupBucket = "mailroom-backup-x" }, StatusOK, "s3://mailroom-backup-x", ""},
		{"backup dated in the future is ignored", CheckBackupRecent, func(f *fixture) {
			f.clock.Advance(30 * time.Hour)
			f.writeBackupFile(BackupPrefix, f.clock.Now().Add(2*day))
		}, StatusFail, "31h0m old (limit 26h0m); ignored mailroom-20260108T150000Z.db dated in the future", "timedatectl"},
		{"backup dated in the future warns", CheckBackupRecent, func(f *fixture) {
			f.writeBackupFile(BackupPrefix, f.clock.Now().Add(time.Hour))
		}, StatusWarn, "1h0m old; ignored mailroom-20260105T100000Z.db", "delete backups dated in the future"},
		{"backup within clock tolerance counts", CheckBackupRecent, func(f *fixture) {
			f.clock.Advance(30 * time.Hour)
			f.writeBackupFile(BackupPrefix, f.clock.Now().Add(time.Minute))
		}, StatusOK, "mailroom-20260106T150100Z.db is 0s old", ""},
		{"backup only dated in the future", CheckBackupRecent, func(f *fixture) {
			f.cfg.DataDir = f.t.TempDir()
			f.writeBackupFile(BackupPrefix, f.clock.Now().Add(day))
		}, StatusFail, "no backup with a plausible date", "timedatectl"},
		{"backup safety copy doesn't count", CheckBackupRecent, func(f *fixture) {
			f.clock.Advance(30 * time.Hour)
			f.writeBackupFile(PreRestorePrefix, f.clock.Now())
			f.writeBackupFile("pre-migrate-v2-", f.clock.Now())
		}, StatusFail, "mailroom-20260105T080000Z.db is 31h0m old", "mailroom backup"},
		{"backup only safety copies", CheckBackupRecent, func(f *fixture) {
			f.cfg.DataDir = f.t.TempDir()
			f.writeBackupFile(PreRestorePrefix, f.clock.Now())
		}, StatusWarn, "no backup", "POST /api/v1/system/backups"},

		// clock_skew
		{"clock ok", CheckClockSkew, nil, StatusOK, "0s ahead of AWS", ""},
		{"clock 45s behind", CheckClockSkew, func(f *fixture) { f.http.date = f.clock.Now().Add(45 * time.Second) }, StatusWarn, "45s behind", "timedatectl"},
		{"clock 6m ahead", CheckClockSkew, func(f *fixture) { f.http.date = f.clock.Now().Add(-6 * time.Minute) }, StatusFail, "6m0s ahead", ""},
		{"clock unreachable", CheckClockSkew, func(f *fixture) { f.http.err = errBoom }, StatusWarn, "couldn't reach", ""},
		{"clock no date", CheckClockSkew, func(f *fixture) { f.http.date = time.Time{} }, StatusWarn, "no usable Date", ""},
		{"clock no client", CheckClockSkew, func(f *fixture) { f.http = nil }, StatusSkipped, "", ""},

		// disk_space
		{"disk ok", CheckDiskSpace, nil, StatusOK, "10.0 GiB free", ""},
		{"disk low", CheckDiskSpace, func(f *fixture) { f.free = 500 << 20 }, StatusWarn, "500.0 MiB", "Free up disk space"},
		{"disk critical", CheckDiskSpace, func(f *fixture) { f.free = 100 << 20 }, StatusFail, "below 200.0 MiB", ""},
		{"disk unreadable", CheckDiskSpace, func(f *fixture) { f.freeErr = errBoom }, StatusWarn, "boom", ""},
		{"disk unsupported", CheckDiskSpace, func(f *fixture) { f.freeErr = errDiskUnsupported }, StatusSkipped, "", ""},

		// config
		{"config ok", CheckConfig, nil, StatusOK, "secret present", ""},
		{"config no secret", CheckConfig, func(f *fixture) { f.cfg.Secret = nil }, StatusFail, "HMAC secret", "MAILROOM_SECRET"},
		{"config from off domain", CheckConfig, func(f *fixture) {
			testutil.Exec(f.t, f.d, `UPDATE settings SET value = json_set(value, '$.sender.from_email', 'news@other.org') WHERE key = 'app'`)
		}, StatusFail, "From address is not on the sending domain", "sender.from_email"},
		{"config no from", CheckConfig, func(f *fixture) {
			testutil.Exec(f.t, f.d, `UPDATE settings SET value = json_set(value, '$.sender.from_email', '') WHERE key = 'app'`)
		}, StatusFail, "no From address", ""},
		{"config no alert address", CheckConfig, func(f *fixture) {
			testutil.Exec(f.t, f.d, `UPDATE settings SET value = json_set(value, '$.owner_email', '', '$.alert_email', '') WHERE key = 'app'`)
		}, StatusWarn, "alerts can't be delivered", "owner_email"},
		{"config no footer", CheckConfig, func(f *fixture) {
			testutil.Exec(f.t, f.d, `UPDATE settings SET value = json_set(value, '$.company_footer', '') WHERE key = 'app'`)
		}, StatusFail, "company footer is empty", "company_footer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFixture(t)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			rep := f.service().Doctor(f.ctx)
			c := findCheck(t, rep, tc.check)
			if c.Status != tc.want {
				t.Fatalf("%s = %s (%s), want %s", tc.check, c.Status, c.Message, tc.want)
			}
			if !strings.Contains(c.Message, tc.msg) {
				t.Errorf("message %q doesn't contain %q", c.Message, tc.msg)
			}
			if !strings.Contains(c.FixHint, tc.hint) {
				t.Errorf("fix hint %q doesn't contain %q", c.FixHint, tc.hint)
			}
			if (tc.want == StatusWarn || tc.want == StatusFail) && c.FixHint == "" && tc.check != CheckClockSkew {
				t.Errorf("%s %s without a fix hint", tc.check, tc.want)
			}
			// The overall status is the worst check.
			worst := StatusOK
			for _, c := range rep.Checks {
				worst = worse(worst, c.Status)
			}
			if worst == StatusSkipped {
				worst = StatusOK
			}
			if rep.Status != worst {
				t.Errorf("overall %s, worst check %s", rep.Status, worst)
			}
		})
	}
}

func TestDoctorWithoutAWS(t *testing.T) {
	f := healthyFixture(t)
	f.ses, f.sns, f.s3 = nil, nil, nil
	f.cfg.AWSRegion, f.cfg.SNSTopicARN = "", ""
	rep := f.service().Doctor(f.ctx)
	if rep.Status != StatusFail || rep.ExitCode() != ExitCheckFailed {
		t.Fatalf("status %s, exit %d", rep.Status, rep.ExitCode())
	}
	for _, name := range []string{CheckSESIdentity, CheckSESProduction, CheckSESSending, CheckSNSSubscription} {
		c := findCheck(t, rep, name)
		if c.Status != StatusFail || !strings.Contains(c.FixHint, "provision ses") {
			t.Errorf("%s = %s %q (hint %q)", name, c.Status, c.Message, c.FixHint)
		}
	}
	if c := findCheck(t, rep, CheckDNSDKIM); c.Status != StatusSkipped {
		t.Errorf("dns_dkim = %s", c.Status)
	}
	if c := findCheck(t, rep, CheckDNSApp); c.Status != StatusOK {
		t.Errorf("dns_app = %s: DNS checks don't need AWS", c.Status)
	}
}

func TestDoctorTimeout(t *testing.T) {
	f := healthyFixture(t)
	f.resolver.block = make(chan struct{})
	t.Cleanup(func() { close(f.resolver.block) })
	s := f.service()
	s.doctorTimeout = 100 * time.Millisecond
	done := make(chan Report, 1)
	go func() { done <- s.Doctor(f.ctx) }()
	var rep Report
	select {
	case rep = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Doctor didn't return after its timeout")
	}
	for _, name := range []string{CheckDNSApp, CheckDNSMailFrom, CheckDNSDMARC} {
		if c := findCheck(t, rep, name); c.Status != StatusFail || !strings.Contains(c.Message, "did not finish") {
			t.Errorf("%s = %s %q", name, c.Status, c.Message)
		}
	}
	if c := findCheck(t, rep, CheckConfig); c.Status != StatusOK {
		t.Errorf("config = %s: checks that finished keep their result", c.Status)
	}
	if rep.Status != StatusFail {
		t.Errorf("overall = %s", rep.Status)
	}
}

func TestDoctorCheckPanicIsFail(t *testing.T) {
	f := healthyFixture(t)
	f.ses.identity = nil // GetEmailIdentity returns (nil, nil): a misbehaving client
	rep := f.service().Doctor(f.ctx)
	c := findCheck(t, rep, CheckSESIdentity)
	if c.Status != StatusFail || !strings.Contains(c.Message, "crashed") || c.Name != CheckSESIdentity {
		t.Fatalf("ses_identity = %+v", c)
	}
}

func TestReportRendering(t *testing.T) {
	rep := Report{
		Status: StatusFail, CheckedAt: testutil.Epoch, DurationMS: 12,
		Checks: []Check{
			{Name: CheckDNSApp, Status: StatusOK, Message: "fine", FixHint: "not shown"},
			{Name: CheckBackupRecent, Status: StatusFail, Message: "too old", FixHint: "run mailroom backup"},
			{Name: CheckHTTPSCert, Status: StatusSkipped, Message: "http only"},
		},
	}
	text := rep.Text()
	for _, want := range []string{"OK       dns_app        fine", "FAIL     backup_recent  too old", "fix: run mailroom backup", "SKIPPED  https_cert", "overall: FAIL (3 checks in 12 ms)"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not shown") {
		t.Error("fix hint of an ok check is printed")
	}
	b, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil || back.Status != StatusFail || len(back.Checks) != 3 || back.Checks[1].FixHint != "run mailroom backup" {
		t.Fatalf("JSON round trip = %+v, %v\n%s", back, err, b)
	}
	for _, tc := range []struct {
		status Status
		code   int
	}{{StatusOK, 0}, {StatusWarn, 0}, {StatusFail, 2}} {
		if got := (Report{Status: tc.status}).ExitCode(); got != tc.code {
			t.Errorf("ExitCode(%s) = %d, want %d", tc.status, got, tc.code)
		}
	}
}

func TestHelpers(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{30 * time.Second, "30s"}, {5 * time.Minute, "5m"}, {26*time.Hour + 5*time.Minute, "26h5m"}, {50 * time.Hour, "2d2h"}, {-time.Second, "0s"}} {
		if got := roundAge(tc.d); got != tc.want {
			t.Errorf("roundAge(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
	for _, tc := range []struct {
		n    uint64
		want string
	}{{512, "512 B"}, {1536, "1.5 KiB"}, {200 << 20, "200.0 MiB"}, {3 << 30, "3.0 GiB"}} {
		if got := formatBytes(tc.n); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
	for host, local := range map[string]bool{"localhost": true, "app.localhost": true, "127.0.0.1": true, "::1": true, "newsletter.example.com": false} {
		if isLocalHost(host) != local {
			t.Errorf("isLocalHost(%q) != %v", host, local)
		}
	}
}

func TestDefaultConstructors(t *testing.T) {
	if NewDefaultResolver() == nil || NewDefaultHTTPClient() == nil || NewTLSChecker(nil) == nil {
		t.Fatal("nil default")
	}
	// No AWS region: no clients and no error, so the checks report it.
	c, err := NewAWSClients(context.Background(), testConfig(t.TempDir()))
	if err != nil || c.SES == nil || c.SNS == nil || c.S3 == nil {
		t.Fatalf("configured clients = %+v, %v", c, err)
	}
	cfg := testConfig(t.TempDir())
	cfg.AWSRegion = ""
	c, err = NewAWSClients(context.Background(), cfg)
	if err != nil || c.SES != nil || c.SNS != nil || c.S3 != nil {
		t.Fatalf("unconfigured clients = %+v, %v", c, err)
	}
	// A canceled context fails the TLS check without network.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewTLSChecker(nil)(ctx, "newsletter.example.com"); err == nil {
		t.Fatal("TLS check with a canceled context succeeded")
	}
}
