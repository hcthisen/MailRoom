package system

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"mailroom/internal/config"
	"mailroom/internal/core"
)

// SESClient is the part of the SES v2 API the system package uses. It is
// satisfied by *sesv2.Client.
type SESClient interface {
	GetAccount(ctx context.Context, in *sesv2.GetAccountInput, optFns ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error)
	GetEmailIdentity(ctx context.Context, in *sesv2.GetEmailIdentityInput, optFns ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error)
}

// SNSClient is the part of the SNS API the system package uses. It is
// satisfied by *sns.Client.
type SNSClient interface {
	ListSubscriptionsByTopic(ctx context.Context, in *sns.ListSubscriptionsByTopicInput, optFns ...func(*sns.Options)) (*sns.ListSubscriptionsByTopicOutput, error)
}

// S3Client is the part of the S3 API the system package uses (backup
// upload). It is satisfied by *s3.Client.
type S3Client interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

var (
	_ SESClient = (*sesv2.Client)(nil)
	_ SNSClient = (*sns.Client)(nil)
	_ S3Client  = (*s3.Client)(nil)
)

// Resolver answers the doctor's DNS lookups. It is satisfied by
// *net.Resolver.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

var _ Resolver = (*net.Resolver)(nil)

// NewDefaultResolver returns the system DNS resolver.
func NewDefaultResolver() Resolver { return net.DefaultResolver }

// CertChecker connects to host:443, verifies the served certificate chain
// for host and returns the leaf certificate's expiry.
type CertChecker func(ctx context.Context, host string) (notAfter time.Time, err error)

// NewTLSChecker returns a CertChecker that dials host:443 with a 5 second
// timeout and verifies the chain against the system roots at clock's time.
func NewTLSChecker(clock core.Clock) CertChecker {
	if clock == nil {
		clock = core.SystemClock{}
	}
	return func(ctx context.Context, host string) (time.Time, error) {
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 5 * time.Second},
			Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, Time: clock.Now},
		}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
		if err != nil {
			return time.Time{}, err
		}
		defer conn.Close()
		tc, ok := conn.(*tls.Conn)
		if !ok {
			return time.Time{}, errors.New("not a TLS connection")
		}
		certs := tc.ConnectionState().PeerCertificates
		if len(certs) == 0 {
			return time.Time{}, errors.New("the server sent no certificate")
		}
		return certs[0].NotAfter, nil
	}
}

// HTTPDoer sends HTTP requests; *http.Client satisfies it. The doctor uses
// it only for the clock-skew check.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewDefaultHTTPClient returns the HTTP client for the clock-skew check: a
// 5 second timeout and no redirects.
func NewDefaultHTTPClient() HTTPDoer {
	return &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// AWSClients are the AWS API clients built from the configuration. A field
// is nil when AWS isn't configured, so they can be copied into Deps as is.
type AWSClients struct {
	SES SESClient
	SNS SNSClient
	S3  S3Client
}

// NewAWSClients builds the SES, SNS and S3 clients for the sending user
// (AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY). Without a region
// it returns empty clients and no error: the AWS checks then fail with a fix
// hint. Without keys the SDK's default credential chain is used. It makes
// no network calls.
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
		return AWSClients{}, fmt.Errorf("system: load AWS configuration: %w", err)
	}
	return newAWSClients(ac), nil
}

func newAWSClients(ac aws.Config) AWSClients {
	return AWSClients{SES: sesv2.NewFromConfig(ac), SNS: sns.NewFromConfig(ac), S3: s3.NewFromConfig(ac)}
}
