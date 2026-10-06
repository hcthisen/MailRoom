package api

import (
	"fmt"
	"strconv"
	"strings"

	"mailroom/internal/core"
)

// ETag returns the entity tag of a resource version: "v12" (with quotes).
// Single-resource GETs send it in the ETag header.
func ETag(version int64) string {
	return `"v` + strconv.FormatInt(version, 10) + `"`
}

// ParseIfMatch parses an If-Match header into a version. It accepts
// "v12", W/"v12" and bare v12. An empty header or "*" returns 0, meaning
// "no precondition". Anything else is a validation error for field
// "header.If-Match".
func ParseIfMatch(header string) (int64, error) {
	h := strings.TrimSpace(header)
	if h == "" || h == "*" {
		return 0, nil
	}
	h = strings.TrimPrefix(h, "W/")
	if len(h) >= 2 && h[0] == '"' && h[len(h)-1] == '"' {
		h = h[1 : len(h)-1]
	}
	if len(h) < 2 || h[0] != 'v' {
		return 0, invalidIfMatch()
	}
	v, err := strconv.ParseInt(h[1:], 10, 64)
	if err != nil || v <= 0 {
		return 0, invalidIfMatch()
	}
	return v, nil
}

func invalidIfMatch() error {
	return core.Invalid("header.If-Match", `must be an ETag from this API, e.g. "v3"`)
}

// IfMatchParam is embedded in Huma inputs of PATCH/PUT/DELETE operations
// on versioned resources.
type IfMatchParam struct {
	IfMatch string `header:"If-Match" doc:"ETag of the version you last read (e.g. \"v3\"). If the resource changed since, the request fails with 412 instead of overwriting the change." example:"\"v3\""`
}

// Version parses the If-Match header (0 = no precondition).
func (p IfMatchParam) Version() (int64, error) { return ParseIfMatch(p.IfMatch) }

// CheckVersion returns a core.ErrPrecondition error when ifVersion is set
// and differs from the current version.
func CheckVersion(ifVersion, current int64) error {
	if ifVersion != 0 && ifVersion != current {
		return fmt.Errorf("resource is at version %d, not %d (If-Match): %w", current, ifVersion, core.ErrPrecondition)
	}
	return nil
}
