package imports

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"mailroom/internal/core"
)

// CSV column names (I-1). Attribute columns are AttrPrefix + key.
const (
	ColEmail         = "email"
	ColFirstName     = "first_name"
	ColLastName      = "last_name"
	ColStatus        = "status"
	ColTags          = "tags"
	ColConsentSource = "consent_source"
	ColConsentDate   = "consent_date"
	AttrPrefix       = "attr."
)

// TagSeparator separates tags within the tags column.
const TagSeparator = ";"

// maxWarnings bounds the warnings of one report.
const maxWarnings = 100

// sniffBytes is how much of the file is looked at to choose the delimiter.
const sniffBytes = 64 << 10

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// attrColumn is one attr.<key> column.
type attrColumn struct {
	index int
	key   string
}

// header maps the known columns to their positions (-1 = absent).
type header struct {
	email, firstName, lastName, status int
	tags, consentSource, consentDate   int

	attrs    []attrColumn
	width    int      // number of columns
	warnings []string // ignored columns
}

// fileError is a problem with the file as a whole (422, field "file").
func fileError(format string, args ...any) error {
	return core.Invalid("file", format, args...)
}

// openCSV prepares a CSV reader over r: it skips a UTF-8 byte order mark,
// picks "," or ";" as the delimiter from the header line, and reads and
// checks the header row.
func openCSV(r io.Reader) (*csv.Reader, header, error) {
	br := bufio.NewReaderSize(r, sniffBytes)
	if b, _ := br.Peek(len(utf8BOM)); bytes.Equal(b, utf8BOM) {
		_, _ = br.Discard(len(utf8BOM))
	}
	sniff, err := br.Peek(sniffBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, header{}, readError(err)
	}
	cr := csv.NewReader(br)
	cr.Comma = detectDelimiter(sniff)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true

	names, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, header{}, fileError("the file is empty; it needs a header row with at least an email column")
	}
	if err != nil {
		return nil, header{}, readError(err)
	}
	h, err := parseHeader(names)
	if err != nil {
		return nil, header{}, err
	}
	return cr, h, nil
}

// readError describes an error reading the CSV. Size-limit errors pass
// through unchanged.
func readError(err error) error {
	if errors.Is(err, errFileTooLarge) {
		return err
	}
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return fileError("the file is not valid CSV: line %d, column %d: %v", pe.Line, pe.Column, pe.Err)
	}
	return fmt.Errorf("imports: read CSV: %w", err)
}

// detectDelimiter returns ';' when the first line has more semicolons than
// commas outside quotes, and ',' otherwise.
func detectDelimiter(b []byte) rune {
	commas, semis := 0, 0
	inQuotes := false
	for _, c := range b {
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case inQuotes:
		case c == '\n' || c == '\r':
			if semis > commas {
				return ';'
			}
			return ','
		case c == ',':
			commas++
		case c == ';':
			semis++
		}
	}
	if semis > commas {
		return ';'
	}
	return ','
}

// parseHeader maps the header row's columns. Column names are matched
// case-insensitively; attribute keys keep their case. Unknown and unnamed
// columns are ignored with a warning.
func parseHeader(names []string) (header, error) {
	h := header{email: -1, firstName: -1, lastName: -1, status: -1, tags: -1, consentSource: -1, consentDate: -1, width: len(names)}
	if len(names) > MaxColumns {
		return header{}, fileError("the header row has %d columns; at most %d are allowed", len(names), MaxColumns)
	}
	known := map[string]*int{
		ColEmail: &h.email, ColFirstName: &h.firstName, ColLastName: &h.lastName, ColStatus: &h.status,
		ColTags: &h.tags, ColConsentSource: &h.consentSource, ColConsentDate: &h.consentDate,
	}
	seenAttr := map[string]bool{}
	for i, raw := range names {
		if !utf8.ValidString(raw) {
			return header{}, fileError("the header row is not valid UTF-8; save the file as UTF-8")
		}
		name := strings.TrimSpace(raw)
		lower := strings.ToLower(name)
		if pos, ok := known[lower]; ok {
			if *pos >= 0 {
				return header{}, fileError("column %q appears more than once", lower)
			}
			*pos = i
			continue
		}
		switch {
		case name == "":
			h.warn("column %d has no name and was ignored", i+1)
		case strings.HasPrefix(lower, AttrPrefix):
			key := strings.TrimSpace(name[len(AttrPrefix):])
			if !validAttrKey(key) {
				h.warn("%s was ignored: attribute keys are 1-64 letters, digits or underscores", columnLabel(name, i))
				continue
			}
			if seenAttr[key] {
				return header{}, fileError("column %q appears more than once", AttrPrefix+key)
			}
			seenAttr[key] = true
			h.attrs = append(h.attrs, attrColumn{index: i, key: key})
		default:
			h.warn("unknown %s was ignored", columnLabel(name, i))
		}
	}
	if h.email < 0 {
		return header{}, fileError("the header row has no %q column; the first row must name the columns", ColEmail)
	}
	return h, nil
}

func (h *header) warn(format string, args ...any) {
	if len(h.warnings) < maxWarnings {
		h.warnings = append(h.warnings, fmt.Sprintf(format, args...))
	}
}

// validAttrKey reports whether k is 1-64 of [A-Za-z0-9_].
func validAttrKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// columnLabel names column i (0-based) in a warning. A name that looks
// like an address (a file without a header row, or a stray cell in it) is
// replaced by the column's number, so warnings, which are kept in the
// report, never hold an address.
func columnLabel(name string, i int) string {
	if strings.Contains(name, "@") {
		return fmt.Sprintf("column %d", i+1)
	}
	return fmt.Sprintf("column %q", clip(name))
}

// clip shortens a column name for a message.
func clip(s string) string {
	const max = 64
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
