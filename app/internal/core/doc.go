// Package core holds Mailroom's shared domain types, enums, sentinel errors
// and the cross-package interfaces that let W1/W2 packages cooperate without
// importing each other (see ARCHITECTURE.md §4.1 and §4.2).
//
// core is a leaf package: it imports only the standard library.
package core
