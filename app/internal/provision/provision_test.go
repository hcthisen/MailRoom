package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

const (
	identityARN  = "arn:aws:ses:eu-central-1:123456789012:identity/example.com"
	configSetARN = "arn:aws:ses:eu-central-1:123456789012:configuration-set/mailroom"
	topicARN     = "arn:aws:sns:eu-central-1:123456789012:mailroom-ses-events"
	bucketName   = "mailroom-backup-123456789012-eu-central-1"
)

func sesInput(t *testing.T, envFile string) SESInput {
	t.Helper()
	return SESInput{Domain: testDomain, MailFromSubdomain: "bounce", EnvFile: envFile, BackupBucket: true}
}

func tempEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func stepActions(steps []Step) map[string]Action {
	out := map[string]Action{}
	for _, s := range steps {
		out[s.Name] = s.Action
	}
	return out
}

var allSteps = []string{
	"aws account", "ses identity", "ses mail from", "ses account suppression", "ses configuration set",
	"sns topic", "sns topic policy", "ses event destination", "ses identity default configuration set",
	"s3 bucket", "s3 public access block", "s3 encryption", "s3 versioning", "s3 lifecycle",
	"iam user", "iam policy", "iam access key", "env file",
}

func TestProvisionSESFirstRunCreatesEverything(t *testing.T) {
	f := newFakeAWS()
	envFile := tempEnv(t, "# server settings\nDOMAIN=example.com\nFROM_NAME=\"Example Co\"\n")
	var streamed []Step
	in := sesInput(t, envFile)
	in.OnStep = func(s Step) { streamed = append(streamed, s) }

	res, err := New(f.clients()).ProvisionSES(context.Background(), in)
	if err != nil {
		t.Fatalf("ProvisionSES: %v", err)
	}
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, allSteps) {
		t.Fatalf("steps = %v\nwant %v", names, allSteps)
	}
	if !reflect.DeepEqual(streamed, res.Steps) {
		t.Errorf("OnStep saw %v, result has %v", streamed, res.Steps)
	}
	for _, s := range res.Steps {
		want := ActionCreated
		switch s.Name {
		case "aws account":
			want = ActionUnchanged
		case "env file":
			// The env file existed.
			want = ActionUpdated
		}
		if s.Action != want {
			t.Errorf("step %q = %s, want %s", s.Name, s.Action, want)
		}
	}

	if res.AccountID != testAccount || res.IdentityARN != identityARN || res.ConfigurationSetARN != configSetARN ||
		res.TopicARN != topicARN || res.BackupBucket != bucketName || res.IAMUser != SenderUserName {
		t.Errorf("result names = %+v", res)
	}
	if res.AccessKeyID != "AKIATEST000000000001" {
		t.Errorf("AccessKeyID = %q", res.AccessKeyID)
	}
	if res.ProductionAccessEnabled || res.IdentityVerified || res.DKIMStatus != "PENDING" || res.MailFromStatus != "PENDING" {
		t.Errorf("status = %+v", res)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), f.secrets[res.AccessKeyID]) {
		t.Errorf("result leaks the secret key: %s", b)
	}

	// The SES side.
	id := f.identities[testDomain]
	if id.DkimAttributes.NextSigningKeyLength != sestypes.DkimSigningKeyLengthRsa2048Bit {
		t.Errorf("DKIM key length = %s", id.DkimAttributes.NextSigningKeyLength)
	}
	if aws.ToString(id.MailFromAttributes.MailFromDomain) != "bounce.example.com" || id.MailFromAttributes.BehaviorOnMxFailure != sestypes.BehaviorOnMxFailureUseDefaultValue {
		t.Errorf("mail from = %+v", id.MailFromAttributes)
	}
	if aws.ToString(id.ConfigurationSetName) != "mailroom" {
		t.Errorf("identity configuration set = %q", aws.ToString(id.ConfigurationSetName))
	}
	if !sameSet(f.account.SuppressionAttributes.SuppressedReasons, []sestypes.SuppressionListReason{"BOUNCE", "COMPLAINT"}) {
		t.Errorf("suppression = %v", f.account.SuppressionAttributes.SuppressedReasons)
	}
	dests := f.configSets["mailroom"]
	if len(dests) != 1 || aws.ToString(dests[0].Name) != EventDestinationName || !dests[0].Enabled ||
		aws.ToString(dests[0].SnsDestination.TopicArn) != topicARN ||
		!sameSet(dests[0].MatchingEventTypes, []sestypes.EventType{"BOUNCE", "COMPLAINT", "DELIVERY", "REJECT"}) {
		t.Errorf("event destinations = %+v", dests)
	}

	// The bucket.
	b := f.buckets[bucketName]
	if b == nil || b.region != testRegion {
		t.Fatalf("bucket = %+v", b)
	}
	if !fullPublicAccessBlock(b.pab) || !aes256Only(b.enc) || len(b.lifecycle) != 1 || !lifecycleRuleOK(b.lifecycle[0]) ||
		b.versioning != s3types.BucketVersioningStatusEnabled {
		t.Errorf("bucket config = pab %+v enc %+v lifecycle %+v versioning %q", b.pab, b.enc, b.lifecycle, b.versioning)
	}

	// The env file.
	env, err := ReadEnvFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DOMAIN":                "example.com",
		"FROM_NAME":             "Example Co",
		"AWS_ACCESS_KEY_ID":     "AKIATEST000000000001",
		"AWS_SECRET_ACCESS_KEY": f.secrets["AKIATEST000000000001"],
		"AWS_REGION":            testRegion,
		"SNS_TOPIC_ARN":         topicARN,
		"SES_CONFIGURATION_SET": "mailroom",
		"BACKUP_BUCKET":         bucketName,
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env file = %v\nwant %v", env, want)
	}
	raw, _ := os.ReadFile(envFile)
	if !strings.HasPrefix(string(raw), "# server settings\nDOMAIN=example.com\nFROM_NAME=\"Example Co\"\n") {
		t.Errorf("env file lost its original lines:\n%s", raw)
	}
	assertMode(t, envFile)

	// DNS records in the result.
	var types []string
	for _, r := range res.Records {
		types = append(types, r.Type+" "+r.Name)
	}
	wantRecs := []string{
		"CNAME tok1._domainkey.example.com", "CNAME tok2._domainkey.example.com", "CNAME tok3._domainkey.example.com",
		"MX bounce.example.com", "TXT bounce.example.com",
	}
	if !slices.Equal(types, wantRecs) {
		t.Errorf("records = %v", types)
	}
}

func assertMode(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != EnvFileMode {
		t.Errorf("mode of %s = %o, want 600", path, fi.Mode().Perm())
	}
}

func TestProvisionSESSecondRunIsUnchanged(t *testing.T) {
	for _, bucket := range []bool{true, false} {
		f := newFakeAWS()
		envFile := tempEnv(t, "")
		in := sesInput(t, envFile)
		in.BackupBucket = bucket
		p := New(f.clients())
		first, err := p.ProvisionSES(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if got := stepActions(first.Steps)["env file"]; got != ActionCreated {
			t.Errorf("new env file step = %s, want created", got)
		}
		assertMode(t, envFile)
		before, _ := os.ReadFile(envFile)
		f.mutations = nil

		res, err := p.ProvisionSES(context.Background(), in)
		if err != nil {
			t.Fatalf("second run: %v", err)
		}
		for _, s := range res.Steps {
			if s.Action != ActionUnchanged {
				t.Errorf("bucket=%v: second run step %q = %s", bucket, s.Name, s.Action)
			}
		}
		if len(f.mutations) != 0 {
			t.Errorf("bucket=%v: second run mutated AWS: %v", bucket, f.mutations)
		}
		after, _ := os.ReadFile(envFile)
		if string(before) != string(after) {
			t.Errorf("second run rewrote the env file:\n%s\n--\n%s", before, after)
		}
		if res.AccessKeyID != "AKIATEST000000000001" || len(f.keyIDs()) != 1 {
			t.Errorf("keys = %v, result key %q", f.keyIDs(), res.AccessKeyID)
		}
		if bucket == (res.BackupBucket == "") {
			t.Errorf("bucket=%v but result bucket %q", bucket, res.BackupBucket)
		}
	}
}

func TestProvisionSESConvergesDrift(t *testing.T) {
	f := newFakeAWS()
	envFile := tempEnv(t, "")
	p := New(f.clients())
	in := sesInput(t, envFile)
	if _, err := p.ProvisionSES(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	// Someone changed things by hand.
	id := f.identities[testDomain]
	id.MailFromAttributes.MailFromDomain = aws.String("mail.example.com")
	id.DkimAttributes.NextSigningKeyLength = sestypes.DkimSigningKeyLengthRsa1024Bit
	id.ConfigurationSetName = aws.String("other")
	f.account.SuppressionAttributes.SuppressedReasons = []sestypes.SuppressionListReason{"BOUNCE"}
	f.configSets["mailroom"][0].Enabled = false
	f.topics[topicARN]["Policy"] = `{"Version":"2012-10-17","Statement":[]}`
	f.policies[SenderUserName+"/"+SenderPolicyName] = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ses:*","Resource":"*"}]}`
	b := f.buckets[bucketName]
	b.pab.RestrictPublicBuckets = aws.Bool(false)
	b.enc.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm = s3types.ServerSideEncryptionAwsKms
	other := s3types.LifecycleRule{ID: aws.String("keep-me"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("tmp/")}, Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(1)}}
	b.lifecycle[0].Expiration.Days = aws.Int32(365)
	b.lifecycle = append(b.lifecycle, other)

	res, err := p.ProvisionSES(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	got := stepActions(res.Steps)
	for _, name := range []string{
		"ses identity", "ses mail from", "ses account suppression", "sns topic policy", "ses event destination",
		"ses identity default configuration set", "s3 public access block", "s3 encryption", "s3 lifecycle", "iam policy",
	} {
		if got[name] != ActionUpdated {
			t.Errorf("step %q = %s, want updated", name, got[name])
		}
	}
	for _, name := range []string{"ses configuration set", "sns topic", "s3 bucket", "iam user", "iam access key", "env file"} {
		if got[name] != ActionUnchanged {
			t.Errorf("step %q = %s, want unchanged", name, got[name])
		}
	}
	if aws.ToString(id.MailFromAttributes.MailFromDomain) != "bounce.example.com" {
		t.Errorf("mail from not converged")
	}
	if len(b.lifecycle) != 2 || aws.ToString(b.lifecycle[1].ID) != "keep-me" || !lifecycleRuleOK(b.lifecycle[0]) {
		t.Errorf("lifecycle = %+v, want mailroom rule fixed and other rule kept", b.lifecycle)
	}

	// And after converging, everything is stable again.
	f.mutations = nil
	if _, err := p.ProvisionSES(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 0 {
		t.Errorf("third run mutated: %v", f.mutations)
	}
}

func TestSenderPolicyDocument(t *testing.T) {
	tests := []struct {
		name   string
		bucket bool
		want   map[string][2][]string // Sid -> actions, resources
	}{
		{"with bucket", true, map[string][2][]string{
			"SendEmail":              {{"ses:SendEmail", "ses:SendRawEmail"}, {identityARN, configSetARN}},
			"ReadAccount":            {{"ses:GetAccount"}, {"*"}},
			"ReadIdentity":           {{"ses:GetEmailIdentity"}, {identityARN}},
			"ListTopicSubscriptions": {{"sns:ListSubscriptionsByTopic"}, {topicARN}},
			"SubscribeWebhook":       {{"sns:Subscribe"}, {topicARN}},
			"ListBackups":            {{"s3:ListBucket"}, {"arn:aws:s3:::" + bucketName}},
			"WriteBackups":           {{"s3:PutObject"}, {"arn:aws:s3:::" + bucketName + "/*"}},
		}},
		{"without bucket", false, map[string][2][]string{
			"SendEmail":              {{"ses:SendEmail", "ses:SendRawEmail"}, {identityARN, configSetARN}},
			"ReadAccount":            {{"ses:GetAccount"}, {"*"}},
			"ReadIdentity":           {{"ses:GetEmailIdentity"}, {identityARN}},
			"ListTopicSubscriptions": {{"sns:ListSubscriptionsByTopic"}, {topicARN}},
			"SubscribeWebhook":       {{"sns:Subscribe"}, {topicARN}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAWS()
			in := sesInput(t, tempEnv(t, ""))
			in.BackupBucket = tt.bucket
			if _, err := New(f.clients()).ProvisionSES(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			doc := f.policy(SenderUserName, SenderPolicyName)
			var raw struct {
				Version   string
				Statement []map[string]any
			}
			if err := json.Unmarshal([]byte(doc), &raw); err != nil {
				t.Fatalf("policy does not parse: %v\n%s", err, doc)
			}
			if raw.Version != "2012-10-17" {
				t.Errorf("Version = %q", raw.Version)
			}
			got := map[string][2][]string{}
			for _, s := range raw.Statement {
				if s["Effect"] != "Allow" {
					t.Errorf("statement %v is not Allow", s)
				}
				// Only Mailroom's own webhook may be subscribed: a leaked
				// sending key can't add an endpoint that keeps receiving
				// every recipient address after the key is rotated.
				cond, ok := s["Condition"]
				switch {
				case s["Sid"] == "SubscribeWebhook":
					want := map[string]any{"StringEquals": map[string]any{
						"sns:Protocol": "https", "sns:Endpoint": "https://newsletter." + testDomain + "/hooks/ses"}}
					if !reflect.DeepEqual(cond, want) {
						t.Errorf("subscribe condition = %v, want %v", cond, want)
					}
				case ok:
					t.Errorf("unexpected condition in %v", s)
				}
				got[s["Sid"].(string)] = [2][]string{sorted(asList(s["Action"])), sorted(asList(s["Resource"]))}
			}
			want := map[string][2][]string{}
			for k, v := range tt.want {
				want[k] = [2][]string{sorted(v[0]), sorted(v[1])}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("policy statements =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

func asList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, e.(string))
		}
		return out
	}
	return nil
}

func sorted(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

func TestTopicPolicyDocument(t *testing.T) {
	f := newFakeAWS()
	if _, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, ""))); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Statement []struct {
			Effect    string
			Principal map[string]any
			Action    any
			Resource  any
			Condition map[string]map[string]any
		}
	}
	if err := json.Unmarshal([]byte(f.topics[topicARN]["Policy"]), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("statements = %+v", doc.Statement)
	}
	s := doc.Statement[0]
	if s.Effect != "Allow" || s.Principal["Service"] != "ses.amazonaws.com" || len(s.Principal) != 1 {
		t.Errorf("principal = %v effect %s", s.Principal, s.Effect)
	}
	if !slices.Equal(asList(s.Action), []string{"sns:Publish"}) || !slices.Equal(asList(s.Resource), []string{topicARN}) {
		t.Errorf("action %v resource %v", s.Action, s.Resource)
	}
	want := map[string]map[string]any{"StringEquals": {"AWS:SourceAccount": testAccount, "AWS:SourceArn": configSetARN}}
	if !reflect.DeepEqual(s.Condition, want) {
		t.Errorf("condition = %v, want %v", s.Condition, want)
	}
}

func TestPolicyMatchesIgnoresFormatting(t *testing.T) {
	want := SenderPolicy("a", "b", "c", "", "https://n.example.com/hooks/ses")
	reordered := `{"Version":"2012-10-17","Statement":[
	 {"Sid":"SubscribeWebhook","Effect":"Allow","Action":"sns:Subscribe","Resource":["c"],
	  "Condition":{"StringEquals":{"sns:Endpoint":"https://n.example.com/hooks/ses","sns:Protocol":["https"]}}},
	 {"Sid":"ListTopicSubscriptions","Effect":"Allow","Action":["sns:ListSubscriptionsByTopic"],"Resource":"c"},
	 {"Sid":"ReadIdentity","Effect":"Allow","Action":["ses:GetEmailIdentity"],"Resource":"a"},
	 {"Sid":"ReadAccount","Effect":"Allow","Action":"ses:GetAccount","Resource":"*"},
	 {"Sid":"SendEmail","Effect":"Allow","Action":["ses:SendRawEmail","ses:SendEmail"],"Resource":["b","a"]}]}`
	if !policyMatches(reordered, want) {
		t.Error("equivalent policy did not match")
	}
	if policyMatches(strings.Replace(reordered, `"Resource":"*"`, `"Resource":"x"`, 1), want) {
		t.Error("different policy matched")
	}
	if policyMatches("not json", want) {
		t.Error("garbage matched")
	}
}

func TestExistingKeyNotInEnvFile(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{"no env file", ""},
		{"env file without keys", "DOMAIN=example.com\n"},
		{"unknown key id", "AWS_ACCESS_KEY_ID=AKIAOTHER\nAWS_SECRET_ACCESS_KEY=x\n"},
		{"key id without secret", "AWS_ACCESS_KEY_ID=AKIATEST000000000001\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAWS()
			p := New(f.clients())
			if _, err := p.ProvisionSES(context.Background(), sesInput(t, tempEnv(t, ""))); err != nil {
				t.Fatal(err)
			}
			envFile := tempEnv(t, tt.env)
			_, err := p.ProvisionSES(context.Background(), sesInput(t, envFile))
			if !errors.Is(err, ErrKeysNotInEnvFile) {
				t.Fatalf("err = %v, want ErrKeysNotInEnvFile", err)
			}
			if !strings.Contains(err.Error(), "--rotate-keys") || !strings.Contains(err.Error(), "AKIATEST000000000001") {
				t.Errorf("error does not tell what to do: %v", err)
			}
			if strings.Contains(err.Error(), f.secrets["AKIATEST000000000001"]) {
				t.Errorf("error leaks a secret: %v", err)
			}
		})
	}
}

func TestRotateKeys(t *testing.T) {
	f := newFakeAWS()
	envFile := tempEnv(t, "# keep me\nOTHER=1\n")
	p := New(f.clients())
	if _, err := p.ProvisionSES(context.Background(), sesInput(t, envFile)); err != nil {
		t.Fatal(err)
	}
	in := sesInput(t, envFile)
	in.RotateKeys = true
	f.mutations = nil
	res, err := p.ProvisionSES(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.keyIDs(); !slices.Equal(got, []string{"AKIATEST000000000002"}) {
		t.Errorf("keys after rotation = %v", got)
	}
	env, _ := ReadEnvFile(envFile)
	if env["AWS_ACCESS_KEY_ID"] != "AKIATEST000000000002" || env["AWS_SECRET_ACCESS_KEY"] != f.secrets["AKIATEST000000000002"] || env["OTHER"] != "1" {
		t.Errorf("env after rotation = %v", env)
	}
	raw, _ := os.ReadFile(envFile)
	if !strings.HasPrefix(string(raw), "# keep me\nOTHER=1\n") {
		t.Errorf("comment lost:\n%s", raw)
	}
	assertMode(t, envFile)
	// Order: create, then write the env file, then delete the old key.
	if !slices.Equal(f.mutations, []string{"CreateAccessKey", "DeleteAccessKey"}) {
		t.Errorf("mutations = %v", f.mutations)
	}
	got := stepActions(res.Steps)
	if got["iam access key"] != ActionUpdated || got["env file"] != ActionUpdated || got["iam old access keys"] != ActionUpdated {
		t.Errorf("steps = %+v", res.Steps)
	}
	if res.AccessKeyID != "AKIATEST000000000002" {
		t.Errorf("AccessKeyID = %q", res.AccessKeyID)
	}
}

func TestRotateKeysMakesRoomWhenTwoKeysExist(t *testing.T) {
	f := newFakeAWS()
	envFile := tempEnv(t, "")
	p := New(f.clients())
	if _, err := p.ProvisionSES(context.Background(), sesInput(t, envFile)); err != nil {
		t.Fatal(err)
	}
	// A second, unused key created by hand (the newest one).
	if _, err := f.CreateAccessKey(context.Background(), &iam.CreateAccessKeyInput{UserName: aws.String(SenderUserName)}); err != nil {
		t.Fatal(err)
	}
	in := sesInput(t, envFile)
	in.RotateKeys = true
	if _, err := p.ProvisionSES(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := f.keyIDs(); !slices.Equal(got, []string{"AKIATEST000000000003"}) {
		t.Errorf("keys = %v, want only the new key", got)
	}
	env, _ := ReadEnvFile(envFile)
	if env["AWS_ACCESS_KEY_ID"] != "AKIATEST000000000003" {
		t.Errorf("env key = %q", env["AWS_ACCESS_KEY_ID"])
	}
}

func TestEvictBeforePrefersInactiveThenOldest(t *testing.T) {
	key := func(status iamtypes.StatusType, hour int) iamtypes.AccessKeyMetadata {
		tm := time.Date(2026, 1, 1, hour, 0, 0, 0, time.UTC)
		return iamtypes.AccessKeyMetadata{Status: status, CreateDate: &tm}
	}
	active, inactive := iamtypes.StatusTypeActive, iamtypes.StatusTypeInactive
	if !evictBefore(key(inactive, 5), key(active, 1)) || evictBefore(key(active, 1), key(inactive, 5)) {
		t.Error("an inactive key should go first")
	}
	if !evictBefore(key(active, 1), key(active, 2)) || evictBefore(key(active, 2), key(active, 1)) {
		t.Error("the older key should go first")
	}
}

// failRename makes WriteEnvFile's rename fail with errno for this test.
func failRename(t *testing.T, errno syscall.Errno) {
	t.Helper()
	orig := renameFile
	renameFile = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errno}
	}
	t.Cleanup(func() { renameFile = orig })
}

func TestEnvWriteFailureDeletesNewKey(t *testing.T) {
	f := newFakeAWS()
	// The env file reads fine; only replacing it fails, after the key exists.
	envFile := tempEnv(t, "DOMAIN=example.com\n")
	failRename(t, syscall.EACCES)
	_, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, envFile))
	if err == nil || !strings.Contains(err.Error(), "replace env file") {
		t.Fatalf("err = %v, want the env file write error", err)
	}
	create := slices.Index(f.mutations, "CreateAccessKey")
	del := slices.Index(f.mutations, "DeleteAccessKey")
	if create < 0 || del < create {
		t.Fatalf("mutations = %v, want CreateAccessKey followed by DeleteAccessKey", f.mutations)
	}
	if got := f.keyIDs(); len(got) != 0 {
		t.Errorf("keys after failed write = %v, want none", got)
	}
	if raw, _ := os.ReadFile(envFile); string(raw) != "DOMAIN=example.com\n" {
		t.Errorf("env file changed: %q", raw)
	}
}

func TestEnvWriteFailureOnRotateKeepsOldKey(t *testing.T) {
	f := newFakeAWS()
	envFile := tempEnv(t, "")
	p := New(f.clients())
	if _, err := p.ProvisionSES(context.Background(), sesInput(t, envFile)); err != nil {
		t.Fatal(err)
	}
	failRename(t, syscall.EACCES)
	in := sesInput(t, envFile)
	in.RotateKeys = true
	if _, err := p.ProvisionSES(context.Background(), in); err == nil {
		t.Fatal("want the env file write error")
	}
	if got := f.keyIDs(); !slices.Equal(got, []string{"AKIATEST000000000001"}) {
		t.Errorf("keys = %v, want only the old key, still in the env file", got)
	}
	if env, _ := ReadEnvFile(envFile); env["AWS_ACCESS_KEY_ID"] != "AKIATEST000000000001" {
		t.Errorf("env key = %q", env["AWS_ACCESS_KEY_ID"])
	}
}

// guideSetupPolicy reads the setup-agent inline policy from
// guide/steps.md step 2.6 (the JSON block after its heading), so the test
// checks the policy the customer actually pastes.
func guideSetupPolicy(t *testing.T) PolicyDocument {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "guide", "steps.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "**2.6 ")
	if i < 0 {
		t.Fatal("guide/steps.md has no step 2.6")
	}
	s = s[i:]
	start := strings.Index(s, "```json")
	if start < 0 {
		t.Fatal("step 2.6 has no JSON block")
	}
	s = s[start+len("```json"):]
	end := strings.Index(s, "```")
	if end < 0 {
		t.Fatal("step 2.6: unterminated JSON block")
	}
	var doc PolicyDocument
	if err := json.Unmarshal([]byte(s[:end]), &doc); err != nil {
		t.Fatalf("step 2.6 policy: %v", err)
	}
	return doc
}

// TestGuideSetupPolicyScope: the step 2.6 policy grants IAM actions on the
// sending user only and S3 actions on the backup buckets only.
func TestGuideSetupPolicyScope(t *testing.T) {
	for _, st := range guideSetupPolicy(t).Statement {
		for _, a := range st.Action {
			var want string
			switch {
			case strings.HasPrefix(a, "iam:"):
				want = "arn:aws:iam::*:user/" + SenderUserName
			case strings.HasPrefix(a, "s3:"):
				want = "arn:aws:s3:::mailroom-backup-*"
			default:
				t.Errorf("unexpected action %s", a)
				continue
			}
			if !slices.Equal([]string(st.Resource), []string{want}) {
				t.Errorf("%s: resource %v, want %s", a, st.Resource, want)
			}
		}
	}
}

// TestProvisionSESWithGuideSetupPolicy runs against a fake that denies
// every S3 and IAM action guide/steps.md step 2.6 does not grant, and
// checks that each action the guide grants is used.
func TestProvisionSESWithGuideSetupPolicy(t *testing.T) {
	var granted []string
	for _, st := range guideSetupPolicy(t).Statement {
		granted = append(granted, st.Action...)
	}
	reads := []string{"s3:GetBucketPublicAccessBlock", "s3:GetEncryptionConfiguration", "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"}
	withoutReads := slices.DeleteFunc(slices.Clone(granted), func(a string) bool { return slices.Contains(reads, a) })
	if len(withoutReads) != len(granted)-len(reads) {
		t.Fatalf("the guide policy lacks some of %v: %v", reads, granted)
	}
	tests := []struct {
		name       string
		allowed    []string
		secondRun  Action // of the three bucket-setting steps
		wantDenied []string
	}{
		{"guide policy", granted, ActionUnchanged, nil},
		{"guide policy without the read actions", withoutReads, ActionUpdated, reads},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAWS()
			f.allow(tt.allowed...)
			envFile := tempEnv(t, "")
			p := New(f.clients())
			first, err := p.ProvisionSES(context.Background(), sesInput(t, envFile))
			if err != nil {
				t.Fatalf("first run: %v (denied %v)", err, f.denied)
			}
			if len(f.denied) != 0 {
				t.Errorf("first run needed actions the guide does not grant: %v", f.denied)
			}
			for _, name := range []string{"s3 bucket", "s3 public access block", "s3 encryption", "s3 versioning", "s3 lifecycle", "iam user", "iam access key"} {
				if got := stepActions(first.Steps)[name]; got != ActionCreated {
					t.Errorf("first run step %q = %s, want created", name, got)
				}
			}
			b := f.buckets[bucketName]
			if !fullPublicAccessBlock(b.pab) || !aes256Only(b.enc) || len(b.lifecycle) != 1 || !lifecycleRuleOK(b.lifecycle[0]) {
				t.Errorf("bucket config = pab %+v enc %+v lifecycle %+v", b.pab, b.enc, b.lifecycle)
			}

			res, err := p.ProvisionSES(context.Background(), sesInput(t, envFile))
			if err != nil {
				t.Fatalf("second run: %v", err)
			}
			if !sameSet(f.denied, tt.wantDenied) {
				t.Errorf("denied = %v, want %v", f.denied, tt.wantDenied)
			}
			for _, s := range res.Steps {
				want := ActionUnchanged
				switch s.Name {
				case "s3 public access block", "s3 encryption", "s3 versioning", "s3 lifecycle":
					want = tt.secondRun
					if blind := strings.Contains(s.Detail, "without comparing"); blind != (want == ActionUpdated) {
						t.Errorf("step %q detail %q", s.Name, s.Detail)
					}
				}
				if s.Action != want {
					t.Errorf("second run step %q = %s, want %s", s.Name, s.Action, want)
				}
			}
			if !lifecycleRuleOK(b.lifecycle[0]) || !fullPublicAccessBlock(b.pab) || !aes256Only(b.enc) {
				t.Error("bucket settings lost on the second run")
			}
			if tt.wantDenied != nil {
				return
			}
			// A key rotation needs the rest; then every action the guide
			// grants has been used (the policy asks for nothing extra).
			in := sesInput(t, envFile)
			in.RotateKeys = true
			if _, err := p.ProvisionSES(context.Background(), in); err != nil {
				t.Fatalf("rotate run: %v (denied %v)", err, f.denied)
			}
			for _, a := range tt.allowed {
				if !f.used[a] {
					t.Errorf("the guide grants %s, which provisioning never uses", a)
				}
			}
		})
	}
}

func TestBucketReadErrorOtherThanAccessDeniedStops(t *testing.T) {
	f := newFakeAWS()
	envFile := tempEnv(t, "")
	p := New(f.clients())
	if _, err := p.ProvisionSES(context.Background(), sesInput(t, envFile)); err != nil {
		t.Fatal(err)
	}
	f.failOn["GetBucketEncryption"] = apiErr("InternalError")
	_, err := p.ProvisionSES(context.Background(), sesInput(t, envFile))
	if err == nil || !strings.Contains(err.Error(), "s3 get-bucket-encryption") {
		t.Fatalf("err = %v", err)
	}
}

func TestProvisionSESWarnings(t *testing.T) {
	tests := []struct {
		name       string
		production bool
		sending    bool
		want       []string
	}{
		{"sandbox", false, true, []string{"SES sandbox"}},
		{"production", true, true, nil},
		{"sending disabled", true, false, []string{"sending is disabled"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAWS()
			f.account.ProductionAccessEnabled = tt.production
			f.account.SendingEnabled = tt.sending
			res, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, "")))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Warnings) != len(tt.want) {
				t.Fatalf("warnings = %q, want %q", res.Warnings, tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(res.Warnings[i], w) {
					t.Errorf("warning %q lacks %q", res.Warnings[i], w)
				}
			}
			if tt.name == "sandbox" && !strings.Contains(res.Warnings[0], "mailbox simulator") {
				t.Errorf("sandbox warning = %q", res.Warnings[0])
			}
		})
	}
}

func TestProvisionSESStopsOnError(t *testing.T) {
	f := newFakeAWS()
	f.failOn["CreateTopic"] = apiErr("AuthorizationError")
	res, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, "")))
	if err == nil || !strings.Contains(err.Error(), "sns create-topic") {
		t.Fatalf("err = %v", err)
	}
	if n := len(res.Steps); n == 0 || res.Steps[n-1].Name != "ses configuration set" {
		t.Errorf("partial steps = %+v", res.Steps)
	}
	if slices.Contains(f.calls, "CreateUser") {
		t.Error("continued after the error")
	}
}

func TestProvisionSESValidation(t *testing.T) {
	tests := []struct {
		name string
		in   SESInput
		want string
	}{
		{"no domain", SESInput{MailFromSubdomain: "bounce"}, "DOMAIN is required"},
		{"bad domain", SESInput{Domain: "exa mple.com", MailFromSubdomain: "bounce"}, "not a valid domain"},
		{"single label", SESInput{Domain: "localhost", MailFromSubdomain: "bounce"}, "not a valid domain"},
		{"bad mail from", SESInput{Domain: testDomain, MailFromSubdomain: "-x"}, "MAIL_FROM_SUBDOMAIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAWS()
			_, err := New(f.clients()).ProvisionSES(context.Background(), tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if len(f.calls) != 0 {
				t.Errorf("called AWS: %v", f.calls)
			}
		})
	}
}

func TestBucketInUSEast1HasNoLocationConstraint(t *testing.T) {
	f := newFakeAWS()
	f.region = "us-east-1"
	res, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupBucket != "mailroom-backup-123456789012-us-east-1" || f.buckets[res.BackupBucket].region != "us-east-1" {
		t.Errorf("bucket = %q", res.BackupBucket)
	}
}

func TestBucketOwnedByAnotherAccount(t *testing.T) {
	f := newFakeAWS()
	f.failOn["HeadBucket"] = apiErr("Forbidden")
	_, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, "")))
	if err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("err = %v", err)
	}
}

// TestProvisionSESChecksEnvFileFirst: an env file the process can't write
// (the container user doesn't own it) or a directory where the file should
// be stops the run before any AWS call, with a hint on how to fix it.
func TestProvisionSESChecksEnvFileFirst(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs POSIX permissions enforced for this user")
	}
	readOnly := tempEnv(t, "DOMAIN=example.com\n")
	if err := os.Chmod(readOnly, 0o400); err != nil {
		t.Fatal(err)
	}
	dirEnv := filepath.Join(t.TempDir(), ".env")
	if err := os.Mkdir(dirEnv, 0o700); err != nil {
		t.Fatal(err)
	}
	lockedDir := t.TempDir()
	if err := os.Chmod(lockedDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o700) })
	for name, tc := range map[string]struct{ path, hint string }{
		"not writable":        {readOnly, "chown 65532:65532"},
		"directory":           {dirEnv, "is a directory"},
		"missing, locked dir": {filepath.Join(lockedDir, ".env"), "create the file on the host"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAWS()
			_, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tc.path))
			if err == nil || !strings.Contains(err.Error(), tc.hint) {
				t.Fatalf("err = %v, want a hint containing %q", err, tc.hint)
			}
			if len(f.calls) != 0 {
				t.Fatalf("AWS was called: %v", f.calls)
			}
		})
	}
	// A missing file in a writable (or not yet existing) directory is fine.
	for _, p := range []string{filepath.Join(t.TempDir(), ".env"), filepath.Join(t.TempDir(), "new", ".env")} {
		if err := CheckEnvFileWritable(p); err != nil {
			t.Fatalf("CheckEnvFileWritable(%s) = %v", p, err)
		}
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("the check created %s", p)
		}
	}
	if entries, _ := os.ReadDir(lockedDir); len(entries) != 0 {
		t.Fatalf("probe files left: %v", entries)
	}
}

// TestBucketVersioningWithOldSetupPolicy: a setup user created from an
// older guide (no s3:*BucketVersioning) still provisions, e.g. for a key
// rotation after a leak, and gets a warning saying what to add.
func TestBucketVersioningWithOldSetupPolicy(t *testing.T) {
	var granted []string
	for _, st := range guideSetupPolicy(t).Statement {
		for _, a := range st.Action {
			if a != "s3:GetBucketVersioning" && a != "s3:PutBucketVersioning" {
				granted = append(granted, a)
			}
		}
	}
	f := newFakeAWS()
	f.allow(granted...)
	res, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, "")))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if f.buckets[bucketName].versioning != "" {
		t.Fatal("versioning was turned on without the permission")
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "s3:PutBucketVersioning") }) {
		t.Fatalf("warnings = %q", res.Warnings)
	}
}

func TestWebhookURL(t *testing.T) {
	for _, tt := range []struct{ base, sub, domain, want string }{
		{"", "newsletter", "example.com", "https://newsletter.example.com/hooks/ses"},
		{"https://Mail.Example.com/", "newsletter", "example.com", "https://mail.example.com/hooks/ses"},
		{"http://127.0.0.1:8080", "x", "example.com", "http://127.0.0.1:8080/hooks/ses"},
	} {
		got := WebhookURL(tt.base, tt.sub, tt.domain)
		if got != tt.want {
			t.Errorf("WebhookURL(%q, %q, %q) = %q, want %q", tt.base, tt.sub, tt.domain, got, tt.want)
		}
		if proto := webhookProtocol(got); proto != strings.SplitN(tt.want, ":", 2)[0] {
			t.Errorf("protocol of %s = %s", got, proto)
		}
	}
}
