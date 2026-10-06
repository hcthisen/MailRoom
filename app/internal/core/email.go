package core

import (
	"crypto/sha256"
	"encoding/hex"
	"net/mail"
	"strings"
)

// MaxEmailLength is the longest accepted address (RFC 5321 path limit).
const MaxEmailLength = 254

// NormalizeEmail trims and lowercases s and checks that it is a bare
// address: it parses with net/mail, has no display name or comments,
// exactly one "@", a non-empty local part of at most 64 bytes, a domain
// containing a dot and at most MaxEmailLength bytes in total. The error is
// a *ValidationError for field "email".
func NormalizeEmail(s string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(s))
	if e == "" {
		return "", Invalid("email", "is required")
	}
	if len(e) > MaxEmailLength {
		return "", Invalid("email", "is longer than %d characters", MaxEmailLength)
	}
	if strings.Count(e, "@") != 1 {
		return "", Invalid("email", "must contain exactly one @")
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Name != "" || addr.Address != e {
		return "", Invalid("email", "is not a valid email address")
	}
	local, domain, _ := strings.Cut(e, "@")
	if local == "" || len(local) > 64 {
		return "", Invalid("email", "has an invalid local part")
	}
	if !validEmailDomain(domain) {
		return "", Invalid("email", "has an invalid domain")
	}
	return e, nil
}

// validEmailDomain checks for a dotted domain with non-empty labels that
// don't start or end with a hyphen. Domain literals ([1.2.3.4]) are rejected.
func validEmailDomain(d string) bool {
	if !strings.Contains(d, ".") || len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r == '[' || r == ']' || r == ' ' || r < 0x20 {
				return false
			}
		}
	}
	return true
}

// EmailHash returns the lowercase hex SHA-256 of an already normalised
// address, as stored in suppressions.email_hash.
func EmailHash(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}
