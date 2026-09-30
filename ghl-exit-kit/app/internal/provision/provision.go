package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Action is what a provisioning step did.
type Action string

// Step actions.
const (
	ActionCreated   Action = "created"
	ActionUpdated   Action = "updated"
	ActionUnchanged Action = "unchanged"
)

// Step is one reported provisioning step.
type Step struct {
	Name   string `json:"name"`
	Action Action `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// SESInput configures ProvisionSES.
type SESInput struct {
	// Domain is the one sending domain (DOMAIN).
	Domain string
	// MailFromSubdomain is the custom MAIL FROM label (MAIL_FROM_SUBDOMAIN).
	MailFromSubdomain string
	// WebhookURL is the one endpoint the sending user may subscribe to the
	// event topic (see WebhookURL); default https://newsletter.DOMAIN/hooks/ses.
	WebhookURL string
	// ConfigurationSet defaults to DefaultConfigurationSet.
	ConfigurationSet string
	// EnvFile is the server env file to merge the sending keys into.
	EnvFile string
	// BackupBucket creates and configures the S3 backup bucket.
	BackupBucket bool
	// RotateKeys replaces the sending user's access key: a new key is
	// created and written to the env file, then the old keys are deleted.
	RotateKeys bool
	// OnStep, when set, is called as each step finishes.
	OnStep func(Step)
}

// Result is the outcome of ProvisionSES. Secrets are never part of it.
type Result struct {
	AccountID               string   `json:"account_id"`
	Region                  string   `json:"region"`
	Domain                  string   `json:"domain"`
	IdentityARN             string   `json:"identity_arn"`
	ConfigurationSet        string   `json:"configuration_set"`
	ConfigurationSetARN     string   `json:"configuration_set_arn"`
	TopicARN                string   `json:"topic_arn"`
	BackupBucket            string   `json:"backup_bucket,omitempty"`
	IAMUser                 string   `json:"iam_user"`
	AccessKeyID             string   `json:"access_key_id,omitempty"`
	EnvFile                 string   `json:"env_file"`
	Steps                   []Step   `json:"steps"`
	IdentityVerified        bool     `json:"identity_verified"`
	DKIMStatus              string   `json:"dkim_status,omitempty"`
	MailFromStatus          string   `json:"mail_from_status,omitempty"`
	ProductionAccessEnabled bool     `json:"production_access_enabled"`
	SendingEnabled          bool     `json:"sending_enabled"`
	Records                 []Record `json:"dns_records"`
	// Warnings are caveats for the person running the setup.
	Warnings []string `json:"warnings"`
}

// Provisioner runs provisioning against a set of AWS clients.
type Provisioner struct {
	Clients Clients
}

// New returns a Provisioner using c.
func New(c Clients) *Provisioner { return &Provisioner{Clients: c} }

// run is the state of one ProvisionSES call.
type run struct {
	c    Clients
	in   SESInput
	res  *Result
	arns arns

	ident      *sesv2.GetEmailIdentityOutput // the identity as found (zero when just created)
	bucketName string                        // the backup bucket, "" when disabled
}

func (r *run) step(name string, a Action, detail string) {
	s := Step{Name: name, Action: a, Detail: detail}
	r.res.Steps = append(r.res.Steps, s)
	if r.in.OnStep != nil {
		r.in.OnStep(s)
	}
}

// ProvisionSES creates or converges every AWS resource Mailroom needs
// (L-1) and writes the sending user's configuration to the env file. It is
// idempotent: a second run reports every step unchanged. On error the
// returned Result holds the steps completed so far.
func (p *Provisioner) ProvisionSES(ctx context.Context, in SESInput) (Result, error) {
	in.Domain = normalizeDomain(in.Domain)
	if err := validateDomain(in.Domain); err != nil {
		return Result{}, err
	}
	if in.MailFromSubdomain == "" {
		return Result{}, errors.New("MAIL_FROM_SUBDOMAIN is required")
	}
	if !validLabel(in.MailFromSubdomain) {
		return Result{}, fmt.Errorf("MAIL_FROM_SUBDOMAIN %q is not a valid DNS label", in.MailFromSubdomain)
	}
	if in.ConfigurationSet == "" {
		in.ConfigurationSet = DefaultConfigurationSet
	}
	if in.WebhookURL == "" {
		in.WebhookURL = WebhookURL("", "newsletter", in.Domain)
	}
	if in.EnvFile == "" {
		in.EnvFile = DefaultEnvFile
	}
	if p.Clients.Region == "" {
		return Result{}, errors.New("AWS region is not set")
	}
	res := Result{
		Region:           p.Clients.Region,
		Domain:           in.Domain,
		ConfigurationSet: in.ConfigurationSet,
		IAMUser:          SenderUserName,
		EnvFile:          in.EnvFile,
		Steps:            []Step{},
		Warnings:         []string{},
	}
	// Fail before any AWS change when the keys could not be stored: a
	// .env the container user can't read and write (wrong owner) would
	// otherwise stop the run only after every resource was created.
	if err := CheckEnvFileWritable(in.EnvFile); err != nil {
		return res, err
	}
	r := &run{c: p.Clients, in: in, res: &res}
	err := r.provision(ctx)
	return res, err
}

func (r *run) provision(ctx context.Context) error {
	for _, f := range []func(context.Context) error{
		r.callerIdentity,
		r.identity,
		r.suppression,
		r.configurationSet,
		r.topic,
		r.eventDestination,
		r.identityConfigurationSet,
		r.bucket,
		r.iamUser,
		r.iamPolicy,
		r.accessKeyAndEnv,
		r.finalState,
	} {
		if err := f(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) callerIdentity(ctx context.Context) error {
	out, err := r.c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("sts get-caller-identity: %w", err)
	}
	account := aws.ToString(out.Account)
	if account == "" {
		return errors.New("sts get-caller-identity: no account in the answer")
	}
	r.arns = arns{
		partition: partitionOf(aws.ToString(out.Arn)),
		region:    r.c.Region,
		account:   account,
		domain:    r.in.Domain,
		configSet: r.in.ConfigurationSet,
	}
	r.res.AccountID = account
	r.res.IdentityARN = r.arns.identity()
	r.res.ConfigurationSetARN = r.arns.configurationSet()
	r.res.TopicARN = r.arns.topic()
	r.step("aws account", ActionUnchanged, fmt.Sprintf("account %s, region %s, as %s", account, r.c.Region, aws.ToString(out.Arn)))
	return nil
}

func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
}

func validateDomain(d string) error {
	if d == "" {
		return errors.New("DOMAIN is required")
	}
	if !strings.Contains(d, ".") || len(d) > 253 {
		return fmt.Errorf("DOMAIN %q is not a valid domain name", d)
	}
	for _, l := range strings.Split(d, ".") {
		if !validLabel(l) {
			return fmt.Errorf("DOMAIN %q is not a valid domain name", d)
		}
	}
	return nil
}

// validLabel reports whether s is a DNS label: 1-63 letters, digits or
// hyphens, not starting or ending with a hyphen.
func validLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// finalState reads back the identity and the account for the result.
func (r *run) finalState(ctx context.Context) error {
	ident, err := r.c.SES.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(r.in.Domain)})
	if err != nil {
		return fmt.Errorf("ses get-email-identity: %w", err)
	}
	acct, err := r.c.SES.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil {
		return fmt.Errorf("ses get-account: %w", err)
	}
	r.res.IdentityVerified = ident.VerifiedForSendingStatus
	var tokens []string
	var hostedZone string
	if d := ident.DkimAttributes; d != nil {
		r.res.DKIMStatus = string(d.Status)
		tokens = d.Tokens
		hostedZone = aws.ToString(d.SigningHostedZone)
	}
	if m := ident.MailFromAttributes; m != nil {
		r.res.MailFromStatus = string(m.MailFromDomainStatus)
	}
	r.res.ProductionAccessEnabled = acct.ProductionAccessEnabled
	r.res.SendingEnabled = acct.SendingEnabled
	if !acct.ProductionAccessEnabled {
		r.res.Warnings = append(r.res.Warnings, sandboxWarning(r.in.Domain))
	}
	if !acct.SendingEnabled {
		r.res.Warnings = append(r.res.Warnings, "sending is disabled for this SES account")
	}
	r.res.Records = Records(RecordsInput{
		Domain:            r.in.Domain,
		MailFromSubdomain: r.in.MailFromSubdomain,
		Region:            r.c.Region,
		DKIMTokens:        tokens,
		DKIMHostedZone:    hostedZone,
	})
	return nil
}

// sandboxWarning explains the sandbox limits of the sending user. While the
// account is in the SES sandbox, SES also authorizes each send against the
// recipient's verified identity, and the mailroom-sender policy covers only
// DOMAIN (PRD §11), so test sends to verified addresses elsewhere fail.
func sandboxWarning(domain string) string {
	return fmt.Sprintf("the account is in the SES sandbox: until production access is enabled, test only with the SES mailbox simulator "+
		"and addresses on %s. SES also checks the sender's permission on each sandbox recipient's verified identity, and "+
		"%s may send only on %s, so sends to verified addresses on other domains fail with \"not authorized to perform ses:SendRawEmail\"",
		domain, SenderUserName, domain)
}
