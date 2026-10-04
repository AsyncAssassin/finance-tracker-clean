// Package money represents amounts as whole kopecks, so arithmetic and
// comparisons are exact: 0.10 + 0.20 is exactly 0.30.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Money is an amount in kopecks, hundredths of a ruble. Arithmetic results
// such as a remaining balance may be negative; amounts and limits accepted
// from clients are always positive.
type Money int64

// Ruble is the number of kopecks in one ruble.
const Ruble Money = 100

// Max is the largest supported individual amount, one trillion rubles.
// Aggregates still require overflow checks.
const Max Money = 1_000_000_000_000 * Ruble

// ErrInvalid is wrapped by every error returned by Parse.
var ErrInvalid = errors.New("invalid amount")

// decimalRe accepts plain decimal numbers with an optional sign and exponent.
var decimalRe = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// maxInputLen bounds decimal parsing work. The exponent is bounded separately
// because a short scientific notation can describe an enormous integer.
const maxInputLen = 32

var hundred = big.NewRat(100, 1)

// Parse converts a decimal string such as "1250.50", "1250,5" or "1250" to
// Money without going through floating point. The amount must have at most
// two decimal places and be within ±Max.
func Parse(s string) (Money, error) {
	t := strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
	if len(t) > maxInputLen {
		return 0, fmt.Errorf("%w: input is too long", ErrInvalid)
	}
	if !decimalRe.MatchString(t) {
		return 0, fmt.Errorf("%w: %q is not a decimal number", ErrInvalid, s)
	}
	if i := strings.IndexAny(t, "eE"); i >= 0 {
		if strings.Trim(t[:i], "+-0.") == "" {
			return 0, nil
		}
		exponent, err := strconv.Atoi(t[i+1:])
		// With at most maxInputLen mantissa characters, a larger exponent
		// cannot produce a nonzero amount in range with whole kopecks.
		if err != nil || exponent < -maxInputLen || exponent > maxInputLen {
			return 0, fmt.Errorf("%w: exponent is out of range", ErrInvalid)
		}
	}
	r, ok := new(big.Rat).SetString(t)
	if !ok {
		return 0, fmt.Errorf("%w: %q is not a decimal number", ErrInvalid, s)
	}
	r.Mul(r, hundred)
	if !r.IsInt() {
		return 0, fmt.Errorf("%w: %q has more than two decimal places", ErrInvalid, s)
	}
	n := r.Num()
	if !n.IsInt64() || n.Int64() > int64(Max) || n.Int64() < -int64(Max) {
		return 0, fmt.Errorf("%w: %q is out of range", ErrInvalid, s)
	}
	return Money(n.Int64()), nil
}

// MustParse is Parse for constants in tests and examples. It panics on error.
func MustParse(s string) Money {
	m, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return m
}

// String formats m with exactly two decimal places, for example "1250.50"
// or "-0.05".
func (m Money) String() string {
	v := uint64(m)
	sign := ""
	if m < 0 {
		sign = "-"
		v = uint64(-(m + 1)) + 1
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// Float64 returns m in rubles as a float, for percentages and display only.
func (m Money) Float64() float64 {
	return float64(m) / float64(Ruble)
}

// MarshalJSON writes m as a JSON number with two decimal places, e.g. 1250.50.
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(m.String()), nil
}

// UnmarshalJSON accepts a JSON number (1250.5) or a string ("1250,50").
// JSON null leaves m unchanged.
func (m *Money) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return nil
	}
	if len(s) > 0 && s[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// ErrOverflow means an aggregate cannot be represented as signed kopecks.
var ErrOverflow = errors.New("aggregate amount exceeds supported range")

// Add sums two amounts without wrapping int64.
func Add(a, b Money) (Money, error) {
	if b > 0 && a > Money(math.MaxInt64)-b || b < 0 && a < Money(math.MinInt64)-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}
