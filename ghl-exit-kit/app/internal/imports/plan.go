package imports

import (
	"fmt"
	"maps"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
)

// outcome is what an import does with one record.
type outcome int

const (
	outCreated outcome = iota + 1
	outUpdated
	outUnchanged
	outSkipped
	outRejected
	// outStatusOnly: the record's other data is rejected, but its status,
	// which blocks sending, is still applied (or the address was already
	// blocked).
	outStatusOnly
)

// existingContact is the current state of a contact the file refers to.
type existingContact struct {
	id        int64
	firstName string
	lastName  string
	attrs     map[string]any
	status    core.ContactStatus
	tags      map[string]bool // ASCII-folded names
}

// plan is the decision for one record, computed from the database state
// alone, so a dry run and the real import agree.
type plan struct {
	outcome outcome
	// reason is why the record (outRejected) or its data other than the
	// status (outStatusOnly) is rejected. For status-only records parsed
	// from the file it is "": parseFile already reported their rows.
	reason string

	create    bool                      // create the contact (with finalStatus)
	update    *contactstore.UpdateInput // existing contact: names/attributes to change; nil = none
	addTags   []string                  // existing contact: tags it doesn't have yet
	setStatus core.ContactStatus        // existing contact: new status; "" = keep
	consent   bool                      // record an import consent event

	finalStatus    core.ContactStatus // the contact's status after the import
	notReactivated bool               // CSV said active, the contact stays non-active
	statusKept     bool               // CSV status refused because it would ease reactivation
}

// withoutDetails turns p into a status-only plan: names, attributes, tags
// and consent are dropped and reason is reported, but the status change or
// creation p makes to block the address is kept.
func (p plan) withoutDetails(reason string) plan {
	return plan{
		outcome: outStatusOnly, reason: reason,
		create: p.create, setStatus: p.setStatus, finalStatus: p.finalStatus, consent: p.create,
	}
}

// appliesStatus reports whether p writes a blocking status (a status-only
// plan that doesn't is a no-op: the address is already blocked).
func (p plan) appliesStatus() bool { return p.create || p.setStatus != "" }

// decide plans the import of r. ex is the existing contact with r's
// address (nil = none); suppressed reports an erased or manually
// suppressed address.
//
// Statuses only ever move away from active (never reactivate): a CSV
// status that blocks sending is applied when contactstore.CanTransition
// allows it for source import; "active" never reactivates a contact that
// is pending, unsubscribed, bounced, complained or suppressed (a manually
// suppressed contact normally still has its suppression and is rejected
// as suppressed before any of this).
func decide(r *record, ex *existingContact, suppressed bool, onExisting string) plan {
	if suppressed {
		if r.statusOnly {
			return plan{outcome: outStatusOnly} // already blocked; already reported
		}
		return plan{outcome: outRejected, reason: ReasonSuppressed}
	}
	if ex == nil {
		st := r.status
		if st == "" {
			st = core.StatusActive
		}
		p := plan{outcome: outCreated, create: true, finalStatus: st, consent: true}
		if r.statusOnly {
			return p.withoutDetails("")
		}
		return p
	}

	p := plan{finalStatus: ex.status}
	switch {
	case r.status == "" || r.status == ex.status:
	case r.status == core.StatusActive:
		p.notReactivated = true
	case contactstore.CanTransition(ex.status, r.status, contactstore.SourceImport, "") == nil:
		p.setStatus, p.finalStatus = r.status, r.status
	default:
		p.statusKept = true
	}
	if r.statusOnly {
		return p.withoutDetails("")
	}
	changed := p.setStatus != ""
	// Every imported row gets a consent event (I-2), existing contacts
	// too and in both modes: the consent log is evidence, not contact data.
	// recordConsentOnce keeps re-imports from repeating it.
	p.consent = true

	if onExisting == OnExistingSkip {
		p.outcome = outSkipped
		if changed {
			p.outcome = outUpdated
		}
		return p
	}

	upd, err := updateFor(r, ex)
	if err != nil {
		if changed {
			return p.withoutDetails(reasonOf(err))
		}
		return plan{outcome: outRejected, reason: reasonOf(err)}
	}
	p.update = upd
	for _, t := range r.tags {
		if !ex.tags[foldASCII(t)] {
			p.addTags = append(p.addTags, t)
		}
	}
	p.outcome = outUnchanged
	if changed || upd != nil || len(p.addTags) > 0 {
		p.outcome = outUpdated
	}
	return p
}

// updateFor returns the name and attribute changes r makes to ex, or nil
// when there are none. Empty CSV cells never clear a value.
func updateFor(r *record, ex *existingContact) (*contactstore.UpdateInput, error) {
	var in contactstore.UpdateInput
	changed := false
	if r.firstName != "" && r.firstName != ex.firstName {
		in.FirstName, changed = &r.firstName, true
	}
	if r.lastName != "" && r.lastName != ex.lastName {
		in.LastName, changed = &r.lastName, true
	}
	for k, v := range r.attrs {
		if cur, ok := ex.attrs[k]; ok && attrEqual(cur, v) {
			continue
		}
		if in.Attributes == nil {
			in.Attributes = map[string]any{}
		}
		in.Attributes[k] = v
	}
	if len(in.Attributes) > 0 {
		merged := maps.Clone(ex.attrs)
		if merged == nil {
			merged = map[string]any{}
		}
		maps.Copy(merged, in.Attributes)
		if err := contactstore.ValidateAttributes(merged); err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return &in, nil
}

// attrEqual reports whether a stored attribute equals a CSV value. Non-string
// values (numbers, booleans set through the API) compare by their text, so
// "3" doesn't turn the number 3 into a string.
func attrEqual(cur any, v string) bool {
	if s, ok := cur.(string); ok {
		return s == v
	}
	if cur == nil {
		return false
	}
	return fmt.Sprint(cur) == v
}
