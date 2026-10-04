package civil

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestNewNormalizes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  Date
		want Date
	}{
		{"day overflow", New(2026, time.February, 30), New(2026, time.March, 2)},
		{"day zero is the last day of the previous month", New(2026, time.March, 0), New(2026, time.February, 28)},
		{"leap year", New(2024, time.March, 0), New(2024, time.February, 29)},
		{"month overflow", New(2026, time.December+1, 1), New(2027, time.January, 1)},
	}
	for _, tc := range tests {
		if !tc.got.Equal(tc.want) {
			t.Errorf("%s: got %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestOfUsesTheLocationOfTheTime(t *testing.T) {
	t.Parallel()
	moscow := time.FixedZone("MSK", 3*60*60)
	late := time.Date(2026, time.November, 1, 1, 0, 0, 0, moscow) // 2026-10-31 22:00 UTC
	if got := Of(late); !got.Equal(New(2026, time.November, 1)) {
		t.Errorf("Of(%v) = %s, want 2026-11-01", late, got)
	}
	if got := Of(late.UTC()); !got.Equal(New(2026, time.October, 31)) {
		t.Errorf("Of(%v) = %s, want 2026-10-31", late.UTC(), got)
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	d, err := Parse("2026-10-03")
	if err != nil {
		t.Fatal(err)
	}
	if d.Year() != 2026 || d.Month() != time.October || d.Day() != 3 {
		t.Errorf("Parse = %d-%d-%d", d.Year(), d.Month(), d.Day())
	}
	if d.Time() != time.Date(2026, time.October, 3, 0, 0, 0, 0, time.UTC) {
		t.Errorf("Time() = %v", d.Time())
	}
	for _, bad := range []string{"", "2026-13-01", "2026-02-30", "03.10.2026", "2026-10-03T00:00:00Z", "2026-1-3", "0000-06-15"} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestArithmeticAndComparison(t *testing.T) {
	t.Parallel()
	d := New(2026, time.January, 31)
	if got := d.AddDays(1); !got.Equal(New(2026, time.February, 1)) {
		t.Errorf("AddDays(1) = %s", got)
	}
	if got := d.AddDays(-31); !got.Equal(New(2025, time.December, 31)) {
		t.Errorf("AddDays(-31) = %s", got)
	}
	if got := d.AddMonths(1); !got.Equal(New(2026, time.March, 3)) {
		t.Errorf("AddMonths(1) from Jan 31 = %s, want normalized 2026-03-03", got)
	}
	a, b := New(2026, time.May, 1), New(2026, time.May, 2)
	if !a.Before(b) || a.After(b) || a.Equal(b) || a.Compare(b) != -1 || b.Compare(a) != 1 || a.Compare(a) != 0 {
		t.Error("comparison methods disagree")
	}
	if !(Date{}).IsZero() || a.IsZero() {
		t.Error("IsZero is wrong")
	}
	// The first day of year 1 is a real date, not "unset".
	if first := New(1, time.January, 1); first.IsZero() || first.String() != "0001-01-01" || first.Time() != time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("New(1, 1, 1) = %s, zero=%v", first, first.IsZero())
	}
	if parsed, err := Parse("0001-01-01"); err != nil || parsed.IsZero() {
		t.Errorf("Parse(0001-01-01) = %s, %v", parsed, err)
	}
	if (Date{}).String() != "" || a.String() != "2026-05-01" {
		t.Error("String is wrong")
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()
	type payload struct {
		Date Date `json:"date"`
	}
	out, err := json.Marshal(payload{Date: New(2026, time.October, 3)})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"date":"2026-10-03"}` {
		t.Errorf("Marshal = %s", out)
	}
	tests := []struct {
		name    string
		in      string
		want    Date
		wantErr bool
	}{
		{name: "date", in: `{"date":"2026-10-03"}`, want: New(2026, time.October, 3)},
		{name: "empty string is zero", in: `{"date":""}`, want: Date{}},
		{name: "missing is zero", in: `{}`, want: Date{}},
		{name: "null is zero", in: `{"date":null}`, want: Date{}},
		{name: "bad format", in: `{"date":"03.10.2026"}`, wantErr: true},
		{name: "number", in: `{"date":20261003}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var p payload
			err := json.Unmarshal([]byte(tc.in), &p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = %s, want error", tc.in, p.Date)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) returned error: %v", tc.in, err)
			}
			if !p.Date.Equal(tc.want) {
				t.Fatalf("Unmarshal(%s) = %s, want %s", tc.in, p.Date, tc.want)
			}
		})
	}
}
