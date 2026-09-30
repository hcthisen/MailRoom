package api

import (
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// newSchemaNamer returns a Huma schema namer that never gives two different Go
// types the same OpenAPI schema name. huma.DefaultSchemaNamer names a schema
// after its Go type only, so e.g. contacts.Enrollment and sequences.Enrollment
// would collide and make Huma panic while the app wires its routes. The first
// type to claim a name keeps it; later types with the same name are prefixed
// with their package name ("SequencesEnrollment"), then numbered if needed.
// The same type always maps to the same name.
func newSchemaNamer() func(t reflect.Type, hint string) string {
	var mu sync.Mutex
	owners := map[string]reflect.Type{}
	byType := map[reflect.Type]string{}

	return func(t reflect.Type, hint string) string {
		base := t
		for base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		name := huma.DefaultSchemaNamer(t, hint)

		mu.Lock()
		defer mu.Unlock()
		if n, ok := byType[base]; ok && strings.HasSuffix(n, name) {
			return n
		}
		claim := func(n string) bool {
			if owner, ok := owners[n]; ok && owner != base {
				return false
			}
			owners[n] = base
			byType[base] = n
			return true
		}
		if claim(name) {
			return name
		}
		prefix := exportedPkgName(base)
		alt := prefix + name
		for i := 2; !claim(alt); i++ {
			alt = fmt.Sprintf("%s%s%d", prefix, name, i)
		}
		return alt
	}
}

// exportedPkgName turns a type's package path into a capitalised identifier
// prefix ("mailroom/internal/sequences" -> "Sequences").
func exportedPkgName(t reflect.Type) string {
	p := path.Base(t.PkgPath())
	if p == "" || p == "." {
		return "X"
	}
	var b strings.Builder
	up := true
	for _, r := range p {
		switch {
		case r == '_' || r == '-' || r == '.':
			up = true
		case up:
			b.WriteString(strings.ToUpper(string(r)))
			up = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
