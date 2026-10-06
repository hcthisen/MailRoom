package core

import "time"

// Clock is the only source of the current time in Mailroom, so tests can
// fake it. Implementations return UTC times.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real wall clock.
type SystemClock struct{}

// Now returns the current time in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }
