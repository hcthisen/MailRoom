package imports

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/contactstore"
	"mailroom/internal/core"
)

// ImportRejection is one CSV row that was not imported. It names the line
// only, never the address.
type ImportRejection struct {
	Line   int    `json:"line" doc:"Line number in the file (the header is line 1)" example:"17"`
	Reason string `json:"reason" doc:"Why the row was not imported" example:"email is not a valid email address"`
}

// Rejection reasons that tests and the docs refer to. Reasons never repeat
// a cell of the row: a misplaced column could put an address into any
// cell, and reports are kept in the job row, which GDPR erase doesn't
// touch.
const (
	ReasonSuppressed = "suppressed: the address was erased or is manually suppressed"
	ReasonNotUTF8    = "the row is not valid UTF-8; save the file as UTF-8"
	ReasonBadTag     = "tags contain a tag that is longer than 64 characters or has invalid characters"
)

// record is one contact of the file: a valid row, or several rows for the
// same address merged into one.
type record struct {
	line          int // first line of the address
	email         string
	firstName     string             // "" = not given
	lastName      string             // "" = not given
	status        core.ContactStatus // "" = not given
	tags          []string
	consentSource string
	consentDate   time.Time // zero = not given
	attrs         map[string]string
	merged        bool
	// statusOnly marks a record whose rows were all rejected except for
	// their status, which blocks sending: only the address and status are
	// kept, so an opted-out address never stays sendable because another
	// field of its row was invalid (G-6).
	statusOnly bool
}

// blocksSending reports whether a CSV status keeps the address from
// being emailed.
func blocksSending(st core.ContactStatus) bool { return st != "" && st != core.StatusActive }

// statusOnlyReason is the rejection reason of a row of which only the
// blocking status is imported.
func statusOnlyReason(reason string, st core.ContactStatus) string {
	return fmt.Sprintf("%s; only the row's status (%s) was imported", reason, st)
}

// statusOnlyRecord returns a record that keeps only r's address, line and
// status.
func statusOnlyRecord(r *record) *record {
	return &record{line: r.line, email: r.email, status: r.status, statusOnly: true}
}

// raiseStatus keeps the status of o when it blocks sending more than r's.
func (r *record) raiseStatus(o core.ContactStatus) {
	if statusRank(o) > statusRank(r.status) {
		r.status = o
	}
}

// hasConsent reports whether the row carries consent details.
func (r *record) hasConsent() bool { return r.consentSource != "" || !r.consentDate.IsZero() }

// parsedFile is a whole file read into merged records.
type parsedFile struct {
	records    []*record // first-appearance order
	rows       int       // data rows read (blank rows excluded)
	duplicates int       // rows merged into an earlier row for the same address
	rejected   []ImportRejection
	rejections int // total rejected rows (rejected is capped)
	warnings   []string
}

func (pf *parsedFile) reject(line int, reason string) {
	pf.rejections++
	if len(pf.rejected) < MaxRejectedListed {
		pf.rejected = append(pf.rejected, ImportRejection{Line: line, Reason: reason})
	}
}

// parseFile reads the whole CSV from r. It returns a validation error
// (field "file") for problems with the file as a whole: no header, no
// email column, unreadable CSV, too large. Row problems become rejections.
// now bounds consent dates (a date more than a day ahead is rejected).
func parseFile(r io.Reader, now time.Time) (*parsedFile, error) {
	cr, h, err := openCSV(r)
	if err != nil {
		return nil, err
	}
	pf := &parsedFile{warnings: h.warnings}
	byEmail := map[string]*record{}
	tags := tagCanon{}
	for {
		fields, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			// The reader continues with the next line; only this row is lost.
			pf.rows++
			pf.reject(pe.StartLine, fmt.Sprintf("the row is not valid CSV (column %d: %v)", pe.Column, pe.Err))
			continue
		}
		if err != nil {
			return nil, readError(err)
		}
		if blank(fields) {
			continue
		}
		line, _ := cr.FieldPos(0)
		pf.rows++
		rec, reason := parseRow(h, fields, now)
		if reason != "" {
			pf.reject(line, reason)
			if rec == nil {
				continue
			}
		}
		rec.line = line
		rec.tags = tags.apply(rec.tags)
		prev, ok := byEmail[rec.email]
		switch {
		case !ok:
			byEmail[rec.email] = rec
			pf.records = append(pf.records, rec)
		case rec.statusOnly:
			// The row is already counted as rejected; its status still counts.
			prev.raiseStatus(rec.status)
		case prev.statusOnly:
			// The first valid row for the address takes the place of the
			// status-only record (whose rows are counted as rejected).
			rec.raiseStatus(prev.status)
			*prev = *rec
		default:
			prev.merge(rec)
			pf.duplicates++
		}
	}
	// Merged attributes may together exceed the attribute size limit.
	kept := pf.records[:0]
	for _, rec := range pf.records {
		if rec.merged {
			if err := contactstore.ValidateAttributes(anyMap(rec.attrs)); err != nil {
				if !blocksSending(rec.status) {
					pf.reject(rec.line, reasonOf(err))
					continue
				}
				pf.reject(rec.line, statusOnlyReason(reasonOf(err), rec.status))
				rec = statusOnlyRecord(rec)
			}
		}
		kept = append(kept, rec)
	}
	pf.records = kept
	return pf, nil
}

// blank reports whether every field is empty or whitespace.
func blank(fields []string) bool {
	for _, f := range fields {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}

// parseRow validates one data row. It returns the record, or a rejection
// reason. Reasons never contain the address. When the address and a
// status that blocks sending are valid but another field isn't, it
// returns both a reason and a status-only record, so the row's opt-out
// is still imported.
func parseRow(h header, fields []string, now time.Time) (*record, string) {
	if len(fields) > h.width && !blank(fields[h.width:]) {
		return nil, fmt.Sprintf("the row has %d fields but the header has %d", len(fields), h.width)
	}
	get := func(i int) string {
		if i < 0 || i >= len(fields) {
			return ""
		}
		return strings.TrimSpace(fields[i])
	}

	rawEmail, rawStatus := get(h.email), get(h.status)
	if !utf8.ValidString(rawEmail) || !utf8.ValidString(rawStatus) {
		return nil, ReasonNotUTF8
	}
	if rawEmail == "" {
		return nil, "email is missing"
	}
	email, err := core.NormalizeEmail(rawEmail)
	if err != nil {
		return nil, reasonOf(err)
	}
	status, err := parseStatus(rawStatus)
	if err != nil {
		return nil, reasonOf(err)
	}
	rec := &record{email: email, status: status}
	if reason := rec.parseDetails(h, fields, get, now); reason != "" {
		if !blocksSending(status) {
			return nil, reason
		}
		return statusOnlyRecord(rec), statusOnlyReason(reason, status)
	}
	return rec, ""
}

// parseDetails reads the row's fields other than email and status into r.
// It returns a rejection reason, or "".
func (r *record) parseDetails(h header, fields []string, get func(int) string, now time.Time) string {
	for _, f := range fields {
		if !utf8.ValidString(f) {
			return ReasonNotUTF8
		}
	}
	var err error
	if r.firstName, err = contactstore.ValidateName(ColFirstName, get(h.firstName)); err != nil {
		return reasonOf(err)
	}
	if r.lastName, err = contactstore.ValidateName(ColLastName, get(h.lastName)); err != nil {
		return reasonOf(err)
	}
	if r.tags, err = parseTags(get(h.tags)); err != nil {
		return reasonOf(err)
	}
	if r.consentSource, err = parseConsentSource(get(h.consentSource)); err != nil {
		return reasonOf(err)
	}
	if r.consentDate, err = parseConsentDate(get(h.consentDate), now); err != nil {
		return reasonOf(err)
	}
	for _, a := range h.attrs {
		if v := get(a.index); v != "" {
			if r.attrs == nil {
				r.attrs = map[string]string{}
			}
			r.attrs[a.key] = v
		}
	}
	if err := contactstore.ValidateAttributes(anyMap(r.attrs)); err != nil {
		return reasonOf(err)
	}
	return ""
}

// importStatuses are the statuses a CSV may carry.
var importStatuses = []core.ContactStatus{
	core.StatusActive, core.StatusUnsubscribed, core.StatusBounced, core.StatusComplained,
}

// parseStatus parses the status column ("" = not given).
func parseStatus(v string) (core.ContactStatus, error) {
	if v == "" {
		return "", nil
	}
	st := core.ContactStatus(strings.ToLower(v))
	for _, ok := range importStatuses {
		if st == ok {
			return st, nil
		}
	}
	return "", core.Invalid(ColStatus, "must be active, unsubscribed, bounced or complained")
}

// statusRank orders statuses by how strongly they block sending; merging
// rows keeps the most blocking one.
func statusRank(s core.ContactStatus) int {
	switch s {
	case core.StatusActive:
		return 1
	case core.StatusUnsubscribed:
		return 2
	case core.StatusBounced:
		return 3
	case core.StatusComplained:
		return 4
	}
	return 0
}

// parseTags splits the tags column on semicolons.
func parseTags(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	var names []string
	for _, t := range strings.Split(v, TagSeparator) {
		if t = strings.TrimSpace(t); t != "" {
			names = append(names, t)
		}
	}
	return contactstore.NormalizeTags(names)
}

// parseConsentSource checks the consent_source column.
func parseConsentSource(v string) (string, error) {
	if utf8.RuneCountInString(v) > MaxConsentSourceLength {
		return "", core.Invalid(ColConsentSource, "is longer than %d characters", MaxConsentSourceLength)
	}
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", core.Invalid(ColConsentSource, "contains control characters")
	}
	return v, nil
}

// consentDateLayouts are the accepted consent_date formats.
var consentDateLayouts = []string{time.RFC3339Nano, time.DateOnly}

// maxConsentAhead is how far in the future a consent date may be (clock
// skew and time zones).
const maxConsentAhead = 24 * time.Hour

// parseConsentDate parses consent_date: RFC 3339 or YYYY-MM-DD (midnight
// UTC). "" = not given.
func parseConsentDate(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	for _, layout := range consentDateLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			t = t.UTC()
			if t.After(now.Add(maxConsentAhead)) {
				return time.Time{}, core.Invalid(ColConsentDate, "is in the future")
			}
			return t, nil
		}
	}
	return time.Time{}, core.Invalid(ColConsentDate, "must be RFC 3339 (2024-03-01T10:00:00Z) or YYYY-MM-DD")
}

// merge folds a later row for the same address into r: tags are united,
// later non-empty values win, and the most blocking status is kept.
func (r *record) merge(o *record) {
	r.merged = true
	if o.firstName != "" {
		r.firstName = o.firstName
	}
	if o.lastName != "" {
		r.lastName = o.lastName
	}
	r.raiseStatus(o.status)
	if o.consentSource != "" {
		r.consentSource = o.consentSource
	}
	if !o.consentDate.IsZero() {
		r.consentDate = o.consentDate
	}
	if len(o.tags) > 0 {
		have := make(map[string]bool, len(r.tags))
		for _, t := range r.tags {
			have[foldASCII(t)] = true
		}
		for _, t := range o.tags {
			if !have[foldASCII(t)] {
				have[foldASCII(t)] = true
				r.tags = append(r.tags, t)
			}
		}
	}
	if len(o.attrs) > 0 {
		if r.attrs == nil {
			r.attrs = map[string]string{}
		}
		maps.Copy(r.attrs, o.attrs)
	}
}

// tagCanon spells every tag of a file the way it first appears in it, so
// "VIP" and "vip" count as one tag (tags are case-insensitive).
type tagCanon map[string]string

func (c tagCanon) apply(tags []string) []string {
	for i, t := range tags {
		k := foldASCII(t)
		if first, ok := c[k]; ok {
			tags[i] = first
		} else {
			c[k] = t
		}
	}
	return tags
}

// foldASCII lowercases ASCII letters only, like SQLite's NOCASE collation
// that the tags table uses.
func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// anyMap converts string attributes for contactstore.
func anyMap(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// reasonOf turns an error into a rejection reason: "field message" for
// validation errors, the error text otherwise. Tag errors from
// contactstore quote the tag, so they become the fixed ReasonBadTag.
func reasonOf(err error) string {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		switch {
		case ve.Field == ColTags:
			return ReasonBadTag
		case ve.Field == "":
			return ve.Message
		}
		return ve.Field + " " + ve.Message
	}
	return err.Error()
}
