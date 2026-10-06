package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/core"
)

// Error codes used in the "code" field of every error response. The set is
// closed (ARCHITECTURE.md §7); CodeForStatus folds every other HTTP error
// into it.
const (
	CodeNotFound           = "not_found"
	CodeConflict           = "conflict"
	CodeValidation         = "validation"
	CodePreconditionFailed = "precondition_failed"
	CodeForbidden          = "forbidden"
	CodeUnauthorized       = "unauthorized"
	CodeRateLimited        = "rate_limited"
	CodeInternal           = "internal"
)

// ErrUnauthorized marks missing, malformed, unknown, revoked or expired
// credentials. Authenticators wrap it; the API answers 401.
var ErrUnauthorized = errors.New("unauthorized")

// ErrorBody is the content of the "error" member of an error response.
type ErrorBody struct {
	Code    string `json:"code" doc:"Machine-readable error code" enum:"not_found,conflict,validation,precondition_failed,forbidden,unauthorized,rate_limited,internal" example:"validation"`
	Message string `json:"message" doc:"Human-readable explanation" example:"is not a valid email address"`
	Field   string `json:"field,omitempty" doc:"The offending input field, when the error is about one (e.g. email, query.limit, header.If-Match)" example:"email"`
}

// Error is the single error format of the API:
//
//	{"error": {"code": "...", "message": "...", "field": "..."}}
//
// It implements huma.StatusError, so handlers may return it directly.
type Error struct {
	status int
	Body   ErrorBody `json:"error"`
}

var _ huma.StatusError = (*Error)(nil)

// NewError returns an error response with the given status, code, message
// and optional field.
func NewError(status int, code, message, field string) *Error {
	return &Error{status: status, Body: ErrorBody{Code: code, Message: message, Field: field}}
}

// Error implements error.
func (e *Error) Error() string {
	if e.Body.Field != "" {
		return e.Body.Code + ": " + e.Body.Field + ": " + e.Body.Message
	}
	return e.Body.Code + ": " + e.Body.Message
}

// GetStatus returns the HTTP status code (huma.StatusError).
func (e *Error) GetStatus() int { return e.status }

// Write writes e as a JSON response. Plain net/http handlers (public
// endpoints, middleware) use it to answer in the API's error format.
func (e *Error) Write(w http.ResponseWriter) {
	b, err := json.Marshal(e)
	if err != nil {
		b = []byte(`{"error":{"code":"internal","message":"internal server error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.status)
	_, _ = w.Write(append(b, '\n'))
}

// CodeForStatus returns the error code for an HTTP status: 401, 403, 404,
// 409, 412 and 429 have their own codes; 405 (no such operation for this
// method) is not_found; 428 is precondition_failed; every other 4xx
// (malformed JSON, body too large, unsupported media type, ...) is
// validation, and 5xx is internal.
func CodeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusPreconditionFailed, http.StatusPreconditionRequired:
		return CodePreconditionFailed
	case http.StatusTooManyRequests:
		return CodeRateLimited
	}
	if status >= 500 {
		return CodeInternal
	}
	return CodeValidation
}

// internalMessage is the only message ever sent for a 500, so internal
// details (SQL, paths) never leak to clients.
const internalMessage = "internal server error"

var rxRequiredProperty = regexp.MustCompile(`^expected required property (\S+) to be present$`)

// init replaces Huma's error constructor, so every error Huma produces
// itself (request validation, unknown content type, body too large, ...)
// uses the Mailroom format too.
func init() {
	huma.NewError = fromHuma
}

// fromHuma converts Huma's (status, message, details) into an *Error. The
// first detail's location becomes the field ("body." is stripped), and the
// details' messages become the message.
func fromHuma(status int, msg string, errs ...error) huma.StatusError {
	e := NewError(status, CodeForStatus(status), msg, "")
	if status >= 500 {
		e.Body.Message = internalMessage
		return e
	}
	var parts []string
	for _, err := range errs {
		if err == nil {
			continue
		}
		var loc, m string
		var d huma.ErrorDetailer
		if errors.As(err, &d) {
			det := d.ErrorDetail()
			loc, m = det.Location, det.Message
		} else {
			m = err.Error()
		}
		field := fieldFromLocation(loc)
		if sm := rxRequiredProperty.FindStringSubmatch(m); sm != nil {
			field = joinField(field, sm[1])
			m = "is required"
		}
		if e.Body.Field == "" {
			e.Body.Field = field
		}
		if field != "" && len(errs) > 1 {
			m = field + ": " + m
		}
		parts = append(parts, m)
	}
	if len(parts) > 0 {
		e.Body.Message = strings.Join(parts, "; ")
	}
	if e.Body.Message == "" {
		e.Body.Message = strings.ToLower(http.StatusText(status))
	}
	return e
}

// fieldFromLocation turns a Huma location ("body.sender.from_email",
// "query.limit", "body") into a field name ("sender.from_email",
// "query.limit", "").
func fieldFromLocation(loc string) string {
	if loc == "body" {
		return ""
	}
	return strings.TrimPrefix(loc, "body.")
}

func joinField(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// MapError converts err into an API error:
//
//	core.ErrNotFound      404 not_found
//	core.ErrConflict      409 conflict
//	core.ErrSuppressed    409 conflict (message "... address suppressed")
//	core.ErrValidation    422 validation (field from *core.ValidationError)
//	core.ErrPrecondition  412 precondition_failed
//	core.ErrForbidden     403 forbidden
//	ErrUnauthorized       401 unauthorized
//
// An error that already is a huma.StatusError is returned unchanged, and
// anything else becomes a 500 with a generic message. MapError(nil) is nil.
func MapError(err error) huma.StatusError {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return NewError(http.StatusUnprocessableEntity, CodeValidation, ve.Message, ve.Field)
	}
	switch {
	case errors.Is(err, core.ErrSuppressed):
		return NewError(http.StatusConflict, CodeConflict, err.Error(), "")
	case errors.Is(err, core.ErrNotFound):
		return NewError(http.StatusNotFound, CodeNotFound, err.Error(), "")
	case errors.Is(err, core.ErrConflict):
		return NewError(http.StatusConflict, CodeConflict, err.Error(), "")
	case errors.Is(err, core.ErrValidation):
		return NewError(http.StatusUnprocessableEntity, CodeValidation, err.Error(), "")
	case errors.Is(err, core.ErrPrecondition):
		return NewError(http.StatusPreconditionFailed, CodePreconditionFailed, err.Error(), "")
	case errors.Is(err, core.ErrForbidden):
		return NewError(http.StatusForbidden, CodeForbidden, err.Error(), "")
	case errors.Is(err, ErrUnauthorized):
		return NewError(http.StatusUnauthorized, CodeUnauthorized, err.Error(), "")
	}
	var se huma.StatusError
	if errors.As(err, &se) {
		return se
	}
	return NewError(http.StatusInternalServerError, CodeInternal, internalMessage, "")
}
