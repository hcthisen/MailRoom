package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"mailroom/internal/core"
)

// Password policy.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 256
)

// Argon2Params are argon2id cost parameters.
type Argon2Params struct {
	Time    uint32 // iterations
	Memory  uint32 // KiB
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultArgon2Params: time=2, memory=64 MiB, threads=2, 16-byte salt,
// 32-byte key.
var DefaultArgon2Params = Argon2Params{Time: 2, Memory: 64 * 1024, Threads: 2, SaltLen: 16, KeyLen: 32}

// Upper bounds accepted when parsing a stored hash, so a corrupted row
// can't make verification exhaust memory or CPU.
const (
	maxArgonTime   = 16
	maxArgonMemory = 1024 * 1024 // 1 GiB
	maxArgonKeyLen = 128
)

var b64 = base64.RawStdEncoding

// hashSlots bounds concurrent argon2 computations (64 MiB each), so a
// burst of logins can't exhaust a small server's memory.
var hashSlots = make(chan struct{}, 2)

// maxHashWaiters bounds the goroutines queued for a hash slot. Beyond it,
// hashing fails fast with ErrBusy instead of piling up requests.
const maxHashWaiters = 16

var hashWaiters atomic.Int32

// ErrBusy is returned when too many password hashes are already queued.
// It is safe to show: "try again in a moment".
var ErrBusy = errors.New("the server is busy checking other logins; try again in a moment")

// acquireHashSlot waits for a hash slot, until ctx is done. It fails with
// ErrBusy when maxHashWaiters are already waiting.
func acquireHashSlot(ctx context.Context) error {
	if hashWaiters.Add(1) > maxHashWaiters {
		hashWaiters.Add(-1)
		return ErrBusy
	}
	defer hashWaiters.Add(-1)
	select {
	case hashSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseHashSlot() { <-hashSlots }

// HashPassword returns the argon2id PHC string of password:
//
//	$argon2id$v=19$m=65536,t=2,p=2$<salt>$<hash>
//
// It waits for one of the few hash slots until ctx is done, and fails with
// ErrBusy when too many hashes are queued.
func HashPassword(ctx context.Context, password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	if err := acquireHashSlot(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	releaseHashSlot()
	return formatPHC(p, salt, key), nil
}

func formatPHC(p Argon2Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// VerifyPassword reports whether password matches the PHC string phc. An
// unparsable hash is an error. Like HashPassword it waits for a hash slot
// until ctx is done and may fail with ErrBusy.
func VerifyPassword(ctx context.Context, password, phc string) (bool, error) {
	p, salt, want, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if err := acquireHashSlot(ctx); err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	releaseHashSlot()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash returns a PHC string with parameters p and random salt and
// key. Verifying any password against it costs as much as a real check and
// fails, so unknown emails take as long as wrong passwords.
func dummyHash(p Argon2Params) string {
	salt, key := make([]byte, p.SaltLen), make([]byte, p.KeyLen)
	_, _ = rand.Read(salt)
	_, _ = rand.Read(key)
	return formatPHC(p, salt, key)
}

var errBadHash = errors.New("auth: malformed password hash")

func parsePHC(phc string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(phc, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, errBadHash
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return Argon2Params{}, nil, nil, errBadHash
	}
	var p Argon2Params
	var threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &threads); err != nil {
		return Argon2Params{}, nil, nil, errBadHash
	}
	if p.Time == 0 || p.Time > maxArgonTime || p.Memory == 0 || p.Memory > maxArgonMemory || threads == 0 || threads > 255 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	p.Threads = uint8(threads)
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > maxArgonKeyLen {
		return Argon2Params{}, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

// CheckPasswordPolicy enforces the password policy: 12 to 256 characters,
// not only whitespace. Errors are *core.ValidationError for "password".
func CheckPasswordPolicy(pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case !utf8.ValidString(pw):
		return core.Invalid("password", "must be valid UTF-8")
	case n < MinPasswordLength:
		return core.Invalid("password", "must be at least %d characters", MinPasswordLength)
	case n > MaxPasswordLength:
		return core.Invalid("password", "must be at most %d characters", MaxPasswordLength)
	case strings.TrimSpace(pw) == "":
		return core.Invalid("password", "must not be only whitespace")
	}
	return nil
}

// passwordAlphabet has no look-alike characters (0/O, 1/l/I).
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GeneratePassword returns a random 24-character password (about 140 bits).
func GeneratePassword() (string, error) {
	return randomString(24, passwordAlphabet)
}

// randomString returns n characters drawn uniformly from alphabet.
func randomString(n int, alphabet string) (string, error) {
	out := make([]byte, n)
	buf := make([]byte, 1)
	limit := 256 - 256%len(alphabet) // rejection sampling avoids modulo bias
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("auth: random: %w", err)
		}
		if int(buf[0]) >= limit {
			continue
		}
		out[i] = alphabet[int(buf[0])%len(alphabet)]
		i++
	}
	return string(out), nil
}
