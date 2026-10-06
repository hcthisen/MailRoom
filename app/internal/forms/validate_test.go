package forms_test

import (
	"errors"
	"strings"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/forms"
)

func TestValidateOrigins(t *testing.T) {
	tests := []struct {
		in    []string
		want  string // comma-joined, or "" with wantErr
		field string // error field, "" = valid
	}{
		{[]string{"https://Example.COM", "https://example.com/", "http://localhost:8080"}, "https://example.com,http://localhost:8080", ""},
		{[]string{}, "", ""},
		{[]string{"example.com"}, "", "allowed_origins[0]"},
		{[]string{"https://example.com", "https://example.com/path"}, "", "allowed_origins[1]"},
		{[]string{"https://example.com?x=1"}, "", "allowed_origins[0]"},
		{[]string{"https://user@example.com"}, "", "allowed_origins[0]"},
		{[]string{"*"}, "", "allowed_origins[0]"},
		{[]string{"wss://example.com"}, "", "allowed_origins[0]"},
	}
	for _, tt := range tests {
		got, err := forms.ValidateOrigins(tt.in)
		if tt.field != "" {
			var ve *core.ValidationError
			if !errors.As(err, &ve) || ve.Field != tt.field {
				t.Errorf("ValidateOrigins(%v) err = %v, want field %s", tt.in, err, tt.field)
			}
			continue
		}
		if err != nil || strings.Join(got, ",") != tt.want {
			t.Errorf("ValidateOrigins(%v) = %v, %v", tt.in, got, err)
		}
	}
	many := make([]string, forms.MaxOrigins+1)
	for i := range many {
		many[i] = "https://example.com"
	}
	if _, err := forms.ValidateOrigins(many); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("too many origins: %v", err)
	}
}

func TestValidateFields(t *testing.T) {
	got, err := forms.ValidateFields([]forms.FormField{
		{Key: " first_name "}, {Key: "last_name", Label: "Surname", Required: true}, {Key: "attr.company_size"},
	})
	if err != nil {
		t.Fatal(err)
	}
	labels := []string{}
	for _, f := range got {
		labels = append(labels, f.Key+"="+f.Label)
	}
	if strings.Join(labels, ",") != "first_name=First name,last_name=Surname,attr.company_size=Company size" || !got[1].Required {
		t.Fatalf("fields = %+v", got)
	}
	bad := []struct {
		f     []forms.FormField
		field string
	}{
		{[]forms.FormField{{Key: ""}}, "fields[0].key"},
		{[]forms.FormField{{Key: "attr."}}, "fields[0].key"},
		{[]forms.FormField{{Key: "first_name", Label: strings.Repeat("x", forms.MaxLabelLength+1)}}, "fields[0].label"},
		{[]forms.FormField{{Key: "first_name", Label: "bad\x00"}}, "fields[0].label"},
	}
	for _, b := range bad {
		var ve *core.ValidationError
		if _, err := forms.ValidateFields(b.f); !errors.As(err, &ve) || ve.Field != b.field {
			t.Errorf("ValidateFields(%+v) = %v, want field %s", b.f, err, b.field)
		}
	}
	if got, err := forms.ValidateFields(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil fields = %v, %v", got, err)
	}
}
