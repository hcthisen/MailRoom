// Package tokens creates and verifies the HMAC-signed tokens used in public
// URLs (unsubscribe, click, open, opt-in and resubscribe confirmation).
//
// A token is base64url(payload JSON) + "." + base64url(HMAC-SHA256). The MAC
// covers the purpose as well as the payload, so a token issued for one
// purpose never verifies for another (PRD U-1: an unsubscribe token does
// not unlock anything else), and a token for one contact can't be edited to
// name another.
package tokens

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/core"
)

// Purpose separates token namespaces.
type Purpose string

// Token purposes.
const (
	PurposeUnsubscribe Purpose = "unsub"
	PurposeClick       Purpose = "click"
	PurposeOpen        Purpose = "open"
	PurposeOptin       Purpose = "optin"
	PurposeResubscribe Purpose = "resub"
)

func (p Purpose) valid() bool {
	switch p {
	case PurposeUnsubscribe, PurposeClick, PurposeOpen, PurposeOptin, PurposeResubscribe:
		return true
	}
	return false
}

// MaxTokenLength bounds accepted tokens, so oversized input is rejected
// before any decoding work.
const MaxTokenLength = 1024

// Errors returned by Verify. Both mean "treat the link as invalid".
var (
	ErrInvalid = errors.New("tokens: invalid token")
	ErrExpired = errors.New("tokens: token expired")
)

// Payload is the signed content of a token. Zero fields are omitted.
type Payload struct {
	ContactID int64
	MessageID int64
	LinkID    int64
	Extra     string    // purpose-specific data, e.g. a pending opt-in ID
	ExpiresAt time.Time // zero: never expires
}

// wire is the compact JSON form of Payload.
type wire struct {
	C int64  `json:"c,omitempty"`
	M int64  `json:"m,omitempty"`
	L int64  `json:"l,omitempty"`
	X string `json:"x,omitempty"`
	E int64  `json:"e,omitempty"` // unix seconds
}

// Signer signs and verifies tokens with a key derived from the install secret.
type Signer struct {
	key   []byte
	clock core.Clock
}

// keyLabel derives a tokens-only subkey, so the install secret can safely
// be used for other MACs too.
const keyLabel = "mailroom/tokens/v1"

// NewSigner returns a Signer for secret. clock is used for expiry checks.
func NewSigner(secret []byte, clock core.Clock) (*Signer, error) {
	if len(secret) < 16 {
		return nil, errors.New("tokens: secret must be at least 16 bytes")
	}
	if clock == nil {
		return nil, errors.New("tokens: clock is required")
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(keyLabel))
	return &Signer{key: m.Sum(nil), clock: clock}, nil
}

var enc = base64.RawURLEncoding.Strict()

// Sign returns a token for purpose p carrying pl.
func (s *Signer) Sign(p Purpose, pl Payload) (string, error) {
	if !p.valid() {
		return "", fmt.Errorf("tokens: unknown purpose %q", p)
	}
	w := wire{C: pl.ContactID, M: pl.MessageID, L: pl.LinkID, X: pl.Extra}
	if !pl.ExpiresAt.IsZero() {
		w.E = pl.ExpiresAt.Unix()
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("tokens: encode payload: %w", err)
	}
	body := enc.EncodeToString(b)
	tok := body + "." + enc.EncodeToString(s.mac(p, body))
	if len(tok) > MaxTokenLength {
		return "", fmt.Errorf("tokens: token longer than %d bytes", MaxTokenLength)
	}
	return tok, nil
}

// Verify checks token for purpose p and returns its payload. It returns
// ErrInvalid for malformed, tampered or wrong-purpose tokens and ErrExpired
// for valid tokens past their expiry.
func (s *Signer) Verify(p Purpose, token string) (Payload, error) {
	if !p.valid() || len(token) > MaxTokenLength {
		return Payload{}, ErrInvalid
	}
	body, sigStr, ok := strings.Cut(token, ".")
	if !ok || body == "" || strings.Contains(sigStr, ".") {
		return Payload{}, ErrInvalid
	}
	sig, err := enc.DecodeString(sigStr)
	if err != nil || !hmac.Equal(sig, s.mac(p, body)) {
		return Payload{}, ErrInvalid
	}
	raw, err := enc.DecodeString(body)
	if err != nil {
		return Payload{}, ErrInvalid
	}
	var w wire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Payload{}, ErrInvalid
	}
	pl := Payload{ContactID: w.C, MessageID: w.M, LinkID: w.L, Extra: w.X}
	if w.E != 0 {
		pl.ExpiresAt = time.Unix(w.E, 0).UTC()
		if !s.clock.Now().Before(pl.ExpiresAt) {
			return pl, ErrExpired
		}
	}
	return pl, nil
}

// mac is HMAC-SHA256(key, purpose || 0x00 || body).
func (s *Signer) mac(p Purpose, body string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(p))
	m.Write([]byte{0})
	m.Write([]byte(body))
	return m.Sum(nil)
}

// Hash returns the lowercase hex SHA-256 of a token, for storing lookups
// (e.g. pending_optins.token_hash) without storing the token itself.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
