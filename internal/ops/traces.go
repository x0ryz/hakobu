package ops

import (
	"fmt"
	"slices"
	"time"

	"github.com/x0ryz/hakobu/internal/ingest"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// Apps whose Sentry SDK traces requests (traces_sample_rate) send each one
// as a transaction. hakobu counts them all by route and hour, and keeps the
// slow and failed ones whole for a week (migration 003_traces.sql).

// slowTrace is how long a request takes to be kept whole.
const slowTrace = time.Second

// maxRoutesPerHour caps the routes counted for an app in an hour: names
// that still carry IDs mustn't grow the table without end. The rest count
// as otherRoute.
const (
	maxRoutesPerHour = 500
	otherRoute       = "(other routes)"
)

// bucketBounds are the upper bounds in milliseconds of the histogram's
// buckets but the last, which has none.
var bucketBounds = [...]int64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

func bucketOf(ms int64) int64 {
	for i, b := range bucketBounds {
		if ms < b {
			return int64(i)
		}
	}
	return int64(len(bucketBounds))
}

// RecordTransaction counts a transaction of app and keeps it whole if it
// was slow or failed.
func RecordTransaction(s *store.Store, app string, tx ingest.Transaction, payload []byte) error {
	now := time.Now()
	hour := now.Truncate(time.Hour).Unix()
	name := tx.Name
	if known, err := s.Tel.HasTraceRoute(ctx(), teldb.HasTraceRouteParams{AppName: app, Hour: hour, Name: name}); err == nil && !known {
		if n, err := s.Tel.CountTraceRoutes(ctx(), teldb.CountTraceRoutesParams{AppName: app, Hour: hour}); err == nil && n >= maxRoutesPerHour {
			name = otherRoute
		}
	}
	ms := tx.Duration.Milliseconds()
	var failed int64
	if tx.Failed() {
		failed = 1
	}
	if err := s.Tel.CountTrace(ctx(), teldb.CountTraceParams{AppName: app, Name: name, Hour: hour, Errors: failed, Ms: ms, Bucket: bucketOf(ms)}); err != nil {
		return err
	}
	if !tx.Failed() && tx.Duration < slowTrace {
		return nil
	}
	slow := ""
	if span, ok := tx.Slowest(); ok {
		slow = fmt.Sprintf("%s %s (%d ms)", span.Op, span.Description, span.Duration.Milliseconds())
	}
	return s.Tel.CreateTrace(ctx(), teldb.CreateTraceParams{
		AppName: app, TraceID: tx.TraceID, Name: tx.Name, Status: tx.Status, HttpStatus: int64(tx.HTTPStatus),
		DurationMs: ms, SlowSpan: secret.String(slow), Payload: secret.String(payload), CreatedAt: now.Unix(),
	})
}

// RouteStats is how a route of an app did over a span.
type RouteStats struct {
	Name          string
	Count, Errors int64
	TotalMs       int64 // all its requests together: what it costs
	AvgMs         int64
	P50Ms, P95Ms  int64
	P95Over       bool // P95Ms is the last bound: it took longer
}

// ErrorPct is the share of requests that failed, in percent.
func (r RouteStats) ErrorPct() float64 { return float64(r.Errors) / float64(r.Count) * 100 }

// RoutesOf sums an app's routes over the span, the ones that took the
// most time in all first.
func RoutesOf(s *store.Store, app string, span time.Duration) ([]RouteStats, error) {
	rows, err := s.Tel.ListTraceRoutes(ctx(), teldb.ListTraceRoutesParams{AppName: app, Hour: time.Now().Add(-span).Truncate(time.Hour).Unix()})
	if err != nil {
		return nil, err
	}
	type sum struct {
		count, errors, total int64
		buckets              [len(bucketBounds) + 1]int64
	}
	byName := map[string]*sum{}
	for _, r := range rows {
		t := byName[r.Name]
		if t == nil {
			t = &sum{}
			byName[r.Name] = t
		}
		t.count += r.Count
		t.errors += r.Errors
		t.total += r.TotalMs
		for i, b := range []int64{r.B0, r.B1, r.B2, r.B3, r.B4, r.B5, r.B6, r.B7, r.B8, r.B9, r.B10} {
			t.buckets[i] += b
		}
	}
	out := make([]RouteStats, 0, len(byName))
	for name, t := range byName {
		if t.count == 0 {
			continue
		}
		r := RouteStats{Name: name, Count: t.count, Errors: t.errors, TotalMs: t.total, AvgMs: t.total / t.count}
		r.P50Ms, _ = percentile(t.buckets[:], t.count, 0.5)
		r.P95Ms, r.P95Over = percentile(t.buckets[:], t.count, 0.95)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RouteStats) int {
		if a.TotalMs != b.TotalMs {
			return int(b.TotalMs - a.TotalMs)
		}
		return int(b.Count - a.Count)
	})
	return out, nil
}

// percentile estimates the q-th duration from the histogram, spread evenly
// within its bucket; over is true when it's in the last, unbounded one.
func percentile(buckets []int64, count int64, q float64) (ms int64, over bool) {
	rank := q * float64(count)
	var seen float64
	for i, n := range buckets {
		if n == 0 || seen+float64(n) < rank {
			seen += float64(n)
			continue
		}
		if i == len(bucketBounds) {
			return bucketBounds[len(bucketBounds)-1], true
		}
		lower := int64(0)
		if i > 0 {
			lower = bucketBounds[i-1]
		}
		frac := (rank - seen) / float64(n)
		return lower + int64(frac*float64(bucketBounds[i]-lower)), false
	}
	return 0, false
}

// Waterfall is a kept trace with its spans laid out over its duration.
type Waterfall struct {
	teldb.Trace
	Tx   ingest.Transaction
	Rows []WaterfallRow
}

// WaterfallRow is the transaction itself or one of its spans.
type WaterfallRow struct {
	Op, Description, Status string
	Depth                   int
	Ms                      float64
	LeftPct, WidthPct       float64
}

// TraceOf reads a kept trace of app and lays it out.
func TraceOf(s *store.Store, app string, id int64) (Waterfall, error) {
	t, err := s.Tel.GetTrace(ctx(), teldb.GetTraceParams{ID: id, AppName: app})
	if err != nil {
		return Waterfall{}, fmt.Errorf("no trace %d of %s (traces are kept for a week)", id, app)
	}
	tx, ok := ingest.ExtractTransaction(ingest.Item{Type: "transaction", Payload: []byte(t.Payload)})
	if !ok {
		return Waterfall{}, fmt.Errorf("trace %d of %s can't be read", id, app)
	}
	return Waterfall{Trace: t, Tx: tx, Rows: waterfallRows(tx)}, nil
}

// maxWaterfallRows caps the spans shown; SDKs send up to 1000.
const maxWaterfallRows = 300

func waterfallRows(tx ingest.Transaction) []WaterfallRow {
	total := float64(max(tx.Duration, time.Microsecond))
	pct := func(d time.Duration) float64 { return min(max(float64(d)/total*100, 0), 100) }
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	rows := []WaterfallRow{{Op: tx.Op, Description: tx.Name, Status: tx.Status, Ms: ms(tx.Duration), WidthPct: 100}}
	depth := map[string]int{}
	for _, sp := range tx.Spans {
		d := 1
		if p, ok := depth[sp.Parent]; ok {
			d = p + 1
		}
		depth[sp.ID] = d
		if len(rows) > maxWaterfallRows {
			continue
		}
		left := pct(sp.Start)
		rows = append(rows, WaterfallRow{
			Op: sp.Op, Description: sp.Description, Status: sp.Status, Depth: min(d, 8), Ms: ms(sp.Duration),
			LeftPct: left, WidthPct: max(min(pct(sp.Duration), 100-left), 0.3),
		})
	}
	return rows
}
