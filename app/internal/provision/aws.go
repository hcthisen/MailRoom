package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// STSAPI is the part of the STS client provisioning uses.
type STSAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// SESAPI is the part of the SES v2 client provisioning uses.
type SESAPI interface {
	GetAccount(ctx context.Context, in *sesv2.GetAccountInput, opts ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error)
	PutAccountSuppressionAttributes(ctx context.Context, in *sesv2.PutAccountSuppressionAttributesInput, opts ...func(*sesv2.Options)) (*sesv2.PutAccountSuppressionAttributesOutput, error)
	PutAccountDetails(ctx context.Context, in *sesv2.PutAccountDetailsInput, opts ...func(*sesv2.Options)) (*sesv2.PutAccountDetailsOutput, error)

	GetEmailIdentity(ctx context.Context, in *sesv2.GetEmailIdentityInput, opts ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error)
	CreateEmailIdentity(ctx context.Context, in *sesv2.CreateEmailIdentityInput, opts ...func(*sesv2.Options)) (*sesv2.CreateEmailIdentityOutput, error)
	PutEmailIdentityDkimSigningAttributes(ctx context.Context, in *sesv2.PutEmailIdentityDkimSigningAttributesInput, opts ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityDkimSigningAttributesOutput, error)
	PutEmailIdentityMailFromAttributes(ctx context.Context, in *sesv2.PutEmailIdentityMailFromAttributesInput, opts ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityMailFromAttributesOutput, error)
	PutEmailIdentityConfigurationSetAttributes(ctx context.Context, in *sesv2.PutEmailIdentityConfigurationSetAttributesInput, opts ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityConfigurationSetAttributesOutput, error)

	GetConfigurationSet(ctx context.Context, in *sesv2.GetConfigurationSetInput, opts ...func(*sesv2.Options)) (*sesv2.GetConfigurationSetOutput, error)
	CreateConfigurationSet(ctx context.Context, in *sesv2.CreateConfigurationSetInput, opts ...func(*sesv2.Options)) (*sesv2.CreateConfigurationSetOutput, error)
	GetConfigurationSetEventDestinations(ctx context.Context, in *sesv2.GetConfigurationSetEventDestinationsInput, opts ...func(*sesv2.Options)) (*sesv2.GetConfigurationSetEventDestinationsOutput, error)
	CreateConfigurationSetEventDestination(ctx context.Context, in *sesv2.CreateConfigurationSetEventDestinationInput, opts ...func(*sesv2.Options)) (*sesv2.CreateConfigurationSetEventDestinationOutput, error)
	UpdateConfigurationSetEventDestination(ctx context.Context, in *sesv2.UpdateConfigurationSetEventDestinationInput, opts ...func(*sesv2.Options)) (*sesv2.UpdateConfigurationSetEventDestinationOutput, error)
}

// SNSAPI is the part of the SNS client provisioning uses.
type SNSAPI interface {
	GetTopicAttributes(ctx context.Context, in *sns.GetTopicAttributesInput, opts ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error)
	CreateTopic(ctx context.Context, in *sns.CreateTopicInput, opts ...func(*sns.Options)) (*sns.CreateTopicOutput, error)
	SetTopicAttributes(ctx context.Context, in *sns.SetTopicAttributesInput, opts ...func(*sns.Options)) (*sns.SetTopicAttributesOutput, error)
}

// IAMAPI is the part of the IAM client provisioning uses.
type IAMAPI interface {
	GetUser(ctx context.Context, in *iam.GetUserInput, opts ...func(*iam.Options)) (*iam.GetUserOutput, error)
	CreateUser(ctx context.Context, in *iam.CreateUserInput, opts ...func(*iam.Options)) (*iam.CreateUserOutput, error)
	GetUserPolicy(ctx context.Context, in *iam.GetUserPolicyInput, opts ...func(*iam.Options)) (*iam.GetUserPolicyOutput, error)
	PutUserPolicy(ctx context.Context, in *iam.PutUserPolicyInput, opts ...func(*iam.Options)) (*iam.PutUserPolicyOutput, error)
	ListAccessKeys(ctx context.Context, in *iam.ListAccessKeysInput, opts ...func(*iam.Options)) (*iam.ListAccessKeysOutput, error)
	CreateAccessKey(ctx context.Context, in *iam.CreateAccessKeyInput, opts ...func(*iam.Options)) (*iam.CreateAccessKeyOutput, error)
	DeleteAccessKey(ctx context.Context, in *iam.DeleteAccessKeyInput, opts ...func(*iam.Options)) (*iam.DeleteAccessKeyOutput, error)
}

// S3API is the part of the S3 client provisioning uses.
type S3API interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, in *s3.CreateBucketInput, opts ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	GetPublicAccessBlock(ctx context.Context, in *s3.GetPublicAccessBlockInput, opts ...func(*s3.Options)) (*s3.GetPublicAccessBlockOutput, error)
	PutPublicAccessBlock(ctx context.Context, in *s3.PutPublicAccessBlockInput, opts ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error)
	GetBucketEncryption(ctx context.Context, in *s3.GetBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error)
	PutBucketEncryption(ctx context.Context, in *s3.PutBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error)
	GetBucketLifecycleConfiguration(ctx context.Context, in *s3.GetBucketLifecycleConfigurationInput, opts ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error)
	PutBucketLifecycleConfiguration(ctx context.Context, in *s3.PutBucketLifecycleConfigurationInput, opts ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error)
	GetBucketVersioning(ctx context.Context, in *s3.GetBucketVersioningInput, opts ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
	PutBucketVersioning(ctx context.Context, in *s3.PutBucketVersioningInput, opts ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error)
	GetBucketPolicy(ctx context.Context, in *s3.GetBucketPolicyInput, opts ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error)
	PutBucketPolicy(ctx context.Context, in *s3.PutBucketPolicyInput, opts ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
}

// SESReceiptAPI is the part of the SES v1 client receiving uses: receipt
// rules exist only in the v1 API.
type SESReceiptAPI interface {
	DescribeActiveReceiptRuleSet(ctx context.Context, in *ses.DescribeActiveReceiptRuleSetInput, opts ...func(*ses.Options)) (*ses.DescribeActiveReceiptRuleSetOutput, error)
	CreateReceiptRuleSet(ctx context.Context, in *ses.CreateReceiptRuleSetInput, opts ...func(*ses.Options)) (*ses.CreateReceiptRuleSetOutput, error)
	DescribeReceiptRule(ctx context.Context, in *ses.DescribeReceiptRuleInput, opts ...func(*ses.Options)) (*ses.DescribeReceiptRuleOutput, error)
	CreateReceiptRule(ctx context.Context, in *ses.CreateReceiptRuleInput, opts ...func(*ses.Options)) (*ses.CreateReceiptRuleOutput, error)
	UpdateReceiptRule(ctx context.Context, in *ses.UpdateReceiptRuleInput, opts ...func(*ses.Options)) (*ses.UpdateReceiptRuleOutput, error)
	SetActiveReceiptRuleSet(ctx context.Context, in *ses.SetActiveReceiptRuleSetInput, opts ...func(*ses.Options)) (*ses.SetActiveReceiptRuleSetOutput, error)
}

// Clients bundles the AWS clients of one provisioning run.
type Clients struct {
	STS STSAPI
	SES SESAPI
	SNS SNSAPI
	IAM IAMAPI
	S3  S3API
	// Receipt is the SES v1 client for receipt rules (receiving).
	Receipt SESReceiptAPI
	// Resolver looks up DOMAIN's MX records; nil means net.DefaultResolver.
	Resolver MXResolver
	// Region is the AWS region every regional client talks to.
	Region string
}

// NewClients builds real AWS clients from the environment: AWS_REGION is
// required, and AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (plus the
// optional AWS_SESSION_TOKEN) are used as static credentials when set. For
// "provision ses" these are the setup user's keys, passed for this one run
// only. Without them the SDK's default credential chain applies.
func NewClients(ctx context.Context, getenv func(string) string) (Clients, error) {
	region := strings.TrimSpace(getenv("AWS_REGION"))
	if region == "" {
		return Clients{}, errors.New("AWS_REGION is required (for example eu-central-1)")
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	keyID := strings.TrimSpace(getenv("AWS_ACCESS_KEY_ID"))
	secret := strings.TrimSpace(getenv("AWS_SECRET_ACCESS_KEY"))
	switch {
	case keyID != "" && secret != "":
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(keyID, secret, strings.TrimSpace(getenv("AWS_SESSION_TOKEN")))))
	case keyID != "" || secret != "":
		return Clients{}, errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set together")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return Clients{}, fmt.Errorf("load AWS configuration: %w", err)
	}
	return clientsFromConfig(cfg), nil
}

func clientsFromConfig(cfg aws.Config) Clients {
	return Clients{
		STS:      sts.NewFromConfig(cfg),
		SES:      sesv2.NewFromConfig(cfg),
		SNS:      sns.NewFromConfig(cfg),
		IAM:      iam.NewFromConfig(cfg),
		S3:       s3.NewFromConfig(cfg),
		Receipt:  ses.NewFromConfig(cfg),
		Resolver: net.DefaultResolver,
		Region:   cfg.Region,
	}
}

// isAPIError reports whether err is an AWS API error with one of codes.
func isAPIError(err error, codes ...string) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.ErrorCode() == c {
			return true
		}
	}
	return false
}

// Error codes of "does not exist" answers, per service.
var (
	sesNotFound = []string{"NotFoundException"}
	snsNotFound = []string{"NotFound", "NotFoundException"}
	iamNotFound = []string{"NoSuchEntity", "NoSuchEntityException"}
	s3NotFound  = []string{"NotFound", "NoSuchBucket"}
	// SES v1 receipt rules.
	sesAlreadyExists = []string{"AlreadyExists", "AlreadyExistsException"}
	sesRuleNotFound  = []string{"RuleDoesNotExist", "RuleDoesNotExistException", "RuleSetDoesNotExist", "RuleSetDoesNotExistException"}
)
