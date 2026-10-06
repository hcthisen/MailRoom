package imports

import (
	"slices"
	"strings"
	"testing"

	"mailroom/internal/core"
)

func TestDecideStatusRules(t *testing.T) {
	const (
		P  = core.StatusPending
		A  = core.StatusActive
		U  = core.StatusUnsubscribed
		B  = core.StatusBounced
		C  = core.StatusComplained
		SM = core.StatusSuppressedManual
	)
	type want struct {
		set            core.ContactStatus
		final          core.ContactStatus
		notReactivated bool
		statusKept     bool
	}
	tests := []struct {
		existing, csv core.ContactStatus
		want          want
	}{
		// No status in the file: never a change.
		{A, "", want{final: A}},
		{U, "", want{final: U}},
		// Same status: nothing to do.
		{U, U, want{final: U}},
		// active never reactivates anything.
		{P, A, want{final: P, notReactivated: true}},
		{U, A, want{final: U, notReactivated: true}},
		{B, A, want{final: B, notReactivated: true}},
		{C, A, want{final: C, notReactivated: true}},
		{SM, A, want{final: SM, notReactivated: true}},
		// Away from active (or pending): applied.
		{A, U, want{set: U, final: U}},
		{A, B, want{set: B, final: B}},
		{A, C, want{set: C, final: C}},
		{P, U, want{set: U, final: U}},
		{P, C, want{set: C, final: C}},
		// Harder to reactivate: applied.
		{U, B, want{set: B, final: B}},
		{U, C, want{set: C, final: C}},
		{B, C, want{set: C, final: C}},
		{SM, B, want{set: B, final: B}},
		// Easier to reactivate: kept.
		{B, U, want{final: B, statusKept: true}},
		{C, U, want{final: C, statusKept: true}},
		{C, B, want{final: C, statusKept: true}},
		{SM, U, want{final: SM, statusKept: true}},
	}
	for _, mode := range []string{OnExistingUpdate, OnExistingSkip} {
		for _, tt := range tests {
			t.Run(mode+"/"+string(tt.existing)+"->"+string(tt.csv), func(t *testing.T) {
				r := &record{email: "a@example.com", status: tt.csv}
				ex := &existingContact{id: 1, status: tt.existing, tags: map[string]bool{}}
				p := decide(r, ex, false, mode)
				got := want{set: p.setStatus, final: p.finalStatus, notReactivated: p.notReactivated, statusKept: p.statusKept}
				if got != tt.want {
					t.Fatalf("plan = %+v, want %+v", got, tt.want)
				}
				if p.finalStatus == core.StatusActive && tt.existing != core.StatusActive {
					t.Fatal("an import must never reactivate a contact")
				}
				wantOutcome := outUnchanged
				if mode == OnExistingSkip {
					wantOutcome = outSkipped
				}
				if p.setStatus != "" {
					wantOutcome = outUpdated
				}
				if p.outcome != wantOutcome {
					t.Fatalf("outcome = %d, want %d", p.outcome, wantOutcome)
				}
			})
		}
	}
}

func TestDecideNewAndSuppressed(t *testing.T) {
	r := &record{email: "a@example.com"}
	if p := decide(r, nil, false, OnExistingUpdate); p.outcome != outCreated || p.finalStatus != core.StatusActive || !p.consent {
		t.Fatalf("new, no status: %+v", p)
	}
	r.status = core.StatusComplained
	if p := decide(r, nil, false, OnExistingSkip); p.outcome != outCreated || p.finalStatus != core.StatusComplained {
		t.Fatalf("new complained: %+v", p)
	}
	ex := &existingContact{id: 1, status: core.StatusActive}
	for _, e := range []*existingContact{nil, ex} {
		if p := decide(r, e, true, OnExistingUpdate); p.outcome != outRejected || p.reason != ReasonSuppressed {
			t.Fatalf("suppressed: %+v", p)
		}
	}
}

func TestDecideUpdates(t *testing.T) {
	ex := &existingContact{
		id: 1, firstName: "Ann", lastName: "Smith", status: core.StatusActive,
		attrs: map[string]any{"company": "Acme", "seats": float64(3), "flag": true},
		tags:  map[string]bool{"vip": true},
	}
	tests := []struct {
		name        string
		rec         record
		wantOutcome outcome
		check       func(t *testing.T, p plan)
	}{
		{"same values", record{firstName: "Ann", attrs: map[string]string{"company": "Acme", "seats": "3", "flag": "true"}, tags: []string{"VIP"}},
			outUnchanged, func(t *testing.T, p plan) {
				if p.update != nil || len(p.addTags) != 0 || !p.consent {
					t.Fatalf("plan = %+v", p)
				}
			}},
		{"empty cells change nothing", record{}, outUnchanged, nil},
		{"new name", record{lastName: "Jones"}, outUpdated, func(t *testing.T, p plan) {
			if p.update == nil || p.update.FirstName != nil || *p.update.LastName != "Jones" || p.update.Attributes != nil {
				t.Fatalf("update = %+v", p.update)
			}
		}},
		{"changed and new attributes", record{attrs: map[string]string{"company": "Beta", "city": "Aarhus", "seats": "3"}}, outUpdated, func(t *testing.T, p plan) {
			if p.update == nil || len(p.update.Attributes) != 2 || p.update.Attributes["company"] != "Beta" || p.update.Attributes["city"] != "Aarhus" {
				t.Fatalf("update = %+v", p.update)
			}
		}},
		{"new tags only", record{tags: []string{"vip", "news"}}, outUpdated, func(t *testing.T, p plan) {
			if !slices.Equal(p.addTags, []string{"news"}) || p.update != nil {
				t.Fatalf("plan = %+v", p)
			}
		}},
		{"consent only", record{consentSource: "form"}, outUnchanged, func(t *testing.T, p plan) {
			if !p.consent {
				t.Fatal("consent details must be recorded")
			}
		}},
		{"merged attributes too large", record{attrs: map[string]string{"big": strings.Repeat("x", 17<<10)}}, outRejected, func(t *testing.T, p plan) {
			if !strings.Contains(p.reason, "attributes") {
				t.Fatalf("reason = %q", p.reason)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := decide(&tt.rec, ex, false, OnExistingUpdate)
			if p.outcome != tt.wantOutcome {
				t.Fatalf("outcome = %d, want %d (%+v)", p.outcome, tt.wantOutcome, p)
			}
			if tt.check != nil {
				tt.check(t, p)
			}
			// skip never changes names, attributes or tags, but still logs
			// the row's consent (I-2).
			if s := decide(&tt.rec, ex, false, OnExistingSkip); s.update != nil || len(s.addTags) != 0 || !s.consent || s.outcome != outSkipped {
				t.Fatalf("skip plan = %+v", s)
			}
		})
	}
}

func TestDecideKeepsBlockingStatus(t *testing.T) {
	big := map[string]string{"big": strings.Repeat("x", 17<<10)}
	active := func() *existingContact {
		return &existingContact{id: 1, status: core.StatusActive, tags: map[string]bool{}}
	}
	tests := []struct {
		name        string
		rec         record
		ex          *existingContact
		suppressed  bool
		wantOutcome outcome
		wantReason  bool
		wantCreate  bool
		wantSet     core.ContactStatus
	}{
		{"attributes overflow keeps unsubscribe", record{status: core.StatusUnsubscribed, attrs: big}, active(), false,
			outStatusOnly, true, false, core.StatusUnsubscribed},
		{"attributes overflow without status change is rejected", record{attrs: big}, active(), false,
			outRejected, true, false, ""},
		{"status-only record, new address", record{status: core.StatusBounced, statusOnly: true}, nil, false,
			outStatusOnly, false, true, ""},
		{"status-only record, active contact", record{status: core.StatusComplained, statusOnly: true}, active(), false,
			outStatusOnly, false, false, core.StatusComplained},
		{"status-only record, already blocked", record{status: core.StatusUnsubscribed, statusOnly: true},
			&existingContact{id: 1, status: core.StatusComplained}, false, outStatusOnly, false, false, ""},
		{"status-only record, suppressed", record{status: core.StatusUnsubscribed, statusOnly: true}, nil, true,
			outStatusOnly, false, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.rec.email = "a@example.com"
			p := decide(&tt.rec, tt.ex, tt.suppressed, OnExistingUpdate)
			if p.outcome != tt.wantOutcome || (p.reason != "") != tt.wantReason || p.create != tt.wantCreate || p.setStatus != tt.wantSet {
				t.Fatalf("plan = %+v", p)
			}
			if p.outcome == outStatusOnly && (p.update != nil || len(p.addTags) != 0 || (p.consent && !p.create)) {
				t.Fatalf("a status-only plan must not write other data: %+v", p)
			}
		})
	}
}

func TestAttrEqual(t *testing.T) {
	tests := []struct {
		cur  any
		v    string
		want bool
	}{
		{"a", "a", true},
		{"a", "b", false},
		{float64(3), "3", true},
		{float64(3.5), "3.5", true},
		{true, "true", true},
		{nil, "", false},
		{nil, "x", false},
	}
	for _, tt := range tests {
		if got := attrEqual(tt.cur, tt.v); got != tt.want {
			t.Errorf("attrEqual(%v, %q) = %v", tt.cur, tt.v, got)
		}
	}
}
