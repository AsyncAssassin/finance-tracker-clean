package usecase_test

import (
	"bytes"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/csvimport"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
)

func TestFormulaTextSurvivesCSVExportAndImport(t *testing.T) {
	for _, text := range []string{"=1+1", "+1+1", "-1+1", "@sum(1)", "'=1+1", "''=1+1", "'ordinary", "ordinary"} {
		var buf bytes.Buffer
		if err := usecase.WriteCSV(&buf, []domain.Transaction{{ID: 1, Amount: 100, Category: text, Description: text, ExternalID: text, Date: today}}); err != nil {
			t.Fatal(err)
		}
		parsed, err := csvimport.Parse(buf.Bytes())
		if err != nil || len(parsed.Rows) != 1 || len(parsed.Problems) != 0 {
			t.Fatalf("parse %q: %+v %v", text, parsed, err)
		}
		got := parsed.Rows[0].Input
		if got.Category != text || got.Description != text || got.ExternalID != text {
			t.Fatalf("text %q changed during round trip: %+v", text, got)
		}
	}
}
