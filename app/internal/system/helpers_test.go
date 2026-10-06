package system

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"

	"mailroom/internal/config"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

var errBoom = errors.New("boom")

// ---------------------------------------------------------------- SES

type fakeSES struct {
	mu          sync.Mutex
	account     *sesv2.GetAccountOutput
	accountErr  error
	identity    *sesv2.GetEmailIdentityOutput
	identityErr error
	calls       map[string]int
}

func goodAccount() *sesv2.GetAccountOutput {
	return &sesv2.GetAccountOutput{
		ProductionAccessEnabled: true,
		SendingEnabled:          true,
		EnforcementStatus:       aws.String("HEALTHY"),
		SendQuota:               &sestypes.SendQuota{Max24HourSend: 50000, MaxSendRate: 14, SentLast24Hours: 1200},
	}
}

func goodIdentity() *sesv2.GetEmailIdentityOutput {
	return &sesv2.GetEmailIdentityOutput{
		VerifiedForSendingStatus: true,
		DkimAttributes: &sestypes.DkimAttributes{
			Status: sestypes.DkimStatusSuccess,
			Tokens: []string{"tok1", "tok2", "tok3"},
		},
		MailFromAttributes: &sestypes.MailFromAttributes{
			MailFromDomain:       aws.String("bounce.example.com"),
			MailFromDomainStatus: sestypes.MailFromDomainStatusSuccess,
		},
	}
}

func (f *fakeSES) count(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[op]++
}

func (f *fakeSES) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *fakeSES) GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error) {
	f.count("GetAccount")
	return f.account, f.accountErr
}

func (f *fakeSES) GetEmailIdentity(_ context.Context, in *sesv2.GetEmailIdentityInput, _ ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error) {
	f.count("GetEmailIdentity")
	if f.identityErr != nil {
		return nil, f.identityErr
	}
	if aws.ToString(in.EmailIdentity) != "example.com" {
		return nil, fmt.Errorf("unexpected identity %q", aws.ToString(in.EmailIdentity))
	}
	return f.identity, nil
}

// ---------------------------------------------------------------- SNS

type fakeSNS struct {
	pages  []*sns.ListSubscriptionsByTopicOutput
	err    error
	topics []string
}

func subscription(endpoint, arn string) snstypes.Subscription {
	return snstypes.Subscription{Endpoint: aws.String(endpoint), SubscriptionArn: aws.String(arn), Protocol: aws.String("https")}
}

func (f *fakeSNS) ListSubscriptionsByTopic(_ context.Context, in *sns.ListSubscriptionsByTopicInput, _ ...func(*sns.Options)) (*sns.ListSubscriptionsByTopicOutput, error) {
	f.topics = append(f.topics, aws.ToString(in.TopicArn))
	if f.err != nil {
		return nil, f.err
	}
	page := 0
	if in.NextToken != nil {
		fmt.Sscanf(*in.NextToken, "page-%d", &page)
	}
	if page >= len(f.pages) {
		return &sns.ListSubscriptionsByTopicOutput{}, nil
	}
	out := *f.pages[page]
	if page+1 < len(f.pages) {
		out.NextToken = aws.String(fmt.Sprintf("page-%d", page+1))
	}
	return &out, nil
}

// ---------------------------------------------------------------- S3

type putRecord struct {
	bucket, key, sse, contentType string
	length                        int64
	body                          []byte
}

type fakeS3 struct {
	mu   sync.Mutex
	puts []putRecord
	err  error
	// failKey makes puts of that object key fail with errBoom.
	failKey string
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.failKey != "" && aws.ToString(in.Key) == f.failKey {
		return nil, errBoom
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.puts = append(f.puts, putRecord{
		bucket: aws.ToString(in.Bucket), key: aws.ToString(in.Key), sse: string(in.ServerSideEncryption),
		contentType: aws.ToString(in.ContentType), length: aws.ToInt64(in.ContentLength), body: body,
	})
	return &s3.PutObjectOutput{}, nil
}

// ---------------------------------------------------------------- DNS

type fakeResolver struct {
	hosts  map[string][]string
	cnames map[string]string
	mx     map[string][]*net.MX
	txt    map[string][]string
	// block, when set, makes every lookup wait until it is closed
	// (ignoring ctx), to test the doctor's overall timeout.
	block chan struct{}
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeResolver) wait() {
	if f.block != nil {
		<-f.block
	}
}

func (f *fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	f.wait()
	if v, ok := f.hosts[host]; ok {
		return v, nil
	}
	return nil, notFound(host)
}

func (f *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	f.wait()
	if v, ok := f.cnames[host]; ok {
		return v, nil
	}
	return "", notFound(host)
}

func (f *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	f.wait()
	if v, ok := f.mx[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.wait()
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func goodResolver() *fakeResolver {
	return &fakeResolver{
		hosts: map[string][]string{"newsletter.example.com": {"203.0.113.10", "2001:db8::10"}},
		cnames: map[string]string{
			"tok1._domainkey.example.com": "tok1.dkim.amazonses.com.",
			"tok2._domainkey.example.com": "tok2.dkim.amazonses.com.",
			"tok3._domainkey.example.com": "TOK3.dkim.amazonses.com",
		},
		mx: map[string][]*net.MX{"bounce.example.com": {{Host: "feedback-smtp.eu-west-1.amazonses.com.", Pref: 10}}},
		txt: map[string][]string{
			"bounce.example.com": {"v=spf1 include:amazonses.com ~all"},
			"_dmarc.example.com": {"v=DMARC1; p=none; rua=mailto:owner@example.org"},
		},
	}
}

// ---------------------------------------------------------------- HTTP, alerts

type fakeHTTP struct {
	date time.Time // Date header to answer with
	err  error
	reqs []*http.Request
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	h := http.Header{}
	if !f.date.IsZero() {
		h.Set("Date", f.date.UTC().Format(http.TimeFormat))
	}
	return &http.Response{StatusCode: http.StatusNotFound, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
}

type alertRec struct{ key, title, body string }

type fakeAlerter struct {
	mu     sync.Mutex
	alerts []alertRec
}

func (f *fakeAlerter) Alert(_ context.Context, _ *sql.Tx, key, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, alertRec{key, title, body})
	return nil
}

func (f *fakeAlerter) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.alerts {
		out = append(out, a.key)
	}
	return out
}

// ---------------------------------------------------------------- fixture

type fixture struct {
	t        *testing.T
	ctx      context.Context
	d        *db.DB
	clock    *testutil.Clock
	cfg      config.Config
	settings *settings.Service
	runner   *jobs.Runner
	ses      *fakeSES
	sns      *fakeSNS
	s3       *fakeS3
	resolver *fakeResolver
	http     *fakeHTTP
	alerter  *fakeAlerter
	certExp  time.Time
	certErr  error
	free     uint64
	freeErr  error
}

func testConfig(dataDir string) config.Config {
	return config.Config{
		DataDir:             dataDir,
		Domain:              "example.com",
		AppSubdomain:        "newsletter",
		BaseURL:             "https://newsletter.example.com",
		MailFromSubdomain:   "bounce",
		FromName:            "Example",
		FromEmail:           "news@example.com",
		OwnerEmail:          "owner@example.org",
		CompanyFooter:       "Example Ltd, 1 High Street",
		Timezone:            "UTC",
		AWSRegion:           "eu-west-1",
		SESConfigurationSet: "mailroom",
		SNSTopicARN:         "arn:aws:sns:eu-west-1:123456789012:mailroom",
		Secret:              []byte("0123456789abcdef0123456789abcdef"),
		HTTPAddr:            ":8080",
	}
}

// newFixture returns a fixture whose every integration is healthy, so each
// doctor check is ok until a test breaks one thing.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testutil.NewDB(t)
	clock := testutil.NewClock(time.Time{})
	cfg := testConfig(t.TempDir())
	st := settings.New(settings.Deps{DB: d, Clock: clock, Domain: cfg.Domain})
	if _, err := st.SeedFromConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, ctx: context.Background(), d: d, clock: clock, cfg: cfg, settings: st,
		runner:   jobs.New(jobs.Deps{DB: d, Clock: clock, Logger: quietLogger(), PollInterval: 20 * time.Millisecond}),
		ses:      &fakeSES{account: goodAccount(), identity: goodIdentity()},
		sns:      &fakeSNS{pages: []*sns.ListSubscriptionsByTopicOutput{{Subscriptions: []snstypes.Subscription{subscription("https://newsletter.example.com/hooks/ses", "arn:aws:sns:eu-west-1:123456789012:mailroom:abc")}}}},
		s3:       &fakeS3{},
		resolver: goodResolver(),
		http:     &fakeHTTP{date: clock.Now()},
		alerter:  &fakeAlerter{},
		certExp:  clock.Now().Add(60 * 24 * time.Hour),
		free:     10 << 30,
	}
	return f
}

// service builds the Service from the fixture's current state.
func (f *fixture) service() *Service {
	f.t.Helper()
	deps := Deps{
		DB: f.d, Clock: f.clock, Config: f.cfg, Settings: f.settings, Jobs: f.runner, Alerter: f.alerter,
		CertChecker: func(context.Context, string) (time.Time, error) { return f.certExp, f.certErr },
		DiskFree:    func(string) (uint64, error) { return f.free, f.freeErr },
		Version:     "1.2.3", StartedAt: f.clock.Now().Add(-time.Hour), Logger: quietLogger(),
	}
	// Assign interfaces only when set, so nil fakes stay nil interfaces.
	if f.ses != nil {
		deps.SES = f.ses
	}
	if f.sns != nil {
		deps.SNS = f.sns
	}
	if f.s3 != nil {
		deps.S3 = f.s3
	}
	if f.resolver != nil {
		deps.Resolver = f.resolver
	}
	if f.http != nil {
		deps.HTTP = f.http
	}
	return New(deps)
}

// writeBackupFile creates a (fake) backup file named for time t.
func (f *fixture) writeBackupFile(prefix string, t time.Time) string {
	f.t.Helper()
	name := backupName(prefix, t)
	if err := os.MkdirAll(f.cfg.BackupsDir(), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.BackupsDir(), name), []byte("backup "+name), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return name
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func findCheck(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %s missing from report", name)
	return Check{}
}
