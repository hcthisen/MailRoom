package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// TimeFormat is the fixed-width UTC format of every TEXT time column. Being
// fixed-width, stored times sort and compare lexicographically.
const TimeFormat = "2006-01-02T15:04:05.000000Z"

// FormatTime formats t in UTC with microsecond precision.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeFormat)
}

// ParseTime parses a value written by FormatTime. RFC 3339 values are also
// accepted, for data written by hand or by SQLite's own functions.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeFormat, s)
	if err == nil {
		return t, nil
	}
	if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
		return t2.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("db: parse time %q: %w", s, err)
}

// NullTime returns nil (SQL NULL) for a nil pointer, else FormatTime(*t).
func NullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}

// ParseNullTime parses a nullable time column; NULL yields nil.
func ParseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := ParseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// NullID returns nil (SQL NULL) for id 0, else id. Use it for nullable
// foreign keys where 0 means "none".
func NullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// NullString returns nil (SQL NULL) for "", else s.
func NullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// MarshalJSON encodes v for a JSON TEXT column. A nil map or nil value
// encodes as "{}" and a nil slice as "[]", so json_valid() checks and
// column defaults stay consistent.
func MarshalJSON(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "{}", nil
	case map[string]any:
		if x == nil {
			return "{}", nil
		}
	case []int64:
		if x == nil {
			return "[]", nil
		}
	case []string:
		if x == nil {
			return "[]", nil
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("db: marshal json: %w", err)
	}
	return string(b), nil
}

// UnmarshalJSON decodes a JSON TEXT column into v. An empty string leaves
// v untouched.
func UnmarshalJSON(s string, v any) error {
	if s == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("db: unmarshal json: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a UNIQUE or PRIMARY KEY
// constraint failure.
func IsUniqueViolation(err error) bool {
	code := extendedCode(err)
	return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// IsForeignKeyViolation reports whether err is a FOREIGN KEY constraint failure.
func IsForeignKeyViolation(err error) bool {
	return extendedCode(err) == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

// IsConstraintViolation reports whether err is any constraint failure
// (UNIQUE, CHECK, NOT NULL, FOREIGN KEY, ...).
func IsConstraintViolation(err error) bool {
	return extendedCode(err)&0xff == sqlite3.SQLITE_CONSTRAINT
}

func extendedCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()
	}
	return 0
}
