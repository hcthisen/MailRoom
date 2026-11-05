package app

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"mailroom/internal/config"
	"mailroom/internal/core"
	"mailroom/internal/events"
	"mailroom/internal/inbound"
	"mailroom/internal/sending"
	"mailroom/internal/system"
)

// AWSClients are the AWS API clients Mailroom uses besides the SES sending
// provider. A nil field disables what needs it: the doctor's AWS checks
// fail with a fix hint, the SNS subscription (X-7) and the S3 backup upload
// are skipped.
type AWSClients struct {
	// SES answers the doctor's identity and account checks and the SES
	// status API.
	SES system.SESClient
	// SNS subscribes /hooks/ses to the topic (events) and lists the
	// topic's subscriptions (doctor). *sns.Client satisfies it.
	SNS events.SNSAPI
	// S3 uploads backups to BACKUP_BUCKET.
	S3 system.S3Client
	// Account feeds the ses-account-poller (sending). Nil means the SES
	// provider's own client when the provider is SES.
	Account sending.AccountAPI
	// Inbound fetches and deletes received mail in INBOUND_BUCKET. Nil
	// leaves received messages pending (no inbound-fetcher worker).
	Inbound inbound.ObjectStore
}

// NewAWSClients builds the clients for the sending user from cfg
// (AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY). Without a region
// it returns no clients and no error; without keys the SDK's default
// credential chain is used. It makes no network calls.
func NewAWSClients(ctx context.Context, cfg config.Config) (AWSClients, error) {
	if cfg.AWSRegion == "" {
		return AWSClients{}, nil
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.AWSRegion)}
	if cfg.AWSAccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, "")))
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return AWSClients{}, fmt.Errorf("load AWS configuration: %w", err)
	}
	s3c := s3.NewFromConfig(ac)
	return AWSClients{SES: sesv2.NewFromConfig(ac), SNS: sns.NewFromConfig(ac), S3: s3c, Inbound: s3c}, nil
}

// SNSClient returns c.SNS as the doctor's narrower interface, or nil.
func (c AWSClients) SNSClient() system.SNSClient {
	if c.SNS == nil {
		return nil
	}
	return c.SNS
}

// SESConfigured reports whether cfg holds the sending user's SES
// credentials, i.e. whether "mailroom provision ses" has run.
func SESConfigured(cfg config.Config) bool {
	return cfg.AWSRegion != "" && cfg.AWSAccessKeyID != "" && cfg.AWSSecretAccessKey != ""
}

// unconfiguredProvider is the email provider of an install without SES
// credentials. The sender worker isn't started then, so queued messages
// wait until the credentials are configured; this only guards direct
// calls (SendDue) with a retryable error.
type unconfiguredProvider struct{}

// Send implements core.EmailProvider.
func (unconfiguredProvider) Send(context.Context, core.OutgoingEmail) (string, error) {
	return "", &core.ProviderError{Retryable: true, Code: "SESNotConfigured",
		Message: "SES is not configured: run `mailroom provision ses`, then recreate the container (docker compose up -d)"}
}
