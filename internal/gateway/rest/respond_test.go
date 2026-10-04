package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONRejectsAmbiguousAndOversizedBodies(t *testing.T) {
	t.Parallel()
	valid := `{"amount":1,"category":"food"}`
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"extra closing array", valid + "]", 400},
		{"extra closing object", valid + "}", 400},
		{"second object", valid + `{}`, 400},
		{"trailing garbage", valid + " x", 400},
		{"oversized whitespace tail", valid + strings.Repeat(" ", maxBodyBytes), 413},
		{"duplicate amount", `{"amount":1,"amount":2,"category":"food"}`, 400},
		{"duplicate null", `{"amount":1,"amount":null,"category":"food"}`, 400},
		{"duplicate case alias", `{"amount":1,"Amount":2,"category":"food"}`, 400},
		{"duplicate escaped key", `{"amount":1,"\u0061mount":2,"category":"food"}`, 400},
		{"duplicate unicode alias", `{"amount":1,"category":"food","description":"a","deſcription":"b"}`, 400},
		{"invalid utf8", "{\"amount\":1,\"category\":\"\xff\"}", 400},
		{"null object", `null`, 400},
		{"array", `[]`, 400},
		{"unknown field", `{"amount":1,"category":"food","user_id":2}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			var dst createTransactionRequest
			if decodeJSON(w, r, &dst) || w.Code != tc.status {
				t.Fatalf("accepted or wrong status: got %d, want %d", w.Code, tc.status)
			}
		})
	}
	for _, body := range []string{valid, valid + " \r\n\t", valid + strings.Repeat(" ", maxBodyBytes-len(valid))} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		var dst createTransactionRequest
		if !decodeJSON(w, r, &dst) || dst.Amount != 100 || dst.Category != "food" {
			t.Fatalf("valid object rejected: %s", w.Body.String())
		}
	}
}
