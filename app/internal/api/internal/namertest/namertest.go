// Package namertest holds a type whose name clashes with one in package api,
// for the schema namer test.
package namertest

// Enrollment has the same Go name as a test type in package api.
type Enrollment struct {
	Status string `json:"status"`
}
