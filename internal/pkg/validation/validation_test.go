package validation

import (
	"errors"
	"slices"
	"testing"
)

func TestError(t *testing.T) {
	t.Parallel()
	v := new(Error)
	if v.ErrOrNil() != nil {
		t.Fatal("empty Error.ErrOrNil() is not nil")
	}
	var nilErr *Error
	if nilErr.ErrOrNil() != nil {
		t.Fatal("nil *Error.ErrOrNil() is not nil")
	}
	v.Add("category", "must not be empty")
	v.Add("amount", "must be greater than 0")
	v.Add("amount", "second problem is dropped")
	err := v.ErrOrNil()
	if err == nil {
		t.Fatal("ErrOrNil() = nil with fields")
	}
	want := "validation failed: amount: must be greater than 0; category: must not be empty"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, ErrValidation) {
		t.Error("errors.Is(err, ErrValidation) = false")
	}
	if got := v.SortedFields(); !slices.Equal(got, []string{"amount", "category"}) {
		t.Errorf("SortedFields() = %v", got)
	}
}
