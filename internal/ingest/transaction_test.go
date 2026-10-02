package ingest

import (
	"os"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []Item {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	items, err := ParseEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// Envelopes the Python SDK sent (testdata, scrubbed of the machine's names).
func TestExtractTransactionFromPythonSDK(t *testing.T) {
	items := readFixture(t, "python_transaction.envelope")
	tx, ok := ExtractTransaction(items[0])
	if !ok {
		t.Fatal("not read as a transaction")
	}
	if tx.Name != "/orders/{order_id}" || tx.TraceID != "b891311e7592455eb34a44fa1599adc9" || tx.Op != "http.server" || tx.HTTPStatus != 200 || tx.Failed() {
		t.Errorf("transaction %+v", tx)
	}
	if tx.Duration < 140*time.Millisecond || tx.Duration > time.Second {
		t.Errorf("took %v, want about 150ms", tx.Duration)
	}
	slow, ok := tx.Slowest()
	if len(tx.Spans) != 2 || !ok || slow.Op != "db" || slow.Description != "SELECT * FROM orders WHERE id = %s" || slow.Duration < 100*time.Millisecond {
		t.Errorf("spans %+v, slowest %+v", tx.Spans, slow)
	}
	if tx.Spans[1].Start < tx.Spans[0].Start+tx.Spans[0].Duration {
		t.Errorf("spans out of order: %+v", tx.Spans)
	}

	failed, ok := ExtractTransaction(readFixture(t, "python_failed_transaction.envelope")[0])
	if !ok || !failed.Failed() || failed.Status != "internal_error" || failed.HTTPStatus != 500 || failed.Name != "/users/{id}/profile" {
		t.Errorf("failed transaction %+v", failed)
	}
	if _, ok := ExtractTransaction(readFixture(t, "python_error_in_transaction.envelope")[0]); ok {
		t.Error("an error event read as a transaction")
	}
}

func TestErrorKnowsItsTrace(t *testing.T) {
	items := readFixture(t, "python_error_in_transaction.envelope")
	sum := ExtractEventSummary(items[0])
	if sum.TraceID != "b626f635bb2c4d158a088b0659727054" || sum.Message != "ZeroDivisionError: division by zero" {
		t.Errorf("summary %+v", sum)
	}
}

func TestTransactionTimesAsNumbers(t *testing.T) {
	// The JavaScript SDK sends seconds since the epoch.
	payload := `{"type":"transaction","transaction":"GET /api/items","transaction_info":{"source":"route"},
		"start_timestamp":1790000000.25,"timestamp":1790000001.75,
		"contexts":{"trace":{"trace_id":"abc","status":"ok"}},
		"spans":[{"span_id":"s","op":"db","start_timestamp":1790000000.5,"timestamp":1790000001.5}]}`
	tx, ok := ExtractTransaction(Item{Type: "transaction", Payload: []byte(payload)})
	if !ok || tx.Duration != 1500*time.Millisecond || tx.Spans[0].Start != 250*time.Millisecond || tx.Spans[0].Duration != time.Second {
		t.Errorf("transaction %+v, ok %v", tx, ok)
	}
}

func TestRouteName(t *testing.T) {
	for _, c := range []struct{ name, source, want string }{
		{"/users/42/profile?email=a@b.c", "url", "/users/{id}/profile"},
		{"http://172.20.0.3:8000/api/v1/devices/42?x=1", "url", "/api/v1/devices/{id}"},
		{"GET https://shop.example.com", "url", "GET /"},
		{"GET /orders/9f8e7d6c-1234-4abc-9def-001122334455", "url", "GET /orders/{id}"},
		{"/files/a1b2c3d4e5f6", "", "/files/{id}"},
		{"/blog/hello-world", "url", "/blog/hello-world"},
		{"/users/{id}", "route", "/users/{id}"},
		{"tasks.send_email", "task", "tasks.send_email"},
		{"", "custom", "(unnamed)"},
	} {
		if got := RouteName(c.name, c.source); got != c.want {
			t.Errorf("RouteName(%q, %q) = %q, want %q", c.name, c.source, got, c.want)
		}
	}
}

// What a FastAPI app sends for a request no route matched: its URL with the
// container's address, status not_found. One route for all of them, and
// not a failure: the client asked for something that isn't there.
func TestNotFoundIsOneRouteAndNoFailure(t *testing.T) {
	payload := `{"type":"transaction","transaction":"http://172.20.0.3:8000/wp-login.php","transaction_info":{"source":"url"},
		"start_timestamp":1790000000,"timestamp":1790000000.002,
		"contexts":{"trace":{"trace_id":"abc","status":"not_found"},"response":{"status_code":404}}}`
	tx, ok := ExtractTransaction(Item{Type: "transaction", Payload: []byte(payload)})
	if !ok || tx.Name != NoRoute || tx.Failed() {
		t.Errorf("transaction %+v, failed %v", tx, tx.Failed())
	}
	for _, c := range []struct {
		status string
		http   int
		failed bool
	}{{"unauthenticated", 401, false}, {"internal_error", 500, true}, {"deadline_exceeded", 0, true}, {"cancelled", 0, false}} {
		if got := (Transaction{Status: c.status, HTTPStatus: c.http}).Failed(); got != c.failed {
			t.Errorf("%s/%d failed = %v", c.status, c.http, got)
		}
	}
}
