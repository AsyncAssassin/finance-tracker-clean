package money

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    Money
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "1", want: 100},
		{in: "1250.50", want: 125050},
		{in: "1250.5", want: 125050},
		{in: "1250,5", want: 125050},
		{in: " 7 ", want: 700},
		{in: "0.01", want: 1},
		{in: ".5", want: 50},
		{in: "-5", want: -500},
		{in: "+5", want: 500},
		{in: "1e3", want: 100000},
		{in: "1000000000000", want: Max},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "1.005", wantErr: true},
		{in: "1 000", wantErr: true},
		{in: "1000000000000.01", wantErr: true},
		{in: "1e30", wantErr: true},
		{in: "1e1000000000", wantErr: true},
		{in: "1e-1000000000", wantErr: true},
		{in: "1e9999999999999999999999999999", wantErr: true},
		{in: "0e9999999999999999999999999999", want: 0},
		{in: "100e-4", want: 1},
		{in: "1.2300", want: 123},
		{in: "0.000000000000000000000000001e27", want: 100},
		{in: "100000000000000000000000000e-28", want: 1},
		{in: "1" + strings.Repeat("0", 40), wantErr: true},
		{in: strings.Repeat("0", 31) + "1", want: 100}, // long but within the length limit
		{in: "1e-3", wantErr: true},
		{in: "0x10", wantErr: true},
		{in: "1/2", wantErr: true},
		{in: "NaN", wantErr: true},
		{in: "Inf", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want error", tc.in, got)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("Parse(%q) error %v does not wrap ErrInvalid", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   Money
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{100, "1.00"},
		{125050, "1250.50"},
		{-5, "-0.05"},
		{-125050, "-1250.50"},
		{Max, "1000000000000.00"},
		{Money(math.MinInt64), "-92233720368547758.08"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Money(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := Money(125050).Float64(); got != 1250.5 {
		t.Errorf("Float64() = %v, want 1250.5", got)
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()
	type payload struct {
		Amount Money `json:"amount"`
	}
	out, err := json.Marshal(payload{Amount: 125050})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"amount":1250.50}` {
		t.Errorf("Marshal = %s, want {\"amount\":1250.50}", out)
	}

	tests := []struct {
		name    string
		in      string
		want    Money
		wantErr bool
	}{
		{name: "number", in: `{"amount": 1250.5}`, want: 125050},
		{name: "integer", in: `{"amount": 7}`, want: 700},
		{name: "string with comma", in: `{"amount": "1250,50"}`, want: 125050},
		{name: "null keeps the old value", in: `{"amount": null}`, want: 42},
		{name: "missing keeps the old value", in: `{}`, want: 42},
		{name: "text", in: `{"amount": "abc"}`, wantErr: true},
		{name: "three decimals", in: `{"amount": 1.005}`, wantErr: true},
		{name: "wrong type", in: `{"amount": true}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := payload{Amount: 42}
			err := json.Unmarshal([]byte(tc.in), &p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = %v, want error", tc.in, p.Amount)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) returned error: %v", tc.in, err)
			}
			if p.Amount != tc.want {
				t.Fatalf("Unmarshal(%s) = %d, want %d", tc.in, p.Amount, tc.want)
			}
		})
	}
}

func TestMustParsePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("MustParse(\"x\") did not panic")
		}
	}()
	MustParse("x")
}

// A short numeric string must not allocate integers proportional to its exponent.
func BenchmarkParseLargeExponent(b *testing.B) {
	for _, input := range []string{"1e100000", "1e-100000"} {
		b.Run(input, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Parse(input); err == nil {
					b.Fatal("expected an invalid amount")
				}
			}
		})
	}
}

func TestAddDetectsOverflow(t *testing.T) {
	for _, tc := range []struct {
		a, b, want Money
		overflow   bool
	}{
		{Money(math.MaxInt64), 1, 0, true}, {Money(math.MinInt64), -1, 0, true},
		{Money(math.MaxInt64) - 1, 1, Money(math.MaxInt64), false},
		{Money(math.MinInt64) + 1, -1, Money(math.MinInt64), false},
		{125050, -50, 125000, false},
	} {
		got, err := Add(tc.a, tc.b)
		if got != tc.want || errors.Is(err, ErrOverflow) != tc.overflow {
			t.Fatalf("Add(%v,%v) = %v, %v", tc.a, tc.b, got, err)
		}
	}
}

func FuzzParseRoundTrip(f *testing.F) {
	for _, s := range []string{"1250.50", "1e100000", "0e-9999999", "100e-4", "1000000000000", "1.005"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := Parse(s)
		if err != nil {
			return
		}
		if m > Max || m < -Max {
			t.Fatal("amount out of range")
		}
		back, err := Parse(m.String())
		if err != nil || back != m {
			t.Fatalf("round trip failed for %q", s)
		}
	})
}
