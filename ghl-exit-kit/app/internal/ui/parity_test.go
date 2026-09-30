package ui_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"
)

// parityAllowlist lists the API write operations that deliberately have
// no UI action, each with the reason (PRD G-1: the API is a superset of
// the UI, never the other way round).
var parityAllowlist = map[string]string{
	"upsert-contact": "integration-only: agents and syncs upsert by email; people create and edit contacts",
}

// strictParity makes a missing UI action for an API write fail the test
// (with false, the gaps are only logged).
const strictParity = true

// TestUIAPIParity is the UI/API parity test (PRD G-1, ARCHITECTURE.md §9):
//   - every UI action names an API operation (OperationID, plus any Uses)
//     that exists in api.Routes(), or explains in UIOnly why it has none;
//   - a GET action never claims a write operation as its OperationID;
//   - every API write operation (not GET) has a UI action, or is on
//     parityAllowlist with a reason; the gaps are listed by name.
func TestUIAPIParity(t *testing.T) {
	hs := newHarness(t)
	routes := hs.app.API.Routes()
	method := map[string]string{}
	for _, r := range routes {
		method[r.OperationID] = r.Method
	}

	covered := map[string][]string{}
	for _, a := range hs.app.UI.Actions() {
		where := a.Method + " " + a.Path
		if a.OperationID == "" && strings.TrimSpace(a.UIOnly) == "" {
			t.Errorf("%s names no API operation and gives no UIOnly reason", where)
		}
		ids := append([]string{a.OperationID}, a.Uses...)
		for i, id := range ids {
			if id == "" {
				continue
			}
			m, ok := method[id]
			if !ok {
				t.Errorf("%s names operation %q, which is not in api.Routes()", where, id)
				continue
			}
			if i == 0 && a.Method == http.MethodGet && m != http.MethodGet {
				t.Errorf("%s is a GET but claims the write operation %s (%s): GET pages must not change data", where, id, m)
			}
			covered[id] = append(covered[id], where)
		}
	}

	for id, reason := range parityAllowlist {
		m, ok := method[id]
		switch {
		case !ok:
			t.Errorf("allowlisted operation %q is not in api.Routes()", id)
		case m == http.MethodGet:
			t.Errorf("allowlisted operation %q is a read; only writes need the allowlist", id)
		case strings.TrimSpace(reason) == "":
			t.Errorf("allowlisted operation %q has no reason", id)
		case len(covered[id]) > 0:
			t.Errorf("allowlisted operation %q has UI actions %v: remove it from the allowlist", id, covered[id])
		}
	}

	var missingWrites, missingReads []string
	for _, r := range routes {
		if len(covered[r.OperationID]) > 0 {
			continue
		}
		if r.Method == http.MethodGet {
			missingReads = append(missingReads, r.OperationID)
			continue
		}
		if _, ok := parityAllowlist[r.OperationID]; !ok {
			missingWrites = append(missingWrites, r.OperationID+" ("+r.Method+" "+r.Path+")")
		}
	}
	sort.Strings(missingWrites)
	sort.Strings(missingReads)
	if len(missingReads) > 0 {
		t.Logf("%d API read operations have no UI page (informational): %s", len(missingReads), strings.Join(missingReads, ", "))
	}
	if len(missingWrites) > 0 {
		msg := "API write operations without a UI action (add one with u.handle, or allowlist them with a reason):\n  " +
			strings.Join(missingWrites, "\n  ")
		if strictParity {
			t.Error(msg)
		} else {
			t.Logf("%d gaps (not failing until strictParity is set):\n%s", len(missingWrites), msg)
		}
	}
}
