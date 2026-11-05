package provision

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestProvisionSESReceiving(t *testing.T) {
	f := newFakeAWS()
	envFile := tempEnv(t, "")
	res, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, envFile))
	if err != nil {
		t.Fatal(err)
	}
	if res.InboundDomain != testDomain || res.InboundBucket != inboundBucketName {
		t.Errorf("inbound = %q %q", res.InboundDomain, res.InboundBucket)
	}
	if f.activeRuleSet != ReceiptRuleSetName {
		t.Errorf("active rule set = %q", f.activeRuleSet)
	}
	rules := f.ruleSets[ReceiptRuleSetName]
	if len(rules) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	rule := rules[0]
	a := rule.Actions[0].S3Action
	if !rule.Enabled || !rule.ScanEnabled || !slices.Equal(rule.Recipients, []string{testDomain}) ||
		aws.ToString(a.BucketName) != inboundBucketName || aws.ToString(a.ObjectKeyPrefix) != InboundPrefix || aws.ToString(a.TopicArn) != topicARN {
		t.Errorf("rule = %+v action %+v", rule, a)
	}
	b := f.buckets[inboundBucketName]
	if b == nil || !fullPublicAccessBlock(b.pab) || !aes256Only(b.enc) || len(b.lifecycle) != 1 || b.versioning != "" {
		t.Fatalf("inbound bucket = %+v", b)
	}
	if l := b.lifecycle[0]; aws.ToString(l.Filter.Prefix) != InboundPrefix || aws.ToInt32(l.Expiration.Days) != InboundRetentionDays {
		t.Errorf("lifecycle = %+v", l)
	}
	var pol PolicyDocument
	if err := json.Unmarshal([]byte(b.policy), &pol); err != nil {
		t.Fatal(err)
	}
	s := pol.Statement[0]
	if s.Principal["Service"][0] != "ses.amazonaws.com" || s.Action[0] != "s3:PutObject" ||
		s.Resource[0] != "arn:aws:s3:::"+inboundBucketName+"/inbound/*" ||
		s.Condition["StringEquals"]["AWS:SourceArn"][0] != receiptRuleARN {
		t.Errorf("bucket policy = %s", b.policy)
	}
	env, _ := ReadEnvFile(envFile)
	if env["INBOUND_BUCKET"] != inboundBucketName || env["INBOUND_DOMAIN"] != testDomain {
		t.Errorf("env = %v", env)
	}
	if !slices.ContainsFunc(res.Records, func(r Record) bool {
		return r.Type == "MX" && r.Name == testDomain && r.Value == "inbound-smtp.eu-central-1.amazonaws.com" && r.Priority == 10
	}) {
		t.Errorf("records lack the inbound MX: %+v", res.Records)
	}

	// Drift in the rule is repaired; everything else is unchanged.
	f.ruleSets[ReceiptRuleSetName][0].ScanEnabled = false
	f.mutations = nil
	res, err = New(f.clients()).ProvisionSES(context.Background(), sesInput(t, envFile))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.mutations, []string{"UpdateReceiptRule"}) {
		t.Errorf("mutations = %v", f.mutations)
	}
	if got := stepActions(res.Steps)["ses receipt rule"]; got != ActionUpdated {
		t.Errorf("rule step = %s", got)
	}
}

func TestProvisionSESReceivingOff(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *fakeAWS)
		receive bool
		warning string // "" = no receiving warning
	}{
		{"--no-receive", func(*fakeAWS) {}, false, ""},
		{"mx elsewhere", func(f *fakeAWS) {
			f.mx = []*net.MX{{Host: "aspmx.l.google.com.", Pref: 1}}
		}, true, "already receives email through aspmx.l.google.com"},
		{"dns error", func(f *fakeAWS) {
			f.mxErr = &net.DNSError{Err: "server misbehaving", Name: testDomain, IsTemporary: true}
		}, true, "could not be looked up"},
		{"other rule set active", func(f *fakeAWS) {
			f.ruleSets["workmail"] = nil
			f.activeRuleSet = "workmail"
		}, true, "another SES receipt rule set (workmail)"},
		{"region without receiving", func(f *fakeAWS) {
			f.failOn["DescribeActiveReceiptRuleSet"] = apiErr("InvalidAction")
		}, true, "not available in eu-central-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAWS()
			tt.setup(f)
			envFile := tempEnv(t, "INBOUND_BUCKET=old\nINBOUND_DOMAIN=example.com\n")
			in := sesInput(t, envFile)
			in.Receive = tt.receive
			res, err := New(f.clients()).ProvisionSES(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if res.InboundDomain != "" || res.InboundBucket != "" {
				t.Errorf("inbound = %q %q", res.InboundDomain, res.InboundBucket)
			}
			for _, op := range []string{"CreateReceiptRuleSet", "CreateReceiptRule", "SetActiveReceiptRuleSet"} {
				if slices.Contains(f.calls, op) {
					t.Errorf("called %s", op)
				}
			}
			if _, ok := f.buckets[inboundBucketName]; ok {
				t.Error("inbound bucket created")
			}
			got := slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "Receiving email is OFF") })
			if (tt.warning != "") != got || tt.warning != "" && !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, tt.warning) }) {
				t.Errorf("warnings = %q, want %q", res.Warnings, tt.warning)
			}
			if slices.ContainsFunc(res.Records, func(r Record) bool { return r.Type == "MX" && r.Name == testDomain }) {
				t.Error("records propose the inbound MX")
			}
			env, _ := ReadEnvFile(envFile)
			if env["INBOUND_BUCKET"] != "" || env["INBOUND_DOMAIN"] != "" {
				t.Errorf("env = %v", env)
			}
			if !strings.Contains(f.topics[topicARN]["Policy"], configSetARN) || strings.Contains(f.topics[topicARN]["Policy"], "receipt-rule") {
				t.Errorf("topic policy = %s", f.topics[topicARN]["Policy"])
			}
		})
	}
}

// An MX record already pointing to SES receiving (a re-run after the DNS
// step) keeps receiving on.
func TestProvisionSESReceivingWithOwnMX(t *testing.T) {
	f := newFakeAWS()
	f.mx = []*net.MX{{Host: "inbound-smtp.eu-central-1.amazonaws.com.", Pref: 10}}
	res, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	if res.InboundDomain != testDomain {
		t.Errorf("warnings %q", res.Warnings)
	}
}

func TestProvisionSESReceivingWithoutInboundPermission(t *testing.T) {
	var granted []string
	for _, st := range guideSetupPolicy(t).Statement {
		if st.Sid != "CreateMailroomInboundBucket" {
			granted = append(granted, st.Action...)
		}
	}
	f := newFakeAWS()
	f.allow(granted...)
	f.buckets[inboundBucketName] = nil
	delete(f.buckets, inboundBucketName)
	// The fake knows actions only: deny the inbound-only ones.
	f.allowed["s3:PutBucketPolicy"] = false
	f.allowed["s3:GetBucketPolicy"] = false
	res, err := New(f.clients()).ProvisionSES(context.Background(), sesInput(t, tempEnv(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	if res.InboundDomain != "" || !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "CreateMailroomInboundBucket") }) {
		t.Errorf("inbound %q warnings %q", res.InboundDomain, res.Warnings)
	}
	if res.AccessKeyID == "" {
		t.Error("the run did not finish")
	}
}

func TestDNSRecordsWithInbound(t *testing.T) {
	recs := Records(RecordsInput{Domain: testDomain, MailFromSubdomain: "bounce", Region: testRegion, InboundDomain: testDomain})
	last := recs[len(recs)-1]
	if last.Type != "MX" || last.Name != testDomain || last.Value != "inbound-smtp.eu-central-1.amazonaws.com" || last.Priority != 10 {
		t.Errorf("records = %+v", recs)
	}
}
