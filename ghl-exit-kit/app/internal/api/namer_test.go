package api

import (
	"reflect"
	"testing"

	"mailroom/internal/api/internal/namertest"
)

type Enrollment struct {
	ID int64 `json:"id"`
}

func TestSchemaNamerDisambiguates(t *testing.T) {
	namer := newSchemaNamer()
	local := reflect.TypeOf(Enrollment{})
	other := reflect.TypeOf(namertest.Enrollment{})

	first := namer(local, "")
	second := namer(other, "")
	if first != "Enrollment" {
		t.Fatalf("first type name = %q, want Enrollment", first)
	}
	if second != "NamertestEnrollment" {
		t.Fatalf("second type name = %q, want NamertestEnrollment", second)
	}
	// Stable: the same type always gets the same name, pointer or not.
	if got := namer(reflect.PointerTo(other), ""); got != second {
		t.Fatalf("pointer to second type = %q, want %q", got, second)
	}
	if got := namer(local, ""); got != first {
		t.Fatalf("first type again = %q, want %q", got, first)
	}
}
