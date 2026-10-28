package sending

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"mailroom/internal/config"
	"mailroom/internal/core"
)

// CodeSendingPaused is the core.ProviderError code for "SES refuses to send
// for the whole account" (SendingPausedException, AccountSuspendedException).
// The sender pauses all sending when it sees it.
const CodeSendingPaused = "sending_paused"

// CodeCredentialsRejected is the core.ProviderError code for "SES does not
// accept the sending user's access key" (InvalidClientTokenId,
// UnrecognizedClientException, SignatureDoesNotMatch): every message would
// fail the same way, e.g. after "provision ses --rotate-keys" deleted the
// key this process still uses. The sender pauses all sending instead of
// failing the queue; the pause lifts when the ses-account-poller succeeds
// with working keys (after the container is recreated with the new env).
const CodeCredentialsRejected = "credentials_rejected"

// CodeDeliveryUnknown is the core.ProviderError code for "the provider
// call ended without a response after the request may have been sent"
// (e.g. a timeout). The message is failed, not retried: it may have been
// delivered, and delivery is at most once.
const CodeDeliveryUnknown = "delivery_unknown"

// sendAPI is the part of the SES v2 client the provider uses.
type sendAPI interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SESProvider sends raw MIME messages through the SES v2 SendEmail API
// (D-1) with the configured configuration set (D-3). It implements
// core.EmailProvider.
type SESProvider struct {
	api       sendAPI
	client    *sesv2.Client
	configSet string
}

var _ core.EmailProvider = (*SESProvider)(nil)

// NewSESProvider returns an SES provider using the sending user's static
// credentials (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) in AWS_REGION,
// and SES_CONFIGURATION_SET for every send. The SDK's own retries are
// disabled: the sender retries with its own backoff, and a transparent
// retry after a lost response could deliver a message twice.
func NewSESProvider(cfg config.Config) (*SESProvider, error) {
	if cfg.AWSRegion == "" {
		return nil, errors.New("sending: AWS_REGION is required for SES")
	}
	if cfg.AWSAccessKeyID == "" || cfg.AWSSecretAccessKey == "" {
		return nil, errors.New("sending: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required for SES")
	}
	awsCfg := aws.Config{
		Region: cfg.AWSRegion,
		Credentials: aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, "")),
	}
	client := sesv2.NewFromConfig(awsCfg, func(o *sesv2.Options) {
		o.Retryer = aws.NopRetryer{}
	})
	return &SESProvider{api: client, client: client, configSet: cfg.SESConfigurationSet}, nil
}

// AccountAPI returns the SES client for the ses-account-poller.
func (p *SESProvider) AccountAPI() AccountAPI { return p.client }

// Send sends m.Raw to m.To. The provider message ID is the SES MessageId.
// Errors are *core.ProviderError (see classifySESError).
func (p *SESProvider) Send(ctx context.Context, m core.OutgoingEmail) (string, error) {
	// Raw MIME already contains the validated, encoded From header. Setting
	// FromEmailAddress to the bare envelope address would replace its display
	// name. Leave this optional API field unset so the MIME sender is used.
	in := &sesv2.SendEmailInput{
		Destination: &types.Destination{ToAddresses: []string{m.To}},
		Content:     &types.EmailContent{Raw: &types.RawMessage{Data: m.Raw}},
		EmailTags:   sesTags(m.Tags),
	}
	if p.configSet != "" {
		in.ConfigurationSetName = aws.String(p.configSet)
	}
	out, err := p.api.SendEmail(ctx, in)
	if err != nil {
		return "", classifySESError(err)
	}
	if out == nil || out.MessageId == nil || *out.MessageId == "" {
		return "", &core.ProviderError{Code: "NoMessageId", Message: "SES accepted the request but returned no MessageId"}
	}
	return *out.MessageId, nil
}

// sesTags converts message tags to SES tags. SES allows only
// [A-Za-z0-9_-] (max 256) in names and values; other characters become
// "_", and empty names are dropped. Tags are sorted by name.
func sesTags(tags map[string]string) []types.MessageTag {
	names := make([]string, 0, len(tags))
	for k := range tags {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]types.MessageTag, 0, len(names))
	for _, k := range names {
		name, value := sanitizeTag(k), sanitizeTag(tags[k])
		if name == "" {
			continue
		}
		if value == "" {
			value = "none"
		}
		out = append(out, types.MessageTag{Name: aws.String(name), Value: aws.String(value)})
	}
	return out
}

func sanitizeTag(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}

// apiError is satisfied by smithy API errors (the SES exception types and
// smithy.GenericAPIError).
type apiError interface {
	error
	ErrorCode() string
	ErrorMessage() string
}

// httpStatusError is satisfied by the SDK's HTTP response errors.
type httpStatusError interface {
	error
	HTTPStatusCode() int
}

// SES error codes by class.
var (
	sesPauseCodes = map[string]bool{
		"SendingPausedException":    true,
		"AccountSuspendedException": true,
	}
	sesPermanentCodes = map[string]bool{
		"MessageRejected":                    true,
		"MailFromDomainNotVerifiedException": true,
		"NotFoundException":                  true,
		"BadRequestException":                true,
		"InvalidParameterValue":              true,
		"ValidationException":                true,
		"AccessDeniedException":              true,
	}
	sesCredentialCodes = map[string]bool{
		"UnrecognizedClientException": true,
		"InvalidClientTokenId":        true,
		"SignatureDoesNotMatch":       true,
	}
	sesRetryableCodes = map[string]bool{
		"TooManyRequestsException":        true,
		"LimitExceededException":          true,
		"Throttling":                      true,
		"ThrottlingException":             true,
		"InternalServiceErrorException":   true,
		"InternalFailure":                 true,
		"ServiceUnavailable":              true,
		"ConcurrentModificationException": true,
		"RequestTimeout":                  true,
		"RequestTimeoutException":         true,
	}
)

// classifySESError turns an SDK error into a *core.ProviderError:
//
//   - SendingPaused / AccountSuspended: permanent, Code CodeSendingPaused
//     (the sender pauses all sending and re-queues the message);
//   - a rejected access key (InvalidClientTokenId, UnrecognizedClient,
//     SignatureDoesNotMatch): Code CodeCredentialsRejected, handled like a
//     pause;
//   - MessageRejected, MailFromDomainNotVerified, NotFound, BadRequest and
//     other client errors: permanent;
//   - throttling, quota, 5xx and 429: retryable;
//   - no API response, and the request was never sent (DNS, dial or
//     certificate error): retryable, Code "NetworkError";
//   - no API response after the request may have been sent (timeout,
//     connection reset, EOF): permanent, Code CodeDeliveryUnknown. SES may
//     have accepted the message, and retrying it could deliver it twice
//     (delivery is at most once).
func classifySESError(err error) error {
	if err == nil {
		return nil
	}
	var pe *core.ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	var ae apiError
	if errors.As(err, &ae) {
		code, msg := ae.ErrorCode(), ae.ErrorMessage()
		switch {
		case sesPauseCodes[code]:
			return &core.ProviderError{Code: CodeSendingPaused, Message: fmt.Sprintf("%s: %s", code, msg)}
		case sesCredentialCodes[code]:
			return &core.ProviderError{Code: CodeCredentialsRejected, Message: fmt.Sprintf("%s: %s", code, msg)}
		case sesPermanentCodes[code]:
			return &core.ProviderError{Code: code, Message: msg}
		case sesRetryableCodes[code]:
			return &core.ProviderError{Retryable: true, Code: code, Message: msg}
		}
		if status, ok := httpStatus(err); ok {
			return &core.ProviderError{Retryable: retryableStatus(status), Code: code, Message: msg}
		}
		return &core.ProviderError{Code: code, Message: msg}
	}
	if status, ok := httpStatus(err); ok {
		return &core.ProviderError{Retryable: retryableStatus(status), Code: fmt.Sprintf("HTTP%d", status), Message: err.Error()}
	}
	if requestNotSent(err) {
		return &core.ProviderError{Retryable: true, Code: "NetworkError", Message: err.Error()}
	}
	return &core.ProviderError{Code: CodeDeliveryUnknown,
		Message: "no response from SES; the message may have been delivered: " + err.Error()}
}

// requestNotSent reports whether err shows that the request never left
// this host: name resolution, connecting (also through a proxy) or
// verifying the server certificate failed. Only then is a retry safe.
func requestNotSent(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "proxyconnect")
}

func httpStatus(err error) (int, bool) {
	var he httpStatusError
	if errors.As(err, &he) {
		return he.HTTPStatusCode(), true
	}
	return 0, false
}

func retryableStatus(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout
}
