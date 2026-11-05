package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type cliRun struct {
	code           int
	stdout, stderr string
}

type cliEnv struct {
	t    *testing.T
	aws  *fakeAWS
	cf   *fakeCloudflare
	env  map[string]string
	file string // env file
	cli  *CLI
}

func newCLIEnv(t *testing.T) *cliEnv {
	e := &cliEnv{
		t:   t,
		aws: newFakeAWS(),
		cf:  newFakeCloudflare(t),
		env: map[string]string{
			"DOMAIN":                "example.com",
			"OWNER_EMAIL":           "owner@example.com",
			"AWS_REGION":            testRegion,
			"AWS_ACCESS_KEY_ID":     "AKIASETUPKEY",
			"AWS_SECRET_ACCESS_KEY": "setup-secret-value",
			"CLOUDFLARE_API_TOKEN":  testCFToken,
		},
		file: filepath.Join(t.TempDir(), ".env"),
	}
	e.cli = &CLI{
		NewClients: func(_ context.Context, getenv func(string) string) (Clients, error) {
			if getenv("AWS_REGION") == "" {
				return Clients{}, errors.New("AWS_REGION is required")
			}
			return e.aws.clients(), nil
		},
		CloudflareBaseURL: e.cf.srv.URL + "/client/v4",
		HTTPClient:        e.cf.srv.Client(),
	}
	return e
}

func (e *cliEnv) provision(args ...string) cliRun {
	var out, errb bytes.Buffer
	code := e.cli.Provision(context.Background(), args, &out, &errb, func(k string) string { return e.env[k] })
	return cliRun{code, out.String(), errb.String()}
}

func (e *cliEnv) dns(args ...string) cliRun {
	var out, errb bytes.Buffer
	code := e.cli.DNS(context.Background(), args, &out, &errb, func(k string) string { return e.env[k] })
	return cliRun{code, out.String(), errb.String()}
}

func (e *cliEnv) assertNoSecrets(r cliRun) {
	e.t.Helper()
	all := r.stdout + r.stderr
	secrets := []string{"setup-secret-value", testCFToken}
	for _, s := range e.aws.secrets {
		secrets = append(secrets, s)
	}
	for _, s := range secrets {
		if strings.Contains(all, s) {
			e.t.Errorf("output contains the secret %q:\n%s", s, all)
		}
	}
}

func TestProvisionSESCommandHuman(t *testing.T) {
	e := newCLIEnv(t)
	r := e.provision("ses", "--env-file", e.file)
	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	e.assertNoSecrets(r)
	for _, want := range []string{
		"Provisioning SES for example.com in eu-central-1",
		"created    ses identity",
		"created    iam access key",
		"AKIATEST000000000001",
		"tok1._domainkey.example.com",
		"feedback-smtp.eu-central-1.amazonses.com",
		"SES sandbox",
		"--request-production",
		"Warning: the account is in the SES sandbox",
		"mailbox simulator",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	env, _ := ReadEnvFile(e.file)
	if env["AWS_ACCESS_KEY_ID"] != "AKIATEST000000000001" || env["AWS_ACCESS_KEY_ID"] == e.env["AWS_ACCESS_KEY_ID"] {
		t.Errorf("env file key = %q; the setup key must never be written", env["AWS_ACCESS_KEY_ID"])
	}

	r = e.provision("ses", "--env-file", e.file)
	if r.code != ExitOK || strings.Contains(r.stdout, "created ") || strings.Contains(r.stdout, "updated ") {
		t.Errorf("second run exit %d:\n%s", r.code, r.stdout)
	}
}

func TestProvisionSESCommandJSON(t *testing.T) {
	e := newCLIEnv(t)
	r := e.provision("ses", "--env-file", e.file, "--no-backup-bucket", "--no-receive", "--json")
	if r.code != ExitOK || r.stderr != "" {
		t.Fatalf("exit %d stderr %s", r.code, r.stderr)
	}
	e.assertNoSecrets(r)
	var res struct {
		Result
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if res.Error != "" || res.AccessKeyID != "AKIATEST000000000001" || res.TopicARN != topicARN || res.BackupBucket != "" || len(res.Records) != 5 || len(res.Steps) == 0 {
		t.Errorf("result = %+v", res)
	}
	if len(e.aws.buckets) != 0 || slices.Contains(e.aws.calls, "HeadBucket") {
		t.Error("--no-backup-bucket still touched S3")
	}
	env, _ := ReadEnvFile(e.file)
	if v, ok := env["BACKUP_BUCKET"]; !ok || v != "" {
		t.Errorf("BACKUP_BUCKET = %q, %v", v, ok)
	}
}

func TestProvisionSESCommandKeysNotInEnvFile(t *testing.T) {
	e := newCLIEnv(t)
	if r := e.provision("ses", "--env-file", e.file); r.code != ExitOK {
		t.Fatal(r.stderr)
	}
	other := filepath.Join(t.TempDir(), ".env")
	r := e.provision("ses", "--env-file", other, "--json")
	if r.code != ExitError || !strings.Contains(r.stderr, "--rotate-keys") {
		t.Fatalf("exit %d stderr %s", r.code, r.stderr)
	}
	var out struct {
		Error string `json:"error"`
		Steps []Step `json:"steps"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || !strings.Contains(out.Error, "--rotate-keys") || len(out.Steps) == 0 {
		t.Errorf("json = %s (%v)", r.stdout, err)
	}

	r = e.provision("ses", "--env-file", other, "--rotate-keys")
	if r.code != ExitOK {
		t.Fatalf("rotate: exit %d %s", r.code, r.stderr)
	}
	e.assertNoSecrets(r)
	if got := e.aws.keyIDs(); !slices.Equal(got, []string{"AKIATEST000000000002"}) {
		t.Errorf("keys = %v", got)
	}
	if !strings.Contains(r.stdout, "deleted AKIATEST000000000001") {
		t.Errorf("rotation not reported:\n%s", r.stdout)
	}
}

func TestProvisionSESCommandNeedsDomain(t *testing.T) {
	e := newCLIEnv(t)
	delete(e.env, "DOMAIN")
	r := e.provision("ses", "--env-file", e.file)
	if r.code != ExitError || !strings.Contains(r.stderr, "DOMAIN is required") || r.stdout != "" {
		t.Errorf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if len(e.aws.calls) != 0 {
		t.Errorf("called AWS: %v", e.aws.calls)
	}
}

func TestProvisionSESCommandAWSConfigError(t *testing.T) {
	e := newCLIEnv(t)
	delete(e.env, "AWS_REGION")
	r := e.provision("ses", "--env-file", e.file)
	if r.code != ExitError || !strings.Contains(r.stderr, "AWS_REGION") {
		t.Errorf("exit %d stderr %s", r.code, r.stderr)
	}
}

func TestRequestProductionCommand(t *testing.T) {
	e := newCLIEnv(t)
	file := filepath.Join(t.TempDir(), "use-case.txt")
	if err := os.WriteFile(file, []byte(goodUseCase), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.provision("ses", "--request-production", "--use-case", file)
	if r.code != ExitOK || !strings.Contains(r.stdout, "submitted") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if e.aws.details == nil || e.aws.details.AdditionalContactEmailAddresses[0] != "owner@example.com" {
		t.Errorf("details = %+v", e.aws.details)
	}
	if slices.Contains(e.aws.calls, "CreateEmailIdentity") {
		t.Error("--request-production also provisioned")
	}

	e.aws.account.ProductionAccessEnabled = true
	r = e.provision("ses", "--request-production", "--use-case", file, "--json")
	var out ProductionResult
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Submitted || !out.ProductionAccessEnabled {
		t.Errorf("already enabled: exit %d %s", r.code, r.stdout)
	}
}

func TestRequestProductionCommandErrors(t *testing.T) {
	short := filepath.Join(t.TempDir(), "short.txt")
	if err := os.WriteFile(short, []byte("We send emails."), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no use-case flag", []string{"ses", "--request-production"}, "--use-case"},
		{"missing file", []string{"ses", "--request-production", "--use-case", filepath.Join(t.TempDir(), "none")}, "use-case file"},
		{"short text", []string{"ses", "--request-production", "--use-case", short}, "too short"},
		{"use-case alone", []string{"ses", "--use-case", short}, "only used with --request-production"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCLIEnv(t)
			r := e.provision(tt.args...)
			if r.code != ExitError || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit %d stderr %q, want %q", r.code, r.stderr, tt.want)
			}
			if len(e.aws.calls) != 0 {
				t.Errorf("called AWS: %v", e.aws.calls)
			}
		})
	}
}

// provisioned returns an env whose SES identity exists.
func provisioned(t *testing.T) *cliEnv {
	e := newCLIEnv(t)
	if r := e.provision("ses", "--env-file", e.file, "--no-backup-bucket"); r.code != ExitOK {
		t.Fatal(r.stderr)
	}
	return e
}

func (e *cliEnv) cfMethods() []string {
	e.cf.mu.Lock()
	defer e.cf.mu.Unlock()
	return slices.Clone(e.cf.methods)
}

func TestProvisionDNSCommandDryRun(t *testing.T) {
	e := provisioned(t)
	r := e.provision("dns", "--cloudflare", "--ipv4", "203.0.113.7")
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	e.assertNoSecrets(r)
	if slices.Contains(e.cfMethods(), http.MethodPost) {
		t.Errorf("dry run created records: %v", e.cf.posts)
	}
	for _, want := range []string{"To create (6)", "+ A newsletter.example.com 203.0.113.7", "+ MX bounce.example.com 10 feedback-smtp", "Run again with --yes", "never changed or deleted"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestProvisionDNSCommandApply(t *testing.T) {
	e := provisioned(t)
	e.cf.records["zone-1"] = []DNSRecord{
		{ID: "old", Type: "TXT", Name: "_dmarc.example.com", Content: "v=DMARC1; p=quarantine"},
	}
	r := e.provision("dns", "--cloudflare", "--ipv4", "203.0.113.7", "--dmarc", "--yes", "--json")
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	var out dnsOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Applied || len(out.Created) != 6 || len(out.Plan.Unchanged) != 1 || out.Zone.ID != "zone-1" {
		t.Errorf("out = %+v", out)
	}
	if len(e.cf.records["zone-1"]) != 7 || e.cf.records["zone-1"][0].Content != "v=DMARC1; p=quarantine" {
		t.Errorf("zone = %+v", e.cf.records["zone-1"])
	}

	// Applying again creates nothing.
	before := len(e.cf.posts)
	r = e.provision("dns", "--cloudflare", "--ipv4", "203.0.113.7", "--dmarc", "--yes")
	if r.code != ExitOK || len(e.cf.posts) != before || !strings.Contains(r.stdout, "Already present (7)") {
		t.Errorf("second apply: exit %d posts %d\n%s", r.code, len(e.cf.posts)-before, r.stdout)
	}
	for _, m := range e.cfMethods() {
		if m != http.MethodGet && m != http.MethodPost {
			t.Errorf("method %s used", m)
		}
	}
}

func TestProvisionDNSCommandConflicts(t *testing.T) {
	e := provisioned(t)
	e.cf.records["zone-1"] = []DNSRecord{
		{ID: "a", Type: "A", Name: "newsletter.example.com", Content: "198.51.100.9"},
		{ID: "spf", Type: "TXT", Name: "bounce.example.com", Content: "v=spf1 -all"},
	}
	r := e.provision("dns", "--cloudflare", "--ipv4", "203.0.113.7", "--yes")
	if r.code != ExitAttention {
		t.Fatalf("exit %d, want %d\n%s\n%s", r.code, ExitAttention, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "Conflicts, not changed (2)") || !strings.Contains(r.stdout, "198.51.100.9") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	if len(e.cf.posts) != 4 {
		t.Errorf("posts = %+v, want the 4 missing records only", e.cf.posts)
	}
	if e.cf.records["zone-1"][0].Content != "198.51.100.9" || e.cf.records["zone-1"][1].Content != "v=spf1 -all" {
		t.Error("existing records changed")
	}
}

func TestProvisionDNSCommandErrors(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*cliEnv)
		args []string
		want string
	}{
		{"no provider", nil, []string{"dns"}, "--cloudflare"},
		{"no token", func(e *cliEnv) { delete(e.env, "CLOUDFLARE_API_TOKEN") }, []string{"dns", "--cloudflare"}, "CLOUDFLARE_API_TOKEN"},
		{"bad ip", nil, []string{"dns", "--cloudflare", "--ipv4", "::1"}, "--ipv4"},
		{"no zone", func(e *cliEnv) { e.cf.zones = nil }, []string{"dns", "--cloudflare"}, "no zone"},
		{"no identity", func(e *cliEnv) { delete(e.aws.identities, testDomain) }, []string{"dns", "--cloudflare"}, "run mailroom provision ses first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := provisioned(t)
			if tt.mod != nil {
				tt.mod(e)
			}
			r := e.provision(tt.args...)
			if r.code != ExitError || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit %d stderr %q, want %q", r.code, r.stderr, tt.want)
			}
			if slices.Contains(e.cfMethods(), http.MethodPost) {
				t.Error("created records despite the error")
			}
		})
	}
}

func TestDNSRecordsCommand(t *testing.T) {
	e := provisioned(t)
	r := e.dns("records", "--ipv4", "203.0.113.7", "--ipv6", "2001:db8::1", "--dmarc")
	if r.code != ExitOK {
		t.Fatalf("exit %d %s", r.code, r.stderr)
	}
	for _, want := range []string{"TYPE", "newsletter.example.com", "203.0.113.7", "2001:db8::1", "tok3._domainkey.example.com", "v=DMARC1; p=none; rua=mailto:owner@example.com"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if len(e.cfMethods()) != 0 {
		t.Error("dns records called Cloudflare")
	}

	r = e.dns("records", "--json")
	var out struct {
		Records []Record `json:"records"`
	}
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &out) != nil || len(out.Records) != 5 {
		t.Errorf("json: exit %d %s", r.code, r.stdout)
	}

	r = e.dns("records")
	if !strings.Contains(r.stdout, "No --ipv4/--ipv6 given") {
		t.Errorf("missing A-record hint:\n%s", r.stdout)
	}
}

func TestCommandUsage(t *testing.T) {
	e := newCLIEnv(t)
	tests := []struct {
		name string
		run  func() cliRun
		code int
		out  string // substring of stdout+stderr
	}{
		{"provision no args", func() cliRun { return e.provision() }, ExitError, "usage:"},
		{"provision help", func() cliRun { return e.provision("help") }, ExitOK, "provision dns --cloudflare"},
		{"provision unknown", func() cliRun { return e.provision("route53") }, ExitError, "unknown provision target"},
		{"provision bad flag", func() cliRun { return e.provision("ses", "--nope") }, ExitError, "flag provided but not defined"},
		{"provision -h", func() cliRun { return e.provision("ses", "-h") }, ExitOK, "-rotate-keys"},
		{"provision extra arg", func() cliRun { return e.provision("ses", "extra") }, ExitError, "unexpected argument"},
		{"dns no args", func() cliRun { return e.dns() }, ExitError, "dns records"},
		{"dns unknown", func() cliRun { return e.dns("zone") }, ExitError, "unknown dns command"},
		{"dns help", func() cliRun { return e.dns("--help") }, ExitOK, "dns records"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.run()
			if r.code != tt.code || !strings.Contains(r.stdout+r.stderr, tt.out) {
				t.Errorf("exit %d, output %q; want %d and %q", r.code, r.stdout+r.stderr, tt.code, tt.out)
			}
		})
	}
	if len(e.aws.calls) != 0 {
		t.Errorf("usage errors called AWS: %v", e.aws.calls)
	}
}

// TestJSONModeErrors checks that with --json every failure also puts a
// JSON document with an error on stdout, for callers that parse stdout.
func TestJSONModeErrors(t *testing.T) {
	useCase := filepath.Join(t.TempDir(), "use-case.txt")
	if err := os.WriteFile(useCase, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		mod  func(*cliEnv)
		dns  bool // run "dns ..." instead of "provision ..."
		args []string
		want string // substring of the JSON error
	}{
		{"ses no domain", func(e *cliEnv) { delete(e.env, "DOMAIN") }, false, []string{"ses", "--json"}, "DOMAIN is required"},
		{"ses aws config", func(e *cliEnv) { delete(e.env, "AWS_REGION") }, false, []string{"ses", "--json"}, "AWS_REGION"},
		{"ses bad flag", nil, false, []string{"ses", "--json", "--nope"}, "flag provided but not defined"},
		{"ses bad flag before json", nil, false, []string{"ses", "--nope", "-json=true"}, "flag provided but not defined"},
		{"ses extra argument", nil, false, []string{"ses", "--json", "extra"}, "unexpected argument"},
		{"ses use-case without request", nil, false, []string{"ses", "--json", "--use-case", useCase}, "only used with --request-production"},
		{"production no file", nil, false, []string{"ses", "--request-production", "--json"}, "--use-case FILE"},
		{"production short text", nil, false, []string{"ses", "--request-production", "--use-case", useCase, "--json"}, "characters"},
		{"unknown target", nil, false, []string{"route53", "--json"}, "unknown provision target"},
		{"dns no provider", nil, false, []string{"dns", "--json"}, "--cloudflare"},
		{"dns no token", func(e *cliEnv) { delete(e.env, "CLOUDFLARE_API_TOKEN") }, false, []string{"dns", "--cloudflare", "--json"}, "CLOUDFLARE_API_TOKEN"},
		{"dns bad ip", nil, false, []string{"dns", "--cloudflare", "--ipv4", "::1", "--json"}, "--ipv4"},
		{"dns no zone", func(e *cliEnv) { e.cf.zones = nil }, false, []string{"dns", "--cloudflare", "--json"}, "no zone"},
		{"dns no identity", func(e *cliEnv) { delete(e.aws.identities, testDomain) }, false, []string{"dns", "--cloudflare", "--json"}, "provision ses first"},
		{"records no identity", func(e *cliEnv) { delete(e.aws.identities, testDomain) }, true, []string{"records", "--json"}, "provision ses first"},
		{"records bad ip", nil, true, []string{"records", "--ipv6", "203.0.113.7", "--json"}, "--ipv6"},
		{"records unknown", nil, true, []string{"zone", "--json"}, "unknown dns command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := provisioned(t)
			if tt.mod != nil {
				tt.mod(e)
			}
			var r cliRun
			if tt.dns {
				r = e.dns(tt.args...)
			} else {
				r = e.provision(tt.args...)
			}
			if r.code != ExitError {
				t.Fatalf("exit %d, want %d\n%s\n%s", r.code, ExitError, r.stdout, r.stderr)
			}
			var out errorOutput
			if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
				t.Fatalf("stdout is not one JSON document: %v\n%q", err, r.stdout)
			}
			if !strings.Contains(out.Error, tt.want) {
				t.Errorf("error = %q, want %q", out.Error, tt.want)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr = %q, want %q too", r.stderr, tt.want)
			}
			e.assertNoSecrets(r)
		})
	}

	// Without --json (or with --json=false) stdout stays empty.
	e := provisioned(t)
	delete(e.env, "DOMAIN")
	for _, args := range [][]string{{"ses"}, {"ses", "--json=false"}} {
		if r := e.provision(args...); r.code != ExitError || r.stdout != "" {
			t.Errorf("%v: exit %d stdout %q", args, r.code, r.stdout)
		}
	}
}

func TestWantsJSON(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--json"}, true},
		{[]string{"-json"}, true},
		{[]string{"--json=true"}, true},
		{[]string{"--json=false"}, false},
		{[]string{"--json", "--json=false"}, false},
		{[]string{"--env-file", "json"}, false},
		{[]string{"--", "--json"}, false},
		{[]string{"--jsonx"}, false},
	}
	for _, tt := range tests {
		if got := wantsJSON(tt.args); got != tt.want {
			t.Errorf("wantsJSON(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}
