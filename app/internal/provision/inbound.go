package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	sesv1types "github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// Receiving (N-1): SES accepts mail for DOMAIN (the MX record points to
// inbound-smtp.REGION.amazonaws.com), stores each message in the inbound
// bucket under inbound/ and notifies the event topic, whose webhook is
// Mailroom's own. Mailroom then fetches and parses the stored message.

// MXResolver looks up the MX records of a domain (*net.Resolver).
type MXResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// InboundMXHost is the SES receiving endpoint of a region, the target of
// DOMAIN's MX record.
func InboundMXHost(region string) string { return "inbound-smtp." + region + ".amazonaws.com" }

// InboundBucketName is the inbound mail bucket of an account and region.
func InboundBucketName(account, region string) string {
	return "mailroom-inbound-" + account + "-" + region
}

func (a arns) receiptRule() string {
	return fmt.Sprintf("arn:%s:ses:%s:%s:receipt-rule-set/%s:receipt-rule/%s", a.partition, a.region, a.account, ReceiptRuleSetName, ReceiptRuleName)
}

// receiveCheck decides whether this run sets up receiving. It changes
// nothing. Receiving is turned off, with a warning, when DOMAIN already
// receives mail elsewhere (an MX record not pointing to SES), when another
// receipt rule set is active in the region (someone else's receiving
// setup), or when SES receiving can't be used in the region.
func (r *run) receiveCheck(ctx context.Context) error {
	if !r.in.Receive {
		r.step("receiving", ActionUnchanged, "off (--no-receive)")
		return nil
	}
	off := func(reason string) {
		r.receive = false
		r.res.Warnings = append(r.res.Warnings, "Receiving email is OFF: "+reason+
			". Mailroom's inbox stays empty; replies go wherever the domain's mail goes today")
		r.step("receiving", ActionUnchanged, "off: "+reason)
	}
	want := InboundMXHost(r.c.Region)
	resolver := r.c.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	mxs, err := resolver.LookupMX(ctx, r.in.Domain)
	var dnsErr *net.DNSError
	switch {
	case err == nil:
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		mxs = nil
	default:
		off(fmt.Sprintf("the MX records of %s could not be looked up (%v); run provision ses again when DNS answers", r.in.Domain, err))
		return nil
	}
	var other []string
	for _, mx := range mxs {
		host := strings.ToLower(strings.TrimSuffix(mx.Host, "."))
		if host != want && host != "" {
			other = append(other, host)
		}
	}
	if len(other) > 0 {
		off(fmt.Sprintf("%s already receives email through %s, and Mailroom never replaces that. "+
			"To receive in Mailroom instead, remove those MX records first and run provision ses again", r.in.Domain, strings.Join(other, ", ")))
		return nil
	}
	active, err := r.c.Receipt.DescribeActiveReceiptRuleSet(ctx, &ses.DescribeActiveReceiptRuleSetInput{})
	switch {
	case isAPIError(err, s3AccessDenied...):
		off("the setup user may not read SES receipt rules (it needs AmazonSESFullAccess, guide step 2.6)")
		return nil
	case err != nil:
		off(fmt.Sprintf("SES email receiving is not available in %s (%v). It is offered only in some regions", r.c.Region, err))
		return nil
	}
	if active.Metadata != nil {
		name := aws.ToString(active.Metadata.Name)
		if name != "" && name != ReceiptRuleSetName {
			off(fmt.Sprintf("another SES receipt rule set (%s) is already active in %s, so this account receives mail with its own setup", name, r.c.Region))
			return nil
		}
		r.ruleSetActive = name == ReceiptRuleSetName
	}
	r.receive = true
	r.res.InboundDomain = r.in.Domain
	r.res.InboundBucket = InboundBucketName(r.res.AccountID, r.c.Region)
	r.step("receiving", ActionUnchanged, "on: mail to any address on "+r.in.Domain+" lands in Mailroom's inbox")
	return nil
}

// inboundSpec is the inbound mail bucket.
func (r *run) inboundSpec() bucketSpec {
	return bucketSpec{
		name: InboundBucketName(r.res.AccountID, r.c.Region), label: "s3 inbound",
		prefix: InboundPrefix, days: InboundRetentionDays, ruleID: InboundLifecycleRuleID,
	}
}

// inbound ensures the inbound bucket (private, encrypted, inbound/
// expiring after InboundRetentionDays, SES may write), the receipt rule set
// and its rule, and activates the set when no set is active.
func (r *run) inbound(ctx context.Context) error {
	if !r.receive {
		return nil
	}
	spec := r.inboundSpec()
	created, err := r.ensureBucket(ctx, spec)
	if err == nil {
		err = r.inboundBucketPolicy(ctx, spec, created)
	}
	if isAPIError(err, s3AccessDenied...) {
		// A setup user from before receiving existed (guide step 2.6 without
		// the inbound bucket): keep the rest of the run working.
		r.receive = false
		r.res.InboundDomain, r.res.InboundBucket = "", ""
		r.res.Warnings = append(r.res.Warnings, "Receiving email is OFF: the setup user may not create the inbound bucket "+spec.name+
			". Add the CreateMailroomInboundBucket statement of guide step 2.6 to its allow-mailroom-setup policy and run provision ses again")
		r.step("receiving", ActionUnchanged, "off: the setup user lacks the inbound bucket permissions (guide step 2.6)")
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.receiptRuleSet(ctx); err != nil {
		return err
	}
	if err := r.receiptRule(ctx, spec); err != nil {
		return err
	}
	return r.activateRuleSet(ctx)
}

// InboundBucketPolicy lets SES store received mail under inbound/, only
// for this account's receipt rule.
func InboundBucketPolicy(bucketARN, account, ruleARN string) PolicyDocument {
	return PolicyDocument{
		Version: policyVersion,
		Statement: []Statement{{
			Sid:       "AllowSESPuts",
			Effect:    "Allow",
			Principal: map[string]StringList{"Service": {"ses.amazonaws.com"}},
			Action:    StringList{"s3:PutObject"},
			Resource:  StringList{bucketARN + "/" + InboundPrefix + "*"},
			Condition: map[string]map[string]StringList{"StringEquals": {
				"AWS:SourceAccount": {account},
				"AWS:SourceArn":     {ruleARN},
			}},
		}},
	}
}

func (r *run) inboundBucketPolicy(ctx context.Context, spec bucketSpec, created bool) error {
	const detail = "ses.amazonaws.com may store mail under " + InboundPrefix
	want := InboundBucketPolicy(r.arns.bucket(spec.name), r.res.AccountID, r.arns.receiptRule())
	b, owner := aws.String(spec.name), aws.String(r.res.AccountID)
	state := stateNew
	if !created {
		out, err := r.c.S3.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b, ExpectedBucketOwner: owner})
		if state, err = readState(err, s3NoBucketPolicy, "s3 get-bucket-policy"); err != nil {
			return err
		}
		if state == stateRead && policyMatches(aws.ToString(out.Policy), want) {
			r.step(spec.stepName("bucket policy"), ActionUnchanged, detail)
			return nil
		}
	}
	if _, err := r.c.S3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: b, ExpectedBucketOwner: owner, Policy: aws.String(want.JSON())}); err != nil {
		return fmt.Errorf("s3 put-bucket-policy %s: %w", spec.name, err)
	}
	r.step(spec.stepName("bucket policy"), state.action(), detail+state.suffix("s3:GetBucketPolicy"))
	return nil
}

func (r *run) receiptRuleSet(ctx context.Context) error {
	_, err := r.c.Receipt.CreateReceiptRuleSet(ctx, &ses.CreateReceiptRuleSetInput{RuleSetName: aws.String(ReceiptRuleSetName)})
	switch {
	case err == nil:
		r.step("ses receipt rule set", ActionCreated, ReceiptRuleSetName)
	case isAPIError(err, sesAlreadyExists...):
		r.step("ses receipt rule set", ActionUnchanged, ReceiptRuleSetName)
	default:
		return fmt.Errorf("ses create-receipt-rule-set %s: %w", ReceiptRuleSetName, err)
	}
	return nil
}

// wantReceiptRule is the rule receiving needs: mail to DOMAIN, spam and
// virus scan, stored in the bucket with a notification on the topic.
func (r *run) wantReceiptRule(spec bucketSpec) sesv1types.ReceiptRule {
	return sesv1types.ReceiptRule{
		Name:        aws.String(ReceiptRuleName),
		Enabled:     true,
		ScanEnabled: true,
		Recipients:  []string{r.in.Domain},
		TlsPolicy:   sesv1types.TlsPolicyOptional,
		Actions: []sesv1types.ReceiptAction{{S3Action: &sesv1types.S3Action{
			BucketName:      aws.String(spec.name),
			ObjectKeyPrefix: aws.String(InboundPrefix),
			TopicArn:        aws.String(r.res.TopicARN),
		}}},
	}
}

func receiptRuleOK(got, want sesv1types.ReceiptRule) bool {
	if got.Enabled != want.Enabled || got.ScanEnabled != want.ScanEnabled || got.TlsPolicy != want.TlsPolicy {
		return false
	}
	gr, wr := slices.Clone(got.Recipients), slices.Clone(want.Recipients)
	for i := range gr {
		gr[i] = strings.ToLower(gr[i])
	}
	slices.Sort(gr)
	slices.Sort(wr)
	if !slices.Equal(gr, wr) || len(got.Actions) != 1 {
		return false
	}
	g, w := got.Actions[0].S3Action, want.Actions[0].S3Action
	return g != nil && aws.ToString(g.BucketName) == aws.ToString(w.BucketName) &&
		aws.ToString(g.ObjectKeyPrefix) == aws.ToString(w.ObjectKeyPrefix) &&
		aws.ToString(g.TopicArn) == aws.ToString(w.TopicArn) && aws.ToString(g.KmsKeyArn) == ""
}

func (r *run) receiptRule(ctx context.Context, spec bucketSpec) error {
	want := r.wantReceiptRule(spec)
	detail := fmt.Sprintf("mail to %s → s3://%s/%s, notify %s", r.in.Domain, spec.name, InboundPrefix, TopicName)
	out, err := r.c.Receipt.DescribeReceiptRule(ctx, &ses.DescribeReceiptRuleInput{
		RuleSetName: aws.String(ReceiptRuleSetName), RuleName: aws.String(ReceiptRuleName),
	})
	switch {
	case isAPIError(err, sesRuleNotFound...):
		if _, err := r.c.Receipt.CreateReceiptRule(ctx, &ses.CreateReceiptRuleInput{RuleSetName: aws.String(ReceiptRuleSetName), Rule: &want}); err != nil {
			return fmt.Errorf("ses create-receipt-rule %s: %w", ReceiptRuleName, err)
		}
		r.step("ses receipt rule", ActionCreated, detail)
		return nil
	case err != nil:
		return fmt.Errorf("ses describe-receipt-rule %s: %w", ReceiptRuleName, err)
	}
	if out.Rule != nil && receiptRuleOK(*out.Rule, want) {
		r.step("ses receipt rule", ActionUnchanged, detail)
		return nil
	}
	if _, err := r.c.Receipt.UpdateReceiptRule(ctx, &ses.UpdateReceiptRuleInput{RuleSetName: aws.String(ReceiptRuleSetName), Rule: &want}); err != nil {
		return fmt.Errorf("ses update-receipt-rule %s: %w", ReceiptRuleName, err)
	}
	r.step("ses receipt rule", ActionUpdated, detail)
	return nil
}

func (r *run) activateRuleSet(ctx context.Context) error {
	if r.ruleSetActive {
		r.step("ses active receipt rule set", ActionUnchanged, ReceiptRuleSetName)
		return nil
	}
	if _, err := r.c.Receipt.SetActiveReceiptRuleSet(ctx, &ses.SetActiveReceiptRuleSetInput{RuleSetName: aws.String(ReceiptRuleSetName)}); err != nil {
		return fmt.Errorf("ses set-active-receipt-rule-set %s: %w", ReceiptRuleSetName, err)
	}
	r.ruleSetActive = true
	r.step("ses active receipt rule set", ActionCreated, ReceiptRuleSetName)
	return nil
}
