package tokens

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func newSigner(t *testing.T, secret string) (*Signer, *fixedClock) {
	t.Helper()
	clk := &fixedClock{t: t0}
	s, err := NewSigner([]byte(secret), clk)
	if err != nil {
		t.Fatal(err)
	}
	return s, clk
}

const secret = "0123456789abcdef0123456789abcdef"

func TestRoundTrip(t *testing.T) {
	s, _ := newSigner(t, secret)
	tests := []struct {
		name string
		p    Purpose
		pl   Payload
	}{
		{"unsubscribe", PurposeUnsubscribe, Payload{ContactID: 42, MessageID: 7}},
		{"click", PurposeClick, Payload{ContactID: 42, MessageID: 7, LinkID: 3}},
		{"open", PurposeOpen, Payload{ContactID: 1, MessageID: 1}},
		{"optin with expiry", PurposeOptin, Payload{ContactID: 5, Extra: "po:12", ExpiresAt: t0.Add(7 * 24 * time.Hour)}},
		{"resubscribe", PurposeResubscribe, Payload{ContactID: 5, Extra: "x"}},
		{"empty payload", PurposeClick, Payload{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := s.Sign(tt.p, tt.pl)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(tok, "+/=") {
				t.Fatalf("token %q is not URL-safe", tok)
			}
			got, err := s.Verify(tt.p, tok)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got != tt.pl {
				t.Fatalf("payload = %+v, want %+v", got, tt.pl)
			}
		})
	}
}

func TestWrongPurpose(t *testing.T) {
	s, _ := newSigner(t, secret)
	purposes := []Purpose{PurposeUnsubscribe, PurposeClick, PurposeOpen, PurposeOptin, PurposeResubscribe}
	for _, signed := range purposes {
		tok, err := s.Sign(signed, Payload{ContactID: 1, MessageID: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, verify := range purposes {
			_, err := s.Verify(verify, tok)
			if signed == verify && err != nil {
				t.Errorf("%s token rejected for its own purpose: %v", signed, err)
			}
			if signed != verify && !errors.Is(err, ErrInvalid) {
				t.Errorf("%s token accepted for purpose %s", signed, verify)
			}
		}
	}
}

func TestTamper(t *testing.T) {
	s, _ := newSigner(t, secret)
	tok, err := s.Sign(PurposeUnsubscribe, Payload{ContactID: 42, MessageID: 7})
	if err != nil {
		t.Fatal(err)
	}
	body, sig, _ := strings.Cut(tok, ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"c":43,"m":7}`))
	other, _ := newSigner(t, "another-secret-another-secret-xx")
	otherTok, _ := other.Sign(PurposeUnsubscribe, Payload{ContactID: 42, MessageID: 7})

	flip := []byte(sig)
	if flip[0] == 'A' {
		flip[0] = 'B'
	} else {
		flip[0] = 'A'
	}
	cases := map[string]string{
		"other contact":      forged + "." + sig,
		"flipped signature":  body + "." + string(flip),
		"truncated sig":      body + "." + sig[:10],
		"no signature":       body,
		"empty":              "",
		"dot only":           ".",
		"extra segment":      tok + ".x",
		"bad base64":         "!!!." + sig,
		"other secret":       otherTok,
		"oversized":          strings.Repeat("a", MaxTokenLength+1),
		"appended to body":   body + "A." + sig,
		"sig of other token": body + "." + strings.SplitN(otherTok, ".", 2)[1],
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Verify(PurposeUnsubscribe, bad); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Verify(%q) err = %v, want ErrInvalid", bad, err)
			}
		})
	}
}

func TestExpiry(t *testing.T) {
	s, clk := newSigner(t, secret)
	tok, err := s.Sign(PurposeOptin, Payload{ContactID: 1, ExpiresAt: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(PurposeOptin, tok); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	clk.t = t0.Add(time.Hour - time.Second)
	if _, err := s.Verify(PurposeOptin, tok); err != nil {
		t.Fatalf("token just before expiry: %v", err)
	}
	clk.t = t0.Add(time.Hour)
	pl, err := s.Verify(PurposeOptin, tok)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expired token err = %v", err)
	}
	if pl.ContactID != 1 {
		t.Fatal("expired verification should still return the payload")
	}
	// A token without expiry never expires.
	forever, _ := s.Sign(PurposeUnsubscribe, Payload{ContactID: 1})
	clk.t = t0.AddDate(10, 0, 0)
	if _, err := s.Verify(PurposeUnsubscribe, forever); err != nil {
		t.Fatalf("non-expiring token: %v", err)
	}
}

func TestSignerErrors(t *testing.T) {
	if _, err := NewSigner([]byte("short"), &fixedClock{}); err == nil {
		t.Fatal("want error for short secret")
	}
	if _, err := NewSigner([]byte(secret), nil); err == nil {
		t.Fatal("want error for nil clock")
	}
	s, _ := newSigner(t, secret)
	if _, err := s.Sign("bogus", Payload{}); err == nil {
		t.Fatal("want error for unknown purpose")
	}
	if _, err := s.Verify("bogus", "a.b"); !errors.Is(err, ErrInvalid) {
		t.Fatal("unknown purpose must be invalid")
	}
}

func TestHash(t *testing.T) {
	if h := Hash("abc"); h != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("Hash = %s", h)
	}
}
