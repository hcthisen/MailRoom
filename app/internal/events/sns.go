package events

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // SNS SignatureVersion 1 is SHA1WithRSA
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SNS message types (the x-amz-sns-message-type header and the Type field).
const (
	TypeNotification             = "Notification"
	TypeSubscriptionConfirmation = "SubscriptionConfirmation"
	TypeUnsubscribeConfirmation  = "UnsubscribeConfirmation"
)

// SNSMessageTypeHeader is the header SNS sets on every HTTP delivery.
const SNSMessageTypeHeader = "x-amz-sns-message-type"

// Envelope is an SNS HTTP(S) message.
type Envelope struct {
	Type             string  `json:"Type"`
	MessageID        string  `json:"MessageId"`
	Token            string  `json:"Token,omitempty"`
	TopicArn         string  `json:"TopicArn"`
	Subject          *string `json:"Subject,omitempty"`
	Message          string  `json:"Message"`
	Timestamp        string  `json:"Timestamp"`
	SignatureVersion string  `json:"SignatureVersion"`
	Signature        string  `json:"Signature"`
	SigningCertURL   string  `json:"SigningCertURL"`
	SubscribeURL     string  `json:"SubscribeURL,omitempty"`
	UnsubscribeURL   string  `json:"UnsubscribeURL,omitempty"`
}

// errMalformed marks an SNS message that is not valid JSON or lacks
// required fields (400).
var errMalformed = errors.New("malformed SNS message")

// errSignature marks a message whose signature, certificate URL or
// certificate is not acceptable (403).
var errSignature = errors.New("invalid SNS signature")

// errCertFetch marks a transient failure to fetch the signing certificate
// (503, so SNS retries).
var errCertFetch = errors.New("fetch SNS signing certificate")

// parseEnvelope decodes and checks the fields every message type needs.
func parseEnvelope(body []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(body, &e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", errMalformed, err)
	}
	switch e.Type {
	case TypeNotification:
	case TypeSubscriptionConfirmation, TypeUnsubscribeConfirmation:
		if e.Token == "" || e.SubscribeURL == "" {
			return Envelope{}, fmt.Errorf("%w: %s without Token or SubscribeURL", errMalformed, e.Type)
		}
	default:
		return Envelope{}, fmt.Errorf("%w: unknown Type %q", errMalformed, e.Type)
	}
	if e.MessageID == "" || e.TopicArn == "" || e.Timestamp == "" || e.Signature == "" || e.SigningCertURL == "" {
		return Envelope{}, fmt.Errorf("%w: missing required fields", errMalformed)
	}
	return e, nil
}

// StringToSign returns the canonical string SNS signs for e: "Key\nValue\n"
// pairs in the fixed order of its message type. Notification: Message,
// MessageId, Subject (only when present), Timestamp, TopicArn, Type.
// SubscriptionConfirmation and UnsubscribeConfirmation: Message, MessageId,
// SubscribeURL, Timestamp, Token, TopicArn, Type.
func StringToSign(e Envelope) string {
	var b strings.Builder
	add := func(k, v string) {
		b.WriteString(k)
		b.WriteByte('\n')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	add("Message", e.Message)
	add("MessageId", e.MessageID)
	if e.Type == TypeNotification {
		if e.Subject != nil {
			add("Subject", *e.Subject)
		}
		add("Timestamp", e.Timestamp)
		add("TopicArn", e.TopicArn)
		add("Type", e.Type)
		return b.String()
	}
	add("SubscribeURL", e.SubscribeURL)
	add("Timestamp", e.Timestamp)
	add("Token", e.Token)
	add("TopicArn", e.TopicArn)
	add("Type", e.Type)
	return b.String()
}

// certHostPattern matches the SNS endpoints that serve signing certificates.
var certHostPattern = regexp.MustCompile(`^sns\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`)

// ValidCertURL reports whether u is an acceptable SigningCertURL: https,
// no credentials or port, a host on sns.<region>.amazonaws.com(.cn) and a
// path ending in .pem.
func ValidCertURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.User != nil || p.Port() != "" || p.RawQuery != "" || p.Fragment != "" {
		return false
	}
	return certHostPattern.MatchString(p.Hostname()) && strings.HasSuffix(p.Path, ".pem")
}

// verify checks e's signature with the certificate at SigningCertURL.
func (s *Service) verify(ctx context.Context, e Envelope) error {
	var (
		hash crypto.Hash
		sum  []byte
	)
	msg := []byte(StringToSign(e))
	switch e.SignatureVersion {
	case "1":
		h := sha1.Sum(msg) //nolint:gosec // mandated by SignatureVersion 1
		hash, sum = crypto.SHA1, h[:]
	case "2":
		h := sha256.Sum256(msg)
		hash, sum = crypto.SHA256, h[:]
	default:
		return fmt.Errorf("%w: unsupported SignatureVersion %q", errSignature, e.SignatureVersion)
	}
	sig, err := base64.StdEncoding.DecodeString(e.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not base64", errSignature)
	}
	if !ValidCertURL(e.SigningCertURL) {
		return fmt.Errorf("%w: SigningCertURL %q is not an SNS certificate URL", errSignature, e.SigningCertURL)
	}
	// SNS signs with a certificate served by the topic's own region, so
	// certificates of every other region are refused before any fetch.
	if u, _ := url.Parse(e.SigningCertURL); u.Hostname() != snsHost(s.region()) {
		return fmt.Errorf("%w: SigningCertURL %q is not in the topic's region %q", errSignature, e.SigningCertURL, s.region())
	}
	key, err := s.signingKey(ctx, e.SigningCertURL)
	if err != nil {
		return err
	}
	if err := rsa.VerifyPKCS1v15(key, hash, sum, sig); err != nil {
		return fmt.Errorf("%w: %v", errSignature, err)
	}
	return nil
}

// maxCachedCerts bounds the certificate cache; SNS uses a handful.
const maxCachedCerts = 32

// maxCertBytes bounds a fetched certificate.
const maxCertBytes = 64 << 10

// badCertTTL is how long a certificate URL that answered with something
// other than a usable certificate is refused without fetching it again.
const badCertTTL = 10 * time.Minute

// maxCertFetches bounds concurrent certificate fetches; further requests
// wait for a slot (or their context) instead of opening more connections.
const maxCertFetches = 4

// certCache holds parsed signing keys by certificate URL, and remembers
// URLs that did not yield a certificate.
type certCache struct {
	mu    sync.Mutex
	max   int
	keys  map[string]*rsa.PublicKey
	bad   map[string]time.Time // URL -> refused until
	slots chan struct{}
}

func newCertCache(max int) *certCache {
	return &certCache{
		max:   max,
		keys:  make(map[string]*rsa.PublicKey),
		bad:   make(map[string]time.Time),
		slots: make(chan struct{}, maxCertFetches),
	}
}

func (c *certCache) get(u string) (*rsa.PublicKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.keys[u]
	return k, ok
}

func (c *certCache) put(u string, k *rsa.PublicKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) >= c.max {
		clear(c.keys)
	}
	c.keys[u] = k
	delete(c.bad, u)
}

// refused reports whether u failed recently.
func (c *certCache) refused(u string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.bad[u]
	if ok && !now.Before(until) {
		delete(c.bad, u)
		return false
	}
	return ok
}

// markBad refuses u for badCertTTL.
func (c *certCache) markBad(u string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bad) >= c.max {
		clear(c.bad)
	}
	c.bad[u] = now.Add(badCertTTL)
}

// signingKey returns the RSA key of the certificate at certURL, fetching
// and caching it on first use. Only successfully parsed keys are cached;
// a URL that answered with no usable certificate is refused for
// badCertTTL, and transient fetch failures (errCertFetch) are retried.
func (s *Service) signingKey(ctx context.Context, certURL string) (*rsa.PublicKey, error) {
	if k, ok := s.certs.get(certURL); ok {
		return k, nil
	}
	if s.certs.refused(certURL, s.clock.Now()) {
		return nil, fmt.Errorf("%w: certificate URL failed recently", errSignature)
	}
	select {
	case s.certs.slots <- struct{}{}:
		defer func() { <-s.certs.slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", errCertFetch, ctx.Err())
	}
	if k, ok := s.certs.get(certURL); ok { // fetched while we waited
		return k, nil
	}
	key, err := s.fetchCertKey(ctx, certURL)
	if err != nil {
		if errors.Is(err, errSignature) {
			s.certs.markBad(certURL, s.clock.Now())
		}
		return nil, err
	}
	s.certs.put(certURL, key)
	return key, nil
}

// fetchCertKey downloads and parses the certificate at certURL.
func (s *Service) fetchCertKey(ctx context.Context, certURL string) (*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, certURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSignature, err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCertFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: HTTP %d", errCertFetch, resp.StatusCode)
		}
		return nil, fmt.Errorf("%w: certificate URL returned HTTP %d", errSignature, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCertBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCertFetch, err)
	}
	if len(raw) > maxCertBytes {
		return nil, fmt.Errorf("%w: certificate too large", errSignature)
	}
	key, err := parseCertKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSignature, err)
	}
	return key, nil
}

// parseCertKey returns the RSA public key of the first PEM certificate.
func parseCertKey(raw []byte) (*rsa.PublicKey, error) {
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return nil, errors.New("no PEM certificate")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		key, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("certificate key is not RSA")
		}
		return key, nil
	}
}

// region returns the region of the pinned topic: the region field of
// SNS_TOPIC_ARN (arn:<partition>:sns:<region>:<account>:<name>), or
// AWS_REGION when the ARN has none. SNS serves a topic's signing
// certificates and SubscribeURLs from the topic's own region.
func (s *Service) region() string {
	parts := strings.Split(s.cfg.SNSTopicARN, ":")
	if len(parts) >= 6 && parts[0] == "arn" && parts[2] == "sns" && parts[3] != "" {
		return strings.ToLower(parts[3])
	}
	return strings.ToLower(strings.TrimSpace(s.cfg.AWSRegion))
}

// snsHost is the SNS endpoint host of region: sns.<region>.amazonaws.com,
// or .amazonaws.com.cn for China regions. It is "" for an empty region.
func snsHost(region string) string {
	if region == "" {
		return ""
	}
	h := "sns." + region + ".amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		h += ".cn"
	}
	return h
}

// validSubscribeURL reports whether u may be fetched to confirm a
// subscription: https on exactly sns.<region>.amazonaws.com (.com.cn for
// China regions) with no credentials or port (SSRF guard).
func (s *Service) validSubscribeURL(u string) bool {
	want := snsHost(s.region())
	if want == "" {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.User != nil || p.Port() != "" {
		return false
	}
	return p.Hostname() == want
}
