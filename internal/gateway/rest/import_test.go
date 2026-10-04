package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/csvimport"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
)

const importPath = "/api/transactions/import"

type importReply struct {
	Total   int `json:"total"`
	Created int `json:"created"`
	Failed  int `json:"failed"`
	Rows    []struct {
		Line   int    `json:"line"`
		Status string `json:"status"`
		ID     int64  `json:"id"`
		Error  *struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	} `json:"rows"`
}

func TestImport(t *testing.T) {
	t.Parallel()
	var gotUser int64
	var gotRows []ledgerclient.ImportRow
	ledger := &fakeLedger{importRows: func(_ context.Context, u int64, rows []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
		gotUser, gotRows = u, rows
		return []ledgerclient.ImportResult{
			{Line: 2, ID: 10},
			{Line: 4, Err: &ledgerclient.BudgetExceededError{Category: "еда", Limit: 100, Spent: 90, Amount: 20, Remaining: 10}},
			{Line: 5, Err: &ledgerclient.DuplicateError{ExternalID: "r5", ExistingID: 3}},
			{Line: 6, Err: &upstream.ValidationError{Fields: map[string]string{"category": "must not be empty"}}},
			{Line: 7, Err: &ledgerclient.RowError{Code: "canceled", Message: "stopped"}},
		}, nil
	}}
	csv := "date,category,amount,external_id\n" +
		"2026-10-01,еда,10,r2\n" +
		"2026-10-01,еда,много,r3\n" + // fails in the gateway
		"2026-10-01,еда,20,r4\n" +
		"2026-10-01,еда,5,r5\n" +
		"2026-10-01, ,5,r6\n" +
		"2026-10-01,кафе,5,r7\n"
	res, body := do(t, newHandler(ledger), http.MethodPost, importPath, csv, "Content-Type", "text/csv; charset=utf-8")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var reply importReply
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Total != 6 || reply.Created != 1 || reply.Failed != 5 || len(reply.Rows) != 6 {
		t.Fatalf("reply = %+v", reply)
	}
	if gotUser != testUser || len(gotRows) != 5 || gotRows[0].Line != 2 || gotRows[1].Line != 4 {
		t.Errorf("the ledger got user %d and rows %+v", gotUser, gotRows)
	}
	wantCodes := map[int]string{3: codeValidation, 4: codeBudgetExceeded, 5: codeAlreadyExists, 6: codeValidation, 7: "canceled"}
	for i, row := range reply.Rows {
		if row.Line != i+2 {
			t.Errorf("row %d is line %d, want the rows in file order", i, row.Line)
		}
		if row.Line == 2 {
			if row.Status != "created" || row.ID != 10 || row.Error != nil {
				t.Errorf("line 2 = %+v", row)
			}
			continue
		}
		if row.Status != "failed" || row.Error == nil || row.Error.Code != wantCodes[row.Line] {
			t.Errorf("line %d = %+v, want failed with %s", row.Line, row, wantCodes[row.Line])
		}
	}
	if d := reply.Rows[2].Error.Details; d["remaining"] != 0.1 {
		t.Errorf("budget details = %v, want the same as a 409", d)
	}
	if d := reply.Rows[1].Error.Details; d["amount"] == nil {
		t.Errorf("the gateway's row problem has details %v", d)
	}
}

func TestImportFileErrors(t *testing.T) {
	t.Parallel()
	ledger := &fakeLedger{importRows: func(context.Context, int64, []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
		t.Error("the ledger was called for a rejected file")
		return nil, nil
	}}
	h := newHandler(ledger)
	for name, tc := range map[string]struct {
		body, contentType string
		status            int
		code              string
	}{
		"json body":      {`{"rows":[]}`, "application/json", 415, codeUnsupportedType},
		"form body":      {"category=еда", "application/x-www-form-urlencoded", 415, codeUnsupportedType},
		"unknown column": {"category,ammount\nеда,1\n", "text/csv", 400, codeInvalidCSV},
		"header only":    {"category,amount\n", "text/csv", 400, codeInvalidCSV},
		"broken quote":   {"category,amount\n\"еда,1\n", "text/csv", 400, codeInvalidCSV},
		"too large":      {"category,amount\n" + strings.Repeat("еда,1\n", csvimport.MaxBytes/8), "text/csv", 413, codePayloadTooLarge},
	} {
		res, body := do(t, h, http.MethodPost, importPath, tc.body, "Content-Type", tc.contentType)
		if res.StatusCode != tc.status || !strings.Contains(body, `"`+tc.code+`"`) {
			t.Errorf("%s: %d %s, want %d %s", name, res.StatusCode, body, tc.status, tc.code)
		}
	}
	res, body := do(t, h, http.MethodPost, importPath, "category,ammount\nеда,1\n", "Content-Type", "text/csv")
	if e := decodeError(t, res, body, 400, codeInvalidCSV); e.Error.Details["line"] != 1.0 {
		t.Errorf("details = %v, want the line", e.Error.Details)
	}
}

func TestImportWithOnlyBadRows(t *testing.T) {
	t.Parallel()
	ledger := &fakeLedger{importRows: func(context.Context, int64, []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
		t.Error("the ledger was called with no valid rows")
		return nil, nil
	}}
	res, body := do(t, newHandler(ledger), http.MethodPost, importPath, "category,amount\nеда,x\n", "Content-Type", "text/csv")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"failed":1`) {
		t.Errorf("%d %s", res.StatusCode, body)
	}
}

func TestImportLedgerDown(t *testing.T) {
	t.Parallel()
	ledger := &fakeLedger{importRows: func(context.Context, int64, []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
		return nil, &upstream.UnavailableError{Service: "ledger", Reason: "refused"}
	}}
	res, body := do(t, newHandler(ledger), http.MethodPost, importPath, "category,amount\nеда,1\n", "Content-Type", "text/csv")
	decodeError(t, res, body, http.StatusBadGateway, codeLedgerDown)
}

func TestImportNeedsAToken(t *testing.T) {
	t.Parallel()
	res, body := do(t, newHandler(&fakeLedger{}), http.MethodPost, importPath, "category,amount\nеда,1\n", "Content-Type", "text/csv", "Authorization", "")
	decodeError(t, res, body, http.StatusUnauthorized, codeUnauthorized)
}
