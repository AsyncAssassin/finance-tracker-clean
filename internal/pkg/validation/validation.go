// Package validation collects the problems of a request field by field.
// The services report invalid input with an *Error, and their transports
// turn it into a list of field violations for the client.
package validation

import (
	"errors"
	"slices"
	"strings"
)

// ErrValidation is matched by every *Error through errors.Is.
var ErrValidation = errors.New("validation failed")

// Error lists the invalid fields of a request and the reason for each.
type Error struct {
	Fields map[string]string
}

// Add records the first problem found for field; later ones are dropped.
func (e *Error) Add(field, problem string) {
	if e.Fields == nil {
		e.Fields = map[string]string{}
	}
	if _, seen := e.Fields[field]; !seen {
		e.Fields[field] = problem
	}
}

// ErrOrNil returns e as an error when it has fields and nil otherwise.
// Returning e directly would produce a non-nil error holding a nil pointer.
func (e *Error) ErrOrNil() error {
	if e == nil || len(e.Fields) == 0 {
		return nil
	}
	return e
}

// SortedFields returns the names of the invalid fields in order, so
// messages and lists built from them are stable.
func (e *Error) SortedFields() []string {
	fields := make([]string, 0, len(e.Fields))
	for f := range e.Fields {
		fields = append(fields, f)
	}
	slices.Sort(fields)
	return fields
}

func (e *Error) Error() string {
	fields := e.SortedFields()
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, f+": "+e.Fields[f])
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrValidation) report true.
func (e *Error) Is(target error) bool { return target == ErrValidation }
