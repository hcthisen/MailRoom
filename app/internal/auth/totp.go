package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters (RFC 6238): SHA-1, 30-second steps, 6 digits, and codes
// from one step before or after are accepted to absorb clock drift.
const (
	TOTPPeriod     = 30 * time.Second
	TOTPDigits     = 6
	TOTPSkew       = 1
	totpSecretSize = 20 // 160 bits, as RFC 4226 recommends
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random base32 secret (no padding) for an
// authenticator app.
func NewTOTPSecret() (string, error) {
	b := make([]byte, totpSecretSize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: totp secret: %w", err)
	}
	return totpEncoding.EncodeToString(b), nil
}

// decodeTOTPSecret accepts the base32 secret with or without padding,
// spaces and in any case.
func decodeTOTPSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	s = strings.TrimRight(s, "=")
	key, err := totpEncoding.DecodeString(s)
	if err != nil || len(key) < 10 {
		return nil, fmt.Errorf("auth: invalid TOTP secret")
	}
	return key, nil
}

// hotp computes the RFC 4226 HOTP value of counter with the given number
// of digits (HMAC-SHA-1 and dynamic truncation).
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}

// totpCounter is the RFC 6238 time step of t.
func totpCounter(t time.Time) uint64 {
	return uint64(t.Unix()) / uint64(TOTPPeriod/time.Second)
}

// TOTPCode returns the current 6-digit code of secret at time t.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, totpCounter(t), TOTPDigits), nil
}

// VerifyTOTP reports whether code is valid for secret at time t, allowing
// TOTPSkew steps of drift either way. Spaces in code are ignored. It does
// not prevent replays; logins use MatchTOTP and remember the step.
func VerifyTOTP(secret, code string, t time.Time) bool {
	_, ok := MatchTOTP(secret, code, t)
	return ok
}

// MatchTOTP checks code like VerifyTOTP and also returns the RFC 6238 time
// step it matched. A verifier must accept each step at most once (RFC 6238
// §5.2): store the step and refuse codes whose step is not greater.
func MatchTOTP(secret, code string, t time.Time) (step uint64, ok bool) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != TOTPDigits {
		return 0, false
	}
	c := totpCounter(t)
	matched := 0
	for d := -TOTPSkew; d <= TOTPSkew; d++ {
		if d < 0 && c < uint64(-d) {
			continue
		}
		s := uint64(int64(c) + int64(d))
		eq := subtle.ConstantTimeCompare([]byte(hotp(key, s, TOTPDigits)), []byte(code))
		if eq == 1 {
			step = s // the latest matching step wins
		}
		matched |= eq
	}
	return step, matched == 1
}

// TOTPURI returns the otpauth:// URI that authenticator apps import (as a
// QR code), e.g. otpauth://totp/Mailroom:owner@example.com?secret=...
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(int(TOTPPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
