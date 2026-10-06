package api

import (
	"encoding/base64"
	"strconv"
	"strings"

	"mailroom/internal/core"
)

// Pagination limits (G-5).
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// cursorPrefix versions the opaque cursor format.
const cursorPrefix = "id:"

// EncodeCursor returns the opaque cursor for a page ending at lastID.
func EncodeCursor(lastID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(lastID, 10)))
}

// DecodeCursor returns the ID encoded by EncodeCursor. An empty cursor is
// 0 (first page); a malformed one is a validation error for "query.cursor".
func DecodeCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, invalidCursor()
	}
	rest, ok := strings.CutPrefix(string(raw), cursorPrefix)
	if !ok {
		return 0, invalidCursor()
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, invalidCursor()
	}
	return id, nil
}

func invalidCursor() error {
	return core.Invalid("query.cursor", "is not a cursor returned by this API")
}

// PageParams is embedded in Huma inputs of list operations.
type PageParams struct {
	Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"50" doc:"Maximum number of items to return (1-500)." example:"50"`
	Cursor string `query:"cursor" maxLength:"128" doc:"Opaque cursor: the next_cursor of the previous page. Omit for the first page."`
}

// PageLimit returns the requested page size, defaulted and clamped.
func (p PageParams) PageLimit() int {
	switch {
	case p.Limit <= 0:
		return DefaultLimit
	case p.Limit > MaxLimit:
		return MaxLimit
	}
	return p.Limit
}

// After decodes the cursor: the last ID of the previous page, or 0.
func (p PageParams) After() (int64, error) { return DecodeCursor(p.Cursor) }

// ListBody is the body of every list response.
type ListBody[T any] struct {
	Items      []T    `json:"items" doc:"The items of this page"`
	NextCursor string `json:"next_cursor" doc:"Cursor for the next page; empty on the last page"`
}

// ListOutput is the output of a list operation.
type ListOutput[T any] struct {
	Body ListBody[T]
}

// NewList builds a page from items fetched with LIMIT limit+1: when more
// than limit items are given, the extra one is dropped and next_cursor
// points after the last kept item (id returns an item's ID). Items is
// never nil, so it encodes as [].
func NewList[T any](items []T, limit int, id func(T) int64) ListBody[T] {
	if items == nil {
		items = []T{}
	}
	body := ListBody[T]{Items: items}
	if limit > 0 && len(items) > limit {
		body.Items = items[:limit]
		body.NextCursor = EncodeCursor(id(body.Items[limit-1]))
	}
	return body
}
