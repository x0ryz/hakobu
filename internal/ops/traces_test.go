package ops

import (
	"fmt"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/ingest"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

func TestTracesCountedAndSlowOnesKept(t *testing.T) {
	s := notifyStore(t)
	tx := func(name string, d time.Duration, status string) ingest.Transaction {
		return ingest.Transaction{TraceID: "t-" + name, Name: name, Op: "http.server", Status: status, Duration: d,
			Spans: []ingest.Span{{ID: "a", Op: "db", Description: "SELECT 1", Duration: d / 2}, {ID: "b", Parent: "a", Op: "db", Start: d / 2, Duration: d / 4}}}
	}
	record := func(tx ingest.Transaction) {
		t.Helper()
		if err := RecordTransaction(s, "web", tx, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	for range 18 {
		record(tx("/fast", 20*time.Millisecond, "ok"))
	}
	record(tx("/fast", 3*time.Second, "ok"))                   // slow: kept
	record(tx("/fast", 30*time.Millisecond, "internal_error")) // failed: kept
	record(tx("/slow", 12*time.Second, "ok"))

	routes, err := RoutesOf(s, "web", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].Name != "/slow" || !routes[0].P95Over {
		t.Fatalf("routes %+v, want /slow first, over the last bound", routes)
	}
	fast := routes[1]
	if fast.Count != 20 || fast.Errors != 1 || fast.ErrorPct() != 5 || fast.P50Ms < 10 || fast.P50Ms > 25 || fast.P95Ms < 25 || fast.P95Ms > 50 {
		t.Errorf("/fast %+v", fast)
	}

	kept, err := s.Tel.ListTraces(ctx(), teldb.ListTracesParams{AppName: "web", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d traces, want the slow and the failed ones: %+v", len(kept), kept)
	}
	if kept[2].SlowSpan != "db SELECT 1 (1500 ms)" {
		t.Errorf("slowest span %q", kept[2].SlowSpan)
	}
}

func TestRoutesCappedPerHour(t *testing.T) {
	s := notifyStore(t)
	for i := range maxRoutesPerHour + 3 {
		if err := RecordTransaction(s, "web", ingest.Transaction{Name: fmt.Sprintf("/r%d", i), Status: "ok"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := RoutesOf(s, "web", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != maxRoutesPerHour+1 {
		t.Errorf("%d routes, want %d and %q", len(routes), maxRoutesPerHour, otherRoute)
	}
	for _, r := range routes {
		if r.Name == otherRoute && r.Count != 3 {
			t.Errorf("%q counts %d", otherRoute, r.Count)
		}
	}
}

func TestWaterfall(t *testing.T) {
	tx := ingest.Transaction{Op: "http.server", Name: "/x", Duration: time.Second, Spans: []ingest.Span{
		{ID: "a", Op: "db", Start: 0, Duration: 500 * time.Millisecond},
		{ID: "b", Parent: "a", Op: "db.query", Start: 100 * time.Millisecond, Duration: 100 * time.Millisecond},
		{ID: "c", Parent: "root", Op: "http.client", Start: 900 * time.Millisecond, Duration: 300 * time.Millisecond}, // ends after the transaction
	}}
	rows := waterfallRows(tx)
	if len(rows) != 4 || rows[0].WidthPct != 100 || rows[1].Depth != 1 || rows[2].Depth != 2 || rows[2].LeftPct != 10 || rows[2].WidthPct != 10 {
		t.Errorf("rows %+v", rows)
	}
	if last := rows[3]; last.LeftPct+last.WidthPct > 100.0001 {
		t.Errorf("a span runs past the end: %+v", last)
	}
}
