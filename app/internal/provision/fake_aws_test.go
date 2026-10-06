package provision

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

const (
	testAccount = "123456789012"
	testRegion  = "eu-central-1"
	testDomain  = "example.com"
)

// fakeAWS is an in-memory AWS account implementing every client interface.
type fakeAWS struct {
	mu sync.Mutex

	region string

	identities map[string]*sesv2.GetEmailIdentityOutput
	account    sesv2.GetAccountOutput
	details    *sesv2.PutAccountDetailsInput
	configSets map[string][]sestypes.EventDestination

	topics map[string]map[string]string

	users    map[string]bool
	policies map[string]string // user/policy -> URL-encoded document, as IAM returns it
	keys     []iamtypes.AccessKeyMetadata
	secrets  map[string]string
	keySeq   int

	buckets map[string]*fakeBucket

	// calls records every operation; mutations records the writing ones
	// that were allowed; denied records the actions the allowlist refused.
	calls, mutations, denied []string
	// used records the IAM actions the allowlist granted.
	used map[string]bool
	// failOn makes the named operation fail.
	failOn map[string]error
	// allowed, when set, is the IAM action allowlist of the caller: any
	// other S3 or IAM action is answered with AccessDenied (SES, SNS and STS
	// are covered by the setup user's managed full-access policies).
	allowed map[string]bool
	now     time.Time
}

type fakeBucket struct {
	region     string
	pab        *s3types.PublicAccessBlockConfiguration
	enc        *s3types.ServerSideEncryptionConfiguration
	lifecycle  []s3types.LifecycleRule
	versioning s3types.BucketVersioningStatus
}

func newFakeAWS() *fakeAWS {
	return &fakeAWS{
		region:     testRegion,
		identities: map[string]*sesv2.GetEmailIdentityOutput{},
		account:    sesv2.GetAccountOutput{SendingEnabled: true},
		configSets: map[string][]sestypes.EventDestination{},
		topics:     map[string]map[string]string{},
		users:      map[string]bool{},
		policies:   map[string]string{},
		secrets:    map[string]string{},
		buckets:    map[string]*fakeBucket{},
		failOn:     map[string]error{},
		now:        time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeAWS) clients() Clients {
	return Clients{STS: f, SES: f, SNS: f, IAM: f, S3: f, Region: f.region}
}

// op records a call and applies failOn and the allowlist. extra are IAM
// actions the call needs besides its own (e.g. iam:TagUser for tags).
func (f *fakeAWS) op(name string, mutating bool, extra ...string) error {
	f.calls = append(f.calls, name)
	if f.allowed != nil {
		needed := extra
		if a, ok := fakeIAMActions[name]; ok {
			needed = append([]string{a}, extra...)
		}
		for _, a := range needed {
			if !f.allowed[a] {
				f.denied = append(f.denied, a)
				return apiErr("AccessDenied")
			}
		}
		if f.used == nil {
			f.used = map[string]bool{}
		}
		for _, a := range needed {
			f.used[a] = true
		}
	}
	if mutating {
		f.mutations = append(f.mutations, name)
	}
	return f.failOn[name]
}

// fakeIAMActions maps the S3 and IAM operations to the IAM action real AWS
// authorizes them with.
var fakeIAMActions = map[string]string{
	"HeadBucket":                      "s3:ListBucket",
	"CreateBucket":                    "s3:CreateBucket",
	"GetPublicAccessBlock":            "s3:GetBucketPublicAccessBlock",
	"PutPublicAccessBlock":            "s3:PutBucketPublicAccessBlock",
	"GetBucketEncryption":             "s3:GetEncryptionConfiguration",
	"PutBucketEncryption":             "s3:PutEncryptionConfiguration",
	"GetBucketLifecycleConfiguration": "s3:GetLifecycleConfiguration",
	"PutBucketLifecycleConfiguration": "s3:PutLifecycleConfiguration",
	"GetBucketVersioning":             "s3:GetBucketVersioning",
	"PutBucketVersioning":             "s3:PutBucketVersioning",
	"GetUser":                         "iam:GetUser",
	"CreateUser":                      "iam:CreateUser",
	"GetUserPolicy":                   "iam:GetUserPolicy",
	"PutUserPolicy":                   "iam:PutUserPolicy",
	"ListAccessKeys":                  "iam:ListAccessKeys",
	"CreateAccessKey":                 "iam:CreateAccessKey",
	"DeleteAccessKey":                 "iam:DeleteAccessKey",
}

// allow restricts the fake to the given actions.
func (f *fakeAWS) allow(actions ...string) {
	f.allowed = map[string]bool{}
	for _, a := range actions {
		f.allowed[a] = true
	}
}

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

// --- STS

func (f *fakeAWS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetCallerIdentity", false); err != nil {
		return nil, err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(testAccount), Arn: aws.String("arn:aws:iam::" + testAccount + ":user/setup-agent")}, nil
}

// --- SES

func (f *fakeAWS) GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetAccount", false); err != nil {
		return nil, err
	}
	out := f.account
	if f.account.SuppressionAttributes != nil {
		out.SuppressionAttributes = &sestypes.SuppressionAttributes{SuppressedReasons: slices.Clone(f.account.SuppressionAttributes.SuppressedReasons)}
	}
	return &out, nil
}

func (f *fakeAWS) PutAccountSuppressionAttributes(_ context.Context, in *sesv2.PutAccountSuppressionAttributesInput, _ ...func(*sesv2.Options)) (*sesv2.PutAccountSuppressionAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutAccountSuppressionAttributes", true); err != nil {
		return nil, err
	}
	f.account.SuppressionAttributes = &sestypes.SuppressionAttributes{SuppressedReasons: slices.Clone(in.SuppressedReasons)}
	return &sesv2.PutAccountSuppressionAttributesOutput{}, nil
}

func (f *fakeAWS) PutAccountDetails(_ context.Context, in *sesv2.PutAccountDetailsInput, _ ...func(*sesv2.Options)) (*sesv2.PutAccountDetailsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutAccountDetails", true); err != nil {
		return nil, err
	}
	f.details = in
	f.account.Details = &sestypes.AccountDetails{
		MailType:      in.MailType,
		WebsiteURL:    in.WebsiteURL,
		ReviewDetails: &sestypes.ReviewDetails{Status: sestypes.ReviewStatusPending, CaseId: aws.String("case-1")},
	}
	return &sesv2.PutAccountDetailsOutput{}, nil
}

func (f *fakeAWS) GetEmailIdentity(_ context.Context, in *sesv2.GetEmailIdentityInput, _ ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetEmailIdentity", false); err != nil {
		return nil, err
	}
	id, ok := f.identities[aws.ToString(in.EmailIdentity)]
	if !ok {
		return nil, &sestypes.NotFoundException{Message: aws.String("identity not found")}
	}
	cp := *id
	if id.DkimAttributes != nil {
		d := *id.DkimAttributes
		cp.DkimAttributes = &d
	}
	if id.MailFromAttributes != nil {
		m := *id.MailFromAttributes
		cp.MailFromAttributes = &m
	}
	return &cp, nil
}

func (f *fakeAWS) CreateEmailIdentity(_ context.Context, in *sesv2.CreateEmailIdentityInput, _ ...func(*sesv2.Options)) (*sesv2.CreateEmailIdentityOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("CreateEmailIdentity", true); err != nil {
		return nil, err
	}
	name := aws.ToString(in.EmailIdentity)
	if _, ok := f.identities[name]; ok {
		return nil, &sestypes.AlreadyExistsException{}
	}
	d := &sestypes.DkimAttributes{
		SigningAttributesOrigin: sestypes.DkimSigningAttributesOriginAwsSes,
		Status:                  sestypes.DkimStatusPending,
		Tokens:                  []string{"tok1", "tok2", "tok3"},
		SigningHostedZone:       aws.String("dkim.amazonses.com"),
	}
	if in.DkimSigningAttributes != nil {
		d.NextSigningKeyLength = in.DkimSigningAttributes.NextSigningKeyLength
	}
	f.identities[name] = &sesv2.GetEmailIdentityOutput{IdentityType: sestypes.IdentityTypeDomain, DkimAttributes: d}
	return &sesv2.CreateEmailIdentityOutput{DkimAttributes: d}, nil
}

func (f *fakeAWS) PutEmailIdentityDkimSigningAttributes(_ context.Context, in *sesv2.PutEmailIdentityDkimSigningAttributesInput, _ ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityDkimSigningAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutEmailIdentityDkimSigningAttributes", true); err != nil {
		return nil, err
	}
	id := f.identities[aws.ToString(in.EmailIdentity)]
	id.DkimAttributes.NextSigningKeyLength = in.SigningAttributes.NextSigningKeyLength
	return &sesv2.PutEmailIdentityDkimSigningAttributesOutput{}, nil
}

func (f *fakeAWS) PutEmailIdentityMailFromAttributes(_ context.Context, in *sesv2.PutEmailIdentityMailFromAttributesInput, _ ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityMailFromAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutEmailIdentityMailFromAttributes", true); err != nil {
		return nil, err
	}
	id := f.identities[aws.ToString(in.EmailIdentity)]
	id.MailFromAttributes = &sestypes.MailFromAttributes{
		MailFromDomain:       in.MailFromDomain,
		BehaviorOnMxFailure:  in.BehaviorOnMxFailure,
		MailFromDomainStatus: sestypes.MailFromDomainStatusPending,
	}
	return &sesv2.PutEmailIdentityMailFromAttributesOutput{}, nil
}

func (f *fakeAWS) PutEmailIdentityConfigurationSetAttributes(_ context.Context, in *sesv2.PutEmailIdentityConfigurationSetAttributesInput, _ ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityConfigurationSetAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutEmailIdentityConfigurationSetAttributes", true); err != nil {
		return nil, err
	}
	if _, ok := f.configSets[aws.ToString(in.ConfigurationSetName)]; !ok {
		return nil, &sestypes.NotFoundException{}
	}
	f.identities[aws.ToString(in.EmailIdentity)].ConfigurationSetName = in.ConfigurationSetName
	return &sesv2.PutEmailIdentityConfigurationSetAttributesOutput{}, nil
}

func (f *fakeAWS) GetConfigurationSet(_ context.Context, in *sesv2.GetConfigurationSetInput, _ ...func(*sesv2.Options)) (*sesv2.GetConfigurationSetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetConfigurationSet", false); err != nil {
		return nil, err
	}
	if _, ok := f.configSets[aws.ToString(in.ConfigurationSetName)]; !ok {
		return nil, &sestypes.NotFoundException{}
	}
	return &sesv2.GetConfigurationSetOutput{ConfigurationSetName: in.ConfigurationSetName}, nil
}

func (f *fakeAWS) CreateConfigurationSet(_ context.Context, in *sesv2.CreateConfigurationSetInput, _ ...func(*sesv2.Options)) (*sesv2.CreateConfigurationSetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("CreateConfigurationSet", true); err != nil {
		return nil, err
	}
	name := aws.ToString(in.ConfigurationSetName)
	if _, ok := f.configSets[name]; ok {
		return nil, &sestypes.AlreadyExistsException{}
	}
	f.configSets[name] = nil
	return &sesv2.CreateConfigurationSetOutput{}, nil
}

func (f *fakeAWS) GetConfigurationSetEventDestinations(_ context.Context, in *sesv2.GetConfigurationSetEventDestinationsInput, _ ...func(*sesv2.Options)) (*sesv2.GetConfigurationSetEventDestinationsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetConfigurationSetEventDestinations", false); err != nil {
		return nil, err
	}
	dests, ok := f.configSets[aws.ToString(in.ConfigurationSetName)]
	if !ok {
		return nil, &sestypes.NotFoundException{}
	}
	return &sesv2.GetConfigurationSetEventDestinationsOutput{EventDestinations: slices.Clone(dests)}, nil
}

func (f *fakeAWS) CreateConfigurationSetEventDestination(_ context.Context, in *sesv2.CreateConfigurationSetEventDestinationInput, _ ...func(*sesv2.Options)) (*sesv2.CreateConfigurationSetEventDestinationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("CreateConfigurationSetEventDestination", true); err != nil {
		return nil, err
	}
	cs := aws.ToString(in.ConfigurationSetName)
	f.configSets[cs] = append(f.configSets[cs], eventDest(in.EventDestinationName, in.EventDestination))
	return &sesv2.CreateConfigurationSetEventDestinationOutput{}, nil
}

func (f *fakeAWS) UpdateConfigurationSetEventDestination(_ context.Context, in *sesv2.UpdateConfigurationSetEventDestinationInput, _ ...func(*sesv2.Options)) (*sesv2.UpdateConfigurationSetEventDestinationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("UpdateConfigurationSetEventDestination", true); err != nil {
		return nil, err
	}
	cs := aws.ToString(in.ConfigurationSetName)
	for i, d := range f.configSets[cs] {
		if aws.ToString(d.Name) == aws.ToString(in.EventDestinationName) {
			f.configSets[cs][i] = eventDest(in.EventDestinationName, in.EventDestination)
			return &sesv2.UpdateConfigurationSetEventDestinationOutput{}, nil
		}
	}
	return nil, &sestypes.NotFoundException{}
}

func eventDest(name *string, d *sestypes.EventDestinationDefinition) sestypes.EventDestination {
	return sestypes.EventDestination{
		Name:               name,
		Enabled:            d.Enabled,
		MatchingEventTypes: slices.Clone(d.MatchingEventTypes),
		SnsDestination:     d.SnsDestination,
	}
}

// --- SNS

func (f *fakeAWS) topicARN(name string) string {
	return fmt.Sprintf("arn:aws:sns:%s:%s:%s", f.region, testAccount, name)
}

func (f *fakeAWS) GetTopicAttributes(_ context.Context, in *sns.GetTopicAttributesInput, _ ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetTopicAttributes", false); err != nil {
		return nil, err
	}
	attrs, ok := f.topics[aws.ToString(in.TopicArn)]
	if !ok {
		return nil, &snstypes.NotFoundException{}
	}
	cp := map[string]string{}
	for k, v := range attrs {
		cp[k] = v
	}
	return &sns.GetTopicAttributesOutput{Attributes: cp}, nil
}

func (f *fakeAWS) CreateTopic(_ context.Context, in *sns.CreateTopicInput, _ ...func(*sns.Options)) (*sns.CreateTopicOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("CreateTopic", true); err != nil {
		return nil, err
	}
	arn := f.topicARN(aws.ToString(in.Name))
	if _, ok := f.topics[arn]; !ok {
		f.topics[arn] = map[string]string{"TopicArn": arn, "Policy": `{"Version":"2008-10-17","Id":"__default_policy_ID","Statement":[]}`}
	}
	return &sns.CreateTopicOutput{TopicArn: aws.String(arn)}, nil
}

func (f *fakeAWS) SetTopicAttributes(_ context.Context, in *sns.SetTopicAttributesInput, _ ...func(*sns.Options)) (*sns.SetTopicAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("SetTopicAttributes", true); err != nil {
		return nil, err
	}
	attrs, ok := f.topics[aws.ToString(in.TopicArn)]
	if !ok {
		return nil, &snstypes.NotFoundException{}
	}
	attrs[aws.ToString(in.AttributeName)] = aws.ToString(in.AttributeValue)
	return &sns.SetTopicAttributesOutput{}, nil
}

// --- IAM

func (f *fakeAWS) GetUser(_ context.Context, in *iam.GetUserInput, _ ...func(*iam.Options)) (*iam.GetUserOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetUser", false); err != nil {
		return nil, err
	}
	if !f.users[aws.ToString(in.UserName)] {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetUserOutput{User: &iamtypes.User{UserName: in.UserName}}, nil
}

func (f *fakeAWS) CreateUser(_ context.Context, in *iam.CreateUserInput, _ ...func(*iam.Options)) (*iam.CreateUserOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var extra []string
	if len(in.Tags) > 0 {
		extra = append(extra, "iam:TagUser")
	}
	if err := f.op("CreateUser", true, extra...); err != nil {
		return nil, err
	}
	f.users[aws.ToString(in.UserName)] = true
	return &iam.CreateUserOutput{User: &iamtypes.User{UserName: in.UserName}}, nil
}

func (f *fakeAWS) GetUserPolicy(_ context.Context, in *iam.GetUserPolicyInput, _ ...func(*iam.Options)) (*iam.GetUserPolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetUserPolicy", false); err != nil {
		return nil, err
	}
	doc, ok := f.policies[aws.ToString(in.UserName)+"/"+aws.ToString(in.PolicyName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetUserPolicyOutput{PolicyDocument: aws.String(doc), PolicyName: in.PolicyName, UserName: in.UserName}, nil
}

func (f *fakeAWS) PutUserPolicy(_ context.Context, in *iam.PutUserPolicyInput, _ ...func(*iam.Options)) (*iam.PutUserPolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutUserPolicy", true); err != nil {
		return nil, err
	}
	f.policies[aws.ToString(in.UserName)+"/"+aws.ToString(in.PolicyName)] = url.PathEscape(aws.ToString(in.PolicyDocument))
	return &iam.PutUserPolicyOutput{}, nil
}

// policy returns the decoded stored policy.
func (f *fakeAWS) policy(user, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, _ := url.PathUnescape(f.policies[user+"/"+name])
	return doc
}

func (f *fakeAWS) ListAccessKeys(_ context.Context, in *iam.ListAccessKeysInput, _ ...func(*iam.Options)) (*iam.ListAccessKeysOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("ListAccessKeys", false); err != nil {
		return nil, err
	}
	// Page one key at a time to exercise pagination.
	start := 0
	if in.Marker != nil {
		fmt.Sscanf(aws.ToString(in.Marker), "%d", &start)
	}
	out := &iam.ListAccessKeysOutput{}
	if start < len(f.keys) {
		out.AccessKeyMetadata = []iamtypes.AccessKeyMetadata{f.keys[start]}
	}
	if start+1 < len(f.keys) {
		out.IsTruncated = true
		out.Marker = aws.String(fmt.Sprint(start + 1))
	}
	return out, nil
}

func (f *fakeAWS) CreateAccessKey(_ context.Context, in *iam.CreateAccessKeyInput, _ ...func(*iam.Options)) (*iam.CreateAccessKeyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("CreateAccessKey", true); err != nil {
		return nil, err
	}
	if len(f.keys) >= 2 {
		return nil, apiErr("LimitExceeded")
	}
	f.keySeq++
	id := fmt.Sprintf("AKIATEST%012d", f.keySeq)
	secret := fmt.Sprintf("secret/%d+SECRETSECRETSECRET", f.keySeq)
	created := f.now.Add(time.Duration(f.keySeq) * time.Hour)
	f.keys = append(f.keys, iamtypes.AccessKeyMetadata{AccessKeyId: aws.String(id), Status: iamtypes.StatusTypeActive, UserName: in.UserName, CreateDate: &created})
	f.secrets[id] = secret
	return &iam.CreateAccessKeyOutput{AccessKey: &iamtypes.AccessKey{AccessKeyId: aws.String(id), SecretAccessKey: aws.String(secret), UserName: in.UserName, Status: iamtypes.StatusTypeActive}}, nil
}

func (f *fakeAWS) DeleteAccessKey(_ context.Context, in *iam.DeleteAccessKeyInput, _ ...func(*iam.Options)) (*iam.DeleteAccessKeyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("DeleteAccessKey", true); err != nil {
		return nil, err
	}
	i := slices.IndexFunc(f.keys, func(k iamtypes.AccessKeyMetadata) bool {
		return aws.ToString(k.AccessKeyId) == aws.ToString(in.AccessKeyId)
	})
	if i < 0 {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	f.keys = slices.Delete(f.keys, i, i+1)
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (f *fakeAWS) keyIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return keyIDs(f.keys)
}

// --- S3

func (f *fakeAWS) bucketFor(name *string) (*fakeBucket, error) {
	b, ok := f.buckets[aws.ToString(name)]
	if !ok {
		return nil, apiErr("NoSuchBucket")
	}
	return b, nil
}

func (f *fakeAWS) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("HeadBucket", false); err != nil {
		return nil, err
	}
	if aws.ToString(in.ExpectedBucketOwner) != testAccount {
		return nil, apiErr("Forbidden")
	}
	if _, ok := f.buckets[aws.ToString(in.Bucket)]; !ok {
		return nil, &s3types.NotFound{}
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeAWS) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var extra []string
	if in.ObjectOwnership != "" {
		extra = append(extra, "s3:PutBucketOwnershipControls")
	}
	if err := f.op("CreateBucket", true, extra...); err != nil {
		return nil, err
	}
	region := "us-east-1"
	if in.CreateBucketConfiguration != nil {
		region = string(in.CreateBucketConfiguration.LocationConstraint)
	}
	if region != f.region {
		return nil, apiErr("IllegalLocationConstraintException")
	}
	// New buckets get SSE-S3 by default, like real S3.
	f.buckets[aws.ToString(in.Bucket)] = &fakeBucket{
		region: region,
		enc: &s3types.ServerSideEncryptionConfiguration{Rules: []s3types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{SSEAlgorithm: s3types.ServerSideEncryptionAes256},
		}}},
	}
	return &s3.CreateBucketOutput{}, nil
}

func (f *fakeAWS) GetPublicAccessBlock(_ context.Context, in *s3.GetPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.GetPublicAccessBlockOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetPublicAccessBlock", false); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	if b.pab == nil {
		return nil, apiErr("NoSuchPublicAccessBlockConfiguration")
	}
	cp := *b.pab
	return &s3.GetPublicAccessBlockOutput{PublicAccessBlockConfiguration: &cp}, nil
}

func (f *fakeAWS) PutPublicAccessBlock(_ context.Context, in *s3.PutPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutPublicAccessBlock", true); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	cp := *in.PublicAccessBlockConfiguration
	b.pab = &cp
	return &s3.PutPublicAccessBlockOutput{}, nil
}

func (f *fakeAWS) GetBucketEncryption(_ context.Context, in *s3.GetBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetBucketEncryption", false); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	if b.enc == nil {
		return nil, apiErr("ServerSideEncryptionConfigurationNotFoundError")
	}
	return &s3.GetBucketEncryptionOutput{ServerSideEncryptionConfiguration: b.enc}, nil
}

func (f *fakeAWS) PutBucketEncryption(_ context.Context, in *s3.PutBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutBucketEncryption", true); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	b.enc = in.ServerSideEncryptionConfiguration
	return &s3.PutBucketEncryptionOutput{}, nil
}

func (f *fakeAWS) GetBucketLifecycleConfiguration(_ context.Context, in *s3.GetBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetBucketLifecycleConfiguration", false); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	if len(b.lifecycle) == 0 {
		return nil, apiErr("NoSuchLifecycleConfiguration")
	}
	return &s3.GetBucketLifecycleConfigurationOutput{Rules: slices.Clone(b.lifecycle)}, nil
}

func (f *fakeAWS) PutBucketLifecycleConfiguration(_ context.Context, in *s3.PutBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutBucketLifecycleConfiguration", true); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	b.lifecycle = slices.Clone(in.LifecycleConfiguration.Rules)
	return &s3.PutBucketLifecycleConfigurationOutput{}, nil
}

func (f *fakeAWS) GetBucketVersioning(_ context.Context, in *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("GetBucketVersioning", false); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	return &s3.GetBucketVersioningOutput{Status: b.versioning}, nil
}

func (f *fakeAWS) PutBucketVersioning(_ context.Context, in *s3.PutBucketVersioningInput, _ ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("PutBucketVersioning", true); err != nil {
		return nil, err
	}
	b, err := f.bucketFor(in.Bucket)
	if err != nil {
		return nil, err
	}
	b.versioning = in.VersioningConfiguration.Status
	return &s3.PutBucketVersioningOutput{}, nil
}

// The real clients are checked by clientsFromConfig; this checks the fake.
var (
	_ STSAPI = (*fakeAWS)(nil)
	_ SESAPI = (*fakeAWS)(nil)
	_ SNSAPI = (*fakeAWS)(nil)
	_ IAMAPI = (*fakeAWS)(nil)
	_ S3API  = (*fakeAWS)(nil)
)
