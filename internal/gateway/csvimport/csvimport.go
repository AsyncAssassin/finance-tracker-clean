// Package csvimport reads a CSV file of transactions for an import.
//
// The first line names the columns, in any order and any case:
//
//	id, date, category, amount, description, external_id
//
// category and amount are required. These are the columns of the CSV
// export, so an exported file can be imported back: the id column tells the
// ledger which rows the user already has, so they are not stored twice. Both
// "," and ";" separate fields (Excel in Russian locales writes ";"), a
// UTF-8 byte order mark is skipped, and amounts may use a decimal comma.
//
// A file that cannot be read as a whole returns an *Error: broken CSV,
// an unknown or missing column, or too many rows. A row with bad values is
// a RowProblem, and the other rows are still imported.
package csvimport

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Limits of one file; MaxRows matches the ledger's limit of one import.
const (
	MaxBytes = 5 << 20
	MaxRows  = 10_000
)

// Error rejects the whole file.
type Error struct {
	Line    int // 0 when the problem is not on one line
	Message string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

// RowProblem is a row with values that cannot be imported.
type RowProblem struct {
	Line   int
	Fields map[string]string // field → problem
}

// Result is a parsed file: the rows to import and the rows with problems.
type Result struct {
	Rows     []ledgerclient.ImportRow
	Problems []RowProblem
}

// Total is the number of data rows in the file.
func (r Result) Total() int { return len(r.Rows) + len(r.Problems) }

var columns = []string{"id", "date", "category", "amount", "description", "external_id"}

var bom = []byte{0xEF, 0xBB, 0xBF} // UTF-8 byte order mark

// Parse reads data, the whole file.
func Parse(data []byte) (Result, error) {
	data = bytes.TrimPrefix(data, bom)
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = separator(data)
	r.FieldsPerRecord = -1 // a short row is a row problem, not a broken file
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return Result{}, &Error{Message: "the file is empty"}
	}
	if err != nil {
		return Result{}, syntaxError(err)
	}
	col, err := mapColumns(header)
	if err != nil {
		return Result{}, err
	}

	var res Result
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, syntaxError(err)
		}
		line, _ := r.FieldPos(0)
		if blank(record) {
			continue
		}
		if res.Total() == MaxRows {
			return Result{}, &Error{Line: line, Message: fmt.Sprintf("the file has more than %d rows; split it", MaxRows)}
		}
		row, problem := parseRow(record, col, len(header))
		if problem != nil {
			res.Problems = append(res.Problems, RowProblem{Line: line, Fields: problem})
			continue
		}
		row.Line = line
		res.Rows = append(res.Rows, row)
	}
	if res.Total() == 0 {
		return Result{}, &Error{Message: "the file has a header but no rows"}
	}
	return res, nil
}

// separator picks ";" when the first line has more of them than commas.
func separator(data []byte) rune {
	first, _, _ := bytes.Cut(data, []byte("\n"))
	if bytes.Count(first, []byte(";")) > bytes.Count(first, []byte(",")) {
		return ';'
	}
	return ','
}

// mapColumns returns the position of every known column in the header.
func mapColumns(header []string) (map[string]int, error) {
	col := make(map[string]int, len(header))
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		if !slices.Contains(columns, name) {
			return nil, &Error{Line: 1, Message: fmt.Sprintf("unknown column %q; the columns are: %s", h, strings.Join(columns, ", "))}
		}
		if _, dup := col[name]; dup {
			return nil, &Error{Line: 1, Message: fmt.Sprintf("column %q appears twice", name)}
		}
		col[name] = i
	}
	for _, required := range []string{"category", "amount"} {
		if _, ok := col[required]; !ok {
			return nil, &Error{Line: 1, Message: fmt.Sprintf("column %q is required", required)}
		}
	}
	return col, nil
}

func parseRow(record []string, col map[string]int, width int) (ledgerclient.ImportRow, map[string]string) {
	if len(record) != width {
		return ledgerclient.ImportRow{}, map[string]string{"row": fmt.Sprintf("has %d fields, the header has %d", len(record), width)}
	}
	get := func(name string) string {
		if i, ok := col[name]; ok {
			return record[i]
		}
		return ""
	}
	problems := map[string]string{}
	for _, field := range []string{"category", "description", "external_id"} {
		value := get(field)
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			problems[field] = "must be valid UTF-8 without NUL characters"
		}
	}
	var in ledgerclient.TransactionInput
	amount, err := money.Parse(get("amount"))
	if err != nil {
		problems["amount"] = "must be a number such as 1250.50"
	}
	in.Amount = amount
	if s := strings.TrimSpace(get("date")); s != "" {
		d, err := civil.Parse(s)
		if err != nil {
			problems["date"] = "must be a date in the form YYYY-MM-DD"
		}
		in.Date = d
	}
	var sourceID int64
	if s := strings.TrimSpace(get("id")); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			problems["id"] = "must be the positive number from an export, or empty"
		}
		sourceID = id
	}
	in.Category = unescape(get("category"))
	in.Description = unescape(get("description"))
	in.ExternalID = strings.TrimSpace(unescape(get("external_id")))
	if len(problems) > 0 {
		return ledgerclient.ImportRow{}, problems
	}
	return ledgerclient.ImportRow{Input: in, SourceID: sourceID}, nil
}

// unescape removes the apostrophe the export puts before text that a
// spreadsheet would run as a formula, so an exported file imports back
// with the original text.
func unescape(s string) string {
	if len(s) >= 2 && s[0] == '\'' && strings.ContainsRune("'=+-@\t\r\n", rune(s[1])) {
		return s[1:]
	}
	return s
}

func blank(record []string) bool {
	for _, f := range record {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}

func syntaxError(err error) error {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return &Error{Line: pe.Line, Message: "broken CSV: " + pe.Err.Error()}
	}
	return &Error{Message: "broken CSV: " + err.Error()}
}
