package sending

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"syscall"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"mailroom/internal/config"
	"mailroom/internal/core"
)

// sdkError wraps err the way the SDK does: OperationError -> ResponseError
// (with the HTTP status) -> the API error.
func sdkError(status int, err error) error {
	return &smithy.OperationError{
		ServiceID: "SESv2", OperationName: "SendEmail",
		Err: &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
				Err:      err,
			},
			RequestID: "req-1",
		},
	}
}

func str(s string) *string { return &s }

func TestClassifySESError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      string
		retryable bool
	}{
		{"message rejected", sdkError(400, &types.MessageRejected{Message: str("Email address is not verified")}), "MessageRejected", false},
		{"mail from not verified", sdkError(400, &types.MailFromDomainNotVerifiedException{}), "MailFromDomainNotVerifiedException", false},
		{"not found", sdkError(404, &types.NotFoundException{}), "NotFoundException", false},
		{"bad request", sdkError(400, &types.BadRequestException{Message: str("bad")}), "BadRequestException", false},
		{"sending paused", sdkError(400, &types.SendingPausedException{Message: str("paused")}), CodeSendingPaused, false},
		{"account suspended", sdkError(400, &types.AccountSuspendedException{}), CodeSendingPaused, false},
		{"key deleted", sdkError(403, &smithy.GenericAPIError{Code: "InvalidClientTokenId", Message: "The security token included in the request is invalid."}), CodeCredentialsRejected, false},
		{"unknown key", sdkError(403, &smithy.GenericAPIError{Code: "UnrecognizedClientException"}), CodeCredentialsRejected, false},
		{"wrong secret", sdkError(403, &smithy.GenericAPIError{Code: "SignatureDoesNotMatch"}), CodeCredentialsRejected, false},
		{"too many requests", sdkError(429, &types.TooManyRequestsException{}), "TooManyRequestsException", true},
		{"daily quota", sdkError(400, &types.LimitExceededException{}), "LimitExceededException", true},
		{"internal", sdkError(500, &types.InternalServiceErrorException{}), "InternalServiceErrorException", true},
		{"throttling generic", sdkError(400, &smithy.GenericAPIError{Code: "Throttling", Message: "Rate exceeded"}), "Throttling", true},
		{"unknown code 503", sdkError(503, &smithy.GenericAPIError{Code: "Weird"}), "Weird", true},
		{"unknown code 400", sdkError(400, &smithy.GenericAPIError{Code: "Weird"}), "Weird", false},
		{"unknown code no status", &smithy.GenericAPIError{Code: "Weird"}, "Weird", false},
		{"http 502 without API error", sdkError(502, errors.New("bad gateway")), "HTTP502", true},
		{"http 403 without API error", sdkError(403, errors.New("forbidden")), "HTTP403", false},
		// Never sent: safe to retry.
		{"dial", &smithy.OperationError{Err: &smithyhttp.RequestSendError{Err: &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}}}, "NetworkError", true},
		{"dns", &smithy.OperationError{Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "email.eu-west-1.amazonaws.com"}}}, "NetworkError", true},
		{"dns bare", &net.DNSError{Err: "no such host"}, "NetworkError", true},
		{"proxy", &net.OpError{Op: "proxyconnect", Err: errors.New("refused")}, "NetworkError", true},
		{"certificate", &url.Error{Err: &tls.CertificateVerificationError{Err: errors.New("x509: unknown authority")}}, "NetworkError", true},
		// Possibly sent, no response: never retried (at most once).
		{"timeout", context.DeadlineExceeded, CodeDeliveryUnknown, false},
		{"wrapped timeout", &smithy.OperationError{Err: &smithyhttp.RequestSendError{Err: &url.Error{Op: "Post", Err: context.DeadlineExceeded}}}, CodeDeliveryUnknown, false},
		{"reset after write", &smithy.OperationError{Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}, CodeDeliveryUnknown, false},
		{"eof", &url.Error{Op: "Post", Err: io.EOF}, CodeDeliveryUnknown, false},
		{"already classified", fmt.Errorf("x: %w", &core.ProviderError{Code: "C", Retryable: true}), "C", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pe *core.ProviderError
			if !errors.As(classifySESError(tt.err), &pe) {
				t.Fatal("not a ProviderError")
			}
			if pe.Code != tt.code || pe.Retryable != tt.retryable {
				t.Fatalf("got %+v, want code %s retryable %v", pe, tt.code, tt.retryable)
			}
		})
	}
	if classifySESError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

type fakeSES struct {
	in  *sesv2.SendEmailInput
	out *sesv2.SendEmailOutput
	err error
}

func (f *fakeSES) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.in = in
	return f.out, f.err
}

func TestSESProviderSend(t *testing.T) {
	f := &fakeSES{out: &sesv2.SendEmailOutput{MessageId: str("0102-abc")}}
	p := &SESProvider{api: f, configSet: "mailroom"}
	id, err := p.Send(context.Background(), core.OutgoingEmail{
		From: "news@example.com", To: "alice@example.org", ReplyTo: "hello@example.com", Raw: []byte("raw"),
		Tags: map[string]string{"kind": "broadcast", "message_id": "42", "bad name!": "a.b c", "empty": ""},
	})
	if err != nil || id != "0102-abc" {
		t.Fatalf("Send = %q %v", id, err)
	}
	in := f.in
	if in.FromEmailAddress != nil || in.Destination.ToAddresses[0] != "alice@example.org" ||
		string(in.Content.Raw.Data) != "raw" || *in.ConfigurationSetName != "mailroom" {
		t.Fatalf("input = %+v", in)
	}
	got := map[string]string{}
	for _, tag := range in.EmailTags {
		got[*tag.Name] = *tag.Value
	}
	want := map[string]string{"kind": "broadcast", "message_id": "42", "bad_name_": "a_b_c", "empty": "none"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}

	f.err = sdkError(400, &types.MessageRejected{Message: str("nope")})
	_, err = p.Send(context.Background(), core.OutgoingEmail{From: "a@example.com", To: "b@example.org"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Code != "MessageRejected" || pe.Retryable {
		t.Fatalf("err = %v", err)
	}
	f.err, f.out = nil, &sesv2.SendEmailOutput{}
	if _, err := p.Send(context.Background(), core.OutgoingEmail{}); !errors.As(err, &pe) || pe.Code != "NoMessageId" {
		t.Fatalf("missing MessageId: %v", err)
	}
}

// SES must use the MIME From, including its encoded display name, rather
// than an API-level bare address overriding the sender the user selected.
func TestSESProviderPreservesRawSender(t *testing.T) {
	for _, name := range []string{"Titan Claws", "Café Ünïcode"} {
		t.Run(name, func(t *testing.T) {
			raw, err := buildMIME(mimeMessage{
				FromName: name, From: "news@example.com", To: "alice@example.org", ReplyTo: "hello@example.com",
				Subject: "News", Text: "Hello", HTML: "<p>Hello</p>",
				Date: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), MessageID: "test@example.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeSES{out: &sesv2.SendEmailOutput{MessageId: str("raw-sender")}}
			p := &SESProvider{api: f, configSet: "mailroom"}
			_, err = p.Send(context.Background(), core.OutgoingEmail{
				From: "news@example.com", To: "alice@example.org", ReplyTo: "hello@example.com", Raw: raw,
				Tags: map[string]string{"kind": "broadcast", "message_id": "42"},
			})
			if err != nil {
				t.Fatal(err)
			}
			in := f.in
			if in.FromEmailAddress != nil || len(in.ReplyToAddresses) != 0 {
				t.Fatal("API fields must not override raw sender or reply-to headers")
			}
			if !bytes.Equal(in.Content.Raw.Data, raw) || len(in.Destination.ToAddresses) != 1 ||
				in.Destination.ToAddresses[0] != "alice@example.org" || *in.ConfigurationSetName != "mailroom" {
				t.Fatal("raw message, destination or configuration set changed")
			}
			from, err := mail.ParseAddress(parseMessage(t, in.Content.Raw.Data).Header.Get("From"))
			if err != nil || from.Name != name || from.Address != "news@example.com" {
				t.Fatalf("MIME sender = %+v, %v", from, err)
			}
			if len(in.EmailTags) != 2 || *in.EmailTags[0].Name != "kind" || *in.EmailTags[0].Value != "broadcast" ||
				*in.EmailTags[1].Name != "message_id" || *in.EmailTags[1].Value != "42" {
				t.Fatalf("message correlation tags = %+v", in.EmailTags)
			}
		})
	}
}

func TestNewSESProvider(t *testing.T) {
	if _, err := NewSESProvider(config.Config{AWSAccessKeyID: "k", AWSSecretAccessKey: "s"}); err == nil {
		t.Fatal("region required")
	}
	if _, err := NewSESProvider(config.Config{AWSRegion: "eu-west-1"}); err == nil {
		t.Fatal("credentials required")
	}
	p, err := NewSESProvider(config.Config{AWSRegion: "eu-west-1", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "secret", SESConfigurationSet: "mailroom"})
	if err != nil || p.AccountAPI() == nil || p.configSet != "mailroom" {
		t.Fatalf("provider = %+v %v", p, err)
	}
}
