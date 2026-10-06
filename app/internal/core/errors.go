package core

import (
	"errors"
	"fmt"
)

// Sentinel errors. Packages wrap them with fmt.Errorf("...: %w", ErrX) and
// the API layer maps them to HTTP status codes.
var (
	ErrNotFound     = errors.New("not found")           // 404
	ErrConflict     = errors.New("conflict")            // 409
	ErrValidation   = errors.New("validation")          // 422; prefer ValidationError
	ErrPrecondition = errors.New("precondition failed") // 412 (If-Match mismatch)
	ErrForbidden    = errors.New("forbidden")           // 403
	ErrSuppressed   = errors.New("address suppressed")
)

// ValidationError reports an invalid input field. It wraps ErrValidation,
// so errors.Is(err, ErrValidation) holds.
type ValidationError struct {
	Field   string
	Message string
}

// Error implements error.
func (e *ValidationError) Error() string {
	if e.Field == "" {
		return "validation: " + e.Message
	}
	return fmt.Sprintf("validation: %s: %s", e.Field, e.Message)
}

// Unwrap returns ErrValidation.
func (e *ValidationError) Unwrap() error { return ErrValidation }

// Invalid returns a *ValidationError for field with a formatted message.
func Invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// ProviderError is returned (possibly wrapped) by EmailProvider
// implementations. Retryable errors (throttling, 5xx) are retried with
// backoff; others fail the message immediately.
type ProviderError struct {
	Retryable bool
	Code      string // provider error code, e.g. "MessageRejected", "Throttling"
	Message   string
}

// Error implements error.
func (e *ProviderError) Error() string {
	kind := "permanent"
	if e.Retryable {
		kind = "retryable"
	}
	return fmt.Sprintf("provider error (%s) %s: %s", kind, e.Code, e.Message)
}
