package auth

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors (SHA-1, 8 digits, 30-second steps).
func TestHOTPRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	tests := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, tt := range tests {
		if got := hotp(key, totpCounter(time.Unix(tt.unix, 0)), 8); got != tt.want {
			t.Errorf("T=%d: got %s, want %s", tt.unix, got, tt.want)
		}
	}
	// The 6-digit code is the last 6 digits of the same truncation.
	secret := totpEncoding.EncodeToString(key)
	if got, _ := TOTPCode(secret, time.Unix(59, 0)); got != "287082" {
		t.Fatalf("6-digit code = %s", got)
	}
}

// RFC 4226 Appendix D HOTP values for counters 0-9.
func TestHOTPRFC4226Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for c, w := range want {
		if got := hotp(key, uint64(c), 6); got != w {
			t.Errorf("counter %d: got %s, want %s", c, got, w)
		}
	}
}
