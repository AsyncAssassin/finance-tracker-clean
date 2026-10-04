package usecase

import (
	"encoding/csv"
	"io"
	"strconv"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
)

// CSVHeader is the first line of every CSV export. The import reads the
// same columns, so an export can be imported back; see the gateway's
// csvimport package.
var CSVHeader = []string{"id", "date", "category", "amount", "description", "external_id"}

// WriteCSV writes txs to w as CSV with CSVHeader. Amounts use a dot as the
// decimal separator and two decimals; dates are YYYY-MM-DD. Text cells that
// a spreadsheet would read as a formula are escaped, see escapeCell.
func WriteCSV(w io.Writer, txs []domain.Transaction) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVHeader); err != nil {
		return err
	}
	for _, tx := range txs {
		row := []string{
			strconv.FormatInt(tx.ID, 10),
			tx.Date.String(),
			escapeCell(tx.Category),
			tx.Amount.String(),
			escapeCell(tx.Description),
			escapeCell(tx.ExternalID),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// escapeCell protects against formula injection: a cell starting with '=',
// '+', '-', '@', a tab or a carriage return would be evaluated as a formula
// by Excel or Google Sheets, so it gets a leading apostrophe, which
// spreadsheets treat as "this is text" and do not display. An existing
// leading apostrophe is doubled so the importer can restore it exactly.
func escapeCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '\'', '=', '+', '-', '@', '\t', '\r', '\n':
		return "'" + s
	}
	return s
}
