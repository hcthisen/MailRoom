package provision

import (
	"context"
	"net"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	sesv1types "github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// --- DNS

func (f *fakeAWS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "LookupMX")
	if f.mxErr != nil {
		return nil, f.mxErr
	}
	if len(f.mx) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return slices.Clone(f.mx), nil
}

// --- S3 bucket policy

func (f *fakeAWS) GetBucketPolicy(_ context.Context, in *s3.GetBucketPolicyInput, _ ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetBucketPolicy", false); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	if b.policy == "" {
		return nil, apiErr("NoSuchBucketPolicy")
	}
	return &s3.GetBucketPolicyOutput{Policy: aws.String(b.policy)}, nil
}

func (f *fakeAWS) PutBucketPolicy(_ context.Context, in *s3.PutBucketPolicyInput, _ ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutBucketPolicy", true); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	b.policy = aws.ToString(in.Policy)
	return &s3.PutBucketPolicyOutput{}, nil
}

// --- SES v1 receipt rules

func (f *fakeAWS) DescribeActiveReceiptRuleSet(context.Context, *ses.DescribeActiveReceiptRuleSetInput, ...func(*ses.Options)) (*ses.DescribeActiveReceiptRuleSetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("DescribeActiveReceiptRuleSet", false); err != nil {
		return nil, err
	}
	out := &ses.DescribeActiveReceiptRuleSetOutput{}
	if f.activeRuleSet != "" {
		out.Metadata = &sesv1types.ReceiptRuleSetMetadata{Name: aws.String(f.activeRuleSet)}
		out.Rules = slices.Clone(f.ruleSets[f.activeRuleSet])
	}
	return out, nil
}

func (f *fakeAWS) CreateReceiptRuleSet(_ context.Context, in *ses.CreateReceiptRuleSetInput, _ ...func(*ses.Options)) (*ses.CreateReceiptRuleSetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.RuleSetName)
	if _, ok := f.ruleSets[name]; ok {
		f.calls = append(f.calls, "CreateReceiptRuleSet")
		return nil, apiErr("AlreadyExists")
	}
	if err := f.op("CreateReceiptRuleSet", true); err != nil {
		return nil, err
	}
	f.ruleSets[name] = nil
	return &ses.CreateReceiptRuleSetOutput{}, nil
}

func (f *fakeAWS) DescribeReceiptRule(_ context.Context, in *ses.DescribeReceiptRuleInput, _ ...func(*ses.Options)) (*ses.DescribeReceiptRuleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("DescribeReceiptRule", false); err != nil {
		return nil, err
	}
	rules, ok := f.ruleSets[aws.ToString(in.RuleSetName)]
	if !ok {
		return nil, apiErr("RuleSetDoesNotExist")
	}
	for _, r := range rules {
		if aws.ToString(r.Name) == aws.ToString(in.RuleName) {
			cp := r
			return &ses.DescribeReceiptRuleOutput{Rule: &cp}, nil
		}
	}
	return nil, apiErr("RuleDoesNotExist")
}

func (f *fakeAWS) CreateReceiptRule(_ context.Context, in *ses.CreateReceiptRuleInput, _ ...func(*ses.Options)) (*ses.CreateReceiptRuleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("CreateReceiptRule", true); err != nil {
		return nil, err
	}
	set := aws.ToString(in.RuleSetName)
	if _, ok := f.ruleSets[set]; !ok {
		return nil, apiErr("RuleSetDoesNotExist")
	}
	if err := f.checkReceiptDestinations(in.Rule); err != nil {
		return nil, err
	}
	f.ruleSets[set] = append(f.ruleSets[set], *in.Rule)
	return &ses.CreateReceiptRuleOutput{}, nil
}

func (f *fakeAWS) UpdateReceiptRule(_ context.Context, in *ses.UpdateReceiptRuleInput, _ ...func(*ses.Options)) (*ses.UpdateReceiptRuleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("UpdateReceiptRule", true); err != nil {
		return nil, err
	}
	set := aws.ToString(in.RuleSetName)
	if err := f.checkReceiptDestinations(in.Rule); err != nil {
		return nil, err
	}
	for i, r := range f.ruleSets[set] {
		if aws.ToString(r.Name) == aws.ToString(in.Rule.Name) {
			f.ruleSets[set][i] = *in.Rule
			return &ses.UpdateReceiptRuleOutput{}, nil
		}
	}
	return nil, apiErr("RuleDoesNotExist")
}

// checkReceiptDestinations mimics SES's check that it may write to the
// bucket (bucket policy) and publish to the topic (topic policy) before a
// rule is saved.
func (f *fakeAWS) checkReceiptDestinations(rule *sesv1types.ReceiptRule) error {
	for _, a := range rule.Actions {
		if a.S3Action == nil {
			continue
		}
		b, ok := f.buckets[aws.ToString(a.S3Action.BucketName)]
		if !ok || b.policy == "" {
			return apiErr("InvalidS3Configuration")
		}
		if t := aws.ToString(a.S3Action.TopicArn); t != "" {
			attrs, ok := f.topics[t]
			if !ok || attrs["Policy"] == "" {
				return apiErr("InvalidSnsTopic")
			}
		}
	}
	return nil
}

func (f *fakeAWS) SetActiveReceiptRuleSet(_ context.Context, in *ses.SetActiveReceiptRuleSetInput, _ ...func(*ses.Options)) (*ses.SetActiveReceiptRuleSetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("SetActiveReceiptRuleSet", true); err != nil {
		return nil, err
	}
	name := aws.ToString(in.RuleSetName)
	if _, ok := f.ruleSets[name]; !ok && name != "" {
		return nil, apiErr("RuleSetDoesNotExist")
	}
	f.activeRuleSet = name
	return &ses.SetActiveReceiptRuleSetOutput{}, nil
}
