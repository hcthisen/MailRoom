package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "alice@example.com", want: "alice@example.com"},
		{in: "  Alice@Example.COM \t", want: "alice@example.com"},
		{in: "first.last+tag@sub.example.co.uk", want: "first.last+tag@sub.example.co.uk"},
		{in: "o'neil@example.ie", want: "o'neil@example.ie"},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "alice", wantErr: true},
		{in: "alice@", wantErr: true},
		{in: "@example.com", wantErr: true},
		{in: "alice@localhost", wantErr: true},
		{in: "alice@@example.com", wantErr: true},
		{in: "alice@bob@example.com", wantErr: true},
		{in: "Alice <alice@example.com>", wantErr: true},
		{in: "alice@example.com (comment)", wantErr: true},
		{in: "alice smith@example.com", wantErr: true},
		{in: `"alice smith"@example.com`, wantErr: true},
		{in: "alice@example..com", wantErr: true},
		{in: "alice@.example.com", wantErr: true},
		{in: "alice@-example.com", wantErr: true},
		{in: "alice@[127.0.0.1]", wantErr: true},
		{in: "a@b.c, d@e.f", wantErr: true},
		{in: strings.Repeat("a", 65) + "@example.com", wantErr: true},
		{in: "a@" + strings.Repeat("b", 250) + ".com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := NormalizeEmail(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeEmail(%q) = %q, want error", tt.in, got)
				}
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("error %v does not wrap ErrValidation", err)
				}
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Field != "email" {
					t.Fatalf("error %v is not a ValidationError for field email", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeEmail(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEmailHash(t *testing.T) {
	// sha256("alice@example.com")
	const want = "ff8d9819fc0e12bf0d24892e45987e249a28dce836a85cad60e28eaaa8c6d976"
	if got := EmailHash("alice@example.com"); got != want {
		t.Fatalf("EmailHash = %s, want %s", got, want)
	}
}

func TestCanReceive(t *testing.T) {
	// want[status] lists the kinds that status may receive.
	want := map[ContactStatus][]MessageKind{
		StatusPending:          {KindOptin, KindTest, KindAlert},
		StatusActive:           {KindBroadcast, KindSequence, KindOptin, KindTest, KindAlert},
		StatusUnsubscribed:     {KindOptin, KindAlert},
		StatusBounced:          {KindAlert},
		StatusComplained:       {KindAlert},
		StatusSuppressedManual: {KindAlert},
	}
	kinds := []MessageKind{KindBroadcast, KindSequence, KindOptin, KindTest, KindAlert}
	for _, st := range ContactStatuses {
		allowed := map[MessageKind]bool{}
		for _, k := range want[st] {
			allowed[k] = true
		}
		for _, k := range kinds {
			t.Run(fmt.Sprintf("%s/%s", st, k), func(t *testing.T) {
				if got := CanReceive(st, k); got != allowed[k] {
					t.Fatalf("CanReceive(%s, %s) = %v, want %v", st, k, got, allowed[k])
				}
			})
		}
	}
	if CanReceive(StatusActive, MessageKind("bogus")) {
		t.Fatal("unknown kind must not be receivable")
	}
}

func TestIsMarketingKind(t *testing.T) {
	tests := map[MessageKind]bool{
		KindBroadcast: true, KindSequence: true,
		KindOptin: false, KindTest: false, KindAlert: false,
	}
	for k, want := range tests {
		if got := IsMarketingKind(k); got != want {
			t.Errorf("IsMarketingKind(%s) = %v, want %v", k, got, want)
		}
	}
}

func TestEnumValid(t *testing.T) {
	for _, s := range ContactStatuses {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if ContactStatus("deleted").Valid() || MessageKind("sms").Valid() || MessageStatus("lost").Valid() {
		t.Error("unknown enum values must be invalid")
	}
	if !MessageQueued.Valid() || !KindAlert.Valid() {
		t.Error("known enum values must be valid")
	}
}

func TestActorContext(t *testing.T) {
	ctx := context.Background()
	if got := ActorFromContext(ctx); got.Type != ActorSystem || got.String() != "system" {
		t.Fatalf("default actor = %+v", got)
	}
	a := Actor{Type: ActorToken, ID: "7", Name: "setup-agent"}
	if got := ActorFromContext(WithActor(ctx, a)); got != a {
		t.Fatalf("ActorFromContext = %+v, want %+v", got, a)
	}
	if a.String() != "token:7" {
		t.Fatalf("String() = %q", a.String())
	}
}

func TestErrors(t *testing.T) {
	err := fmt.Errorf("create contact: %w", Invalid("tags", "tag %q is too long", "x"))
	if !errors.Is(err, ErrValidation) {
		t.Fatal("wrapped ValidationError must match ErrValidation")
	}
	if !strings.Contains(err.Error(), `tags: tag "x" is too long`) {
		t.Fatalf("message = %q", err.Error())
	}
	pe := fmt.Errorf("send: %w", &ProviderError{Retryable: true, Code: "Throttling", Message: "slow down"})
	var got *ProviderError
	if !errors.As(pe, &got) || !got.Retryable {
		t.Fatalf("errors.As ProviderError failed: %v", pe)
	}
}
