package csvimport

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

func mustParse(t *testing.T, data string) Result {
	t.Helper()
	res, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return res
}

func TestExportFormatImportsBack(t *testing.T) {
	t.Parallel()
	// An older CSV export without the optional external_id column.
	data := "id,date,category,amount,description\n" +
		"3,2026-10-04,здоровье,700.00,лекарства\n" +
		"2,2026-10-04,транспорт,1300.00,проездной\n" +
		"1,2026-10-03,еда,4750.50,'=SUM(A1) не формула\n"
	res := mustParse(t, data)
	if len(res.Rows) != 3 || len(res.Problems) != 0 || res.Total() != 3 {
		t.Fatalf("result = %+v", res)
	}
	want := ledgerclient.ImportRow{Line: 4, SourceID: 1, Input: ledgerclient.TransactionInput{
		Amount: 475050, Category: "еда", Description: "=SUM(A1) не формула", Date: civil.New(2026, time.October, 3),
	}}
	if res.Rows[2] != want {
		t.Errorf("last row = %+v, want %+v", res.Rows[2], want)
	}
	if res.Rows[0].Line != 2 {
		t.Errorf("first data row is on line %d, want 2", res.Rows[0].Line)
	}
}

func TestCurrentExportFormat(t *testing.T) {
	t.Parallel()
	data := "id,date,category,amount,description,external_id\n" +
		"7,2026-10-03,еда,10.00,,bank-1\n" +
		"8,2026-10-03,еда,20.00,,\n" +
		"x,2026-10-03,еда,30.00,,\n" // a hand-made id
	res := mustParse(t, data)
	if len(res.Rows) != 2 || res.Rows[0].SourceID != 7 || res.Rows[0].Input.ExternalID != "bank-1" || res.Rows[1].SourceID != 8 {
		t.Errorf("rows = %+v", res.Rows)
	}
	if len(res.Problems) != 1 || res.Problems[0].Fields["id"] == "" {
		t.Errorf("problems = %+v, want the bad id", res.Problems)
	}
}

func TestExcelFlavour(t *testing.T) {
	t.Parallel()
	// A byte order mark, ";" between fields, a decimal comma, headers in
	// another order and case, a blank row of separators.
	data := "\xEF\xBB\xBFAmount;CATEGORY;Date;External_ID\r\n" +
		"1250,50;Еда;2026-10-01;row-1\r\n" +
		";;;\r\n" +
		"99;кафе;;row-2\r\n"
	res := mustParse(t, data)
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %+v, problems = %+v", res.Rows, res.Problems)
	}
	if r := res.Rows[0]; r.Input.Amount != 125050 || r.Input.Category != "Еда" || r.Input.ExternalID != "row-1" || r.Line != 2 {
		t.Errorf("first row = %+v", r)
	}
	if r := res.Rows[1]; !r.Input.Date.IsZero() || r.Line != 4 {
		t.Errorf("second row = %+v, want no date on line 4", r)
	}
}

func TestQuotedNewlinesKeepLineNumbers(t *testing.T) {
	t.Parallel()
	data := "category,amount,description\n" +
		"еда,10,\"две\nстроки\"\n" +
		"кафе,20,одна\n"
	res := mustParse(t, data)
	if res.Rows[0].Input.Description != "две\nстроки" || res.Rows[1].Line != 4 {
		t.Errorf("rows = %+v; want the second data row on line 4", res.Rows)
	}
}

func TestRowProblems(t *testing.T) {
	t.Parallel()
	data := "date,category,amount\n" +
		"2026-10-01,еда,10\n" +
		"01.10.2026,еда,10\n" + // bad date
		"2026-10-01,еда,десять\n" + // bad amount
		"2026-10-01,еда\n" + // a field short
		"2026-10-01,еда,1.234\n" // three decimals
	res := mustParse(t, data)
	if len(res.Rows) != 1 || res.Total() != 5 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	want := []struct {
		line  int
		field string
	}{{3, "date"}, {4, "amount"}, {5, "row"}, {6, "amount"}}
	if len(res.Problems) != len(want) {
		t.Fatalf("problems = %+v", res.Problems)
	}
	for i, w := range want {
		if p := res.Problems[i]; p.Line != w.line || p.Fields[w.field] == "" {
			t.Errorf("problem %d = %+v, want line %d with %s", i, p, w.line, w.field)
		}
	}
}

func TestFileErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		data    string
		line    int
		message string
	}{
		"empty":           {"", 0, "empty"},
		"only a BOM":      {"\xEF\xBB\xBF", 0, "empty"},
		"header only":     {"category,amount\n", 0, "no rows"},
		"unknown column":  {"category,ammount\nеда,1\n", 1, `unknown column "ammount"`},
		"no amount":       {"category,description\nеда,x\n", 1, `"amount" is required`},
		"twice":           {"category,amount,Amount\nеда,1,2\n", 1, "twice"},
		"broken quote":    {"category,amount\nеда,1\n\"кафе,2\n", 3, "broken CSV"},
		"bare quote":      {"category,amount\nе\"да,1\n", 2, "broken CSV"},
		"russian headers": {"категория,сумма\nеда,1\n", 1, "unknown column"},
	}
	for name, tc := range tests {
		_, err := Parse([]byte(tc.data))
		var fe *Error
		if !errors.As(err, &fe) || fe.Line != tc.line || !strings.Contains(fe.Error(), tc.message) {
			t.Errorf("%s: err = %v, want line %d with %q", name, err, tc.line, tc.message)
		}
	}
}

func TestRowLimit(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("category,amount\n")
	for i := range MaxRows {
		fmt.Fprintf(&b, "еда,%d\n", i+1)
	}
	if res := mustParse(t, b.String()); len(res.Rows) != MaxRows {
		t.Fatalf("%d rows, want %d", len(res.Rows), MaxRows)
	}
	b.WriteString("еда,1\n")
	var fe *Error
	if _, err := Parse([]byte(b.String())); !errors.As(err, &fe) || !strings.Contains(fe.Message, "more than") {
		t.Errorf("one row over the limit: %v", err)
	}
}

func TestUnescape(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"'=1+1": "=1+1", "'+7": "+7", "'-5": "-5", "'@me": "@me",
		"'\tтаб": "\tтаб", "'quoted'": "'quoted'", "'": "'", "plain": "plain", "": "",
	} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInvalidTextIsARowProblem(t *testing.T) {
	for _, bad := range []string{"bad\x00text", "bad\xfftext"} {
		for _, field := range []string{"category", "description", "external_id"} {
			header := "category,amount"
			badRow := bad + ",1"
			goodRow := "valid,1"
			if field != "category" {
				header += "," + field
				badRow = "invalid,1," + bad
				goodRow += ",valid"
			}
			res, err := Parse([]byte(header + "\n" + badRow + "\n" + goodRow + "\n"))
			if err != nil || len(res.Rows) != 1 || len(res.Problems) != 1 || res.Problems[0].Fields[field] == "" {
				t.Fatalf("field %s: rows=%d problems=%+v err=%v", field, len(res.Rows), res.Problems, err)
			}
		}
	}
}
