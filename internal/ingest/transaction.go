package ingest

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Transaction is a "transaction" item: one request or task an app traced,
// with the spans it was made of.
type Transaction struct {
	TraceID    string
	Name       string // the route, with IDs in a URL replaced by {id}
	Op         string
	Status     string // Sentry's span status: "ok", "internal_error", ...
	HTTPStatus int
	Start      time.Time
	Duration   time.Duration
	Spans      []Span // by start
}

// Span is a part of a transaction; Start is from the transaction's start.
type Span struct {
	ID, Parent  string
	Op          string
	Description string
	Start       time.Duration
	Duration    time.Duration
	Status      string
}

// Failed reports a transaction that ended in an error: a server error for
// a request, any status but ok for a task. A 404 or 401 is the client's.
func (t Transaction) Failed() bool {
	if t.HTTPStatus != 0 {
		return t.HTTPStatus >= 500
	}
	switch t.Status {
	case "", "ok", "unknown", "cancelled":
		return false
	}
	return true
}

// Slowest is the span that took longest, if there are any.
func (t Transaction) Slowest() (Span, bool) {
	if len(t.Spans) == 0 {
		return Span{}, false
	}
	return slices.MaxFunc(t.Spans, func(a, b Span) int { return int(a.Duration - b.Duration) }), true
}

// sentryTime is a timestamp as SDKs send it: seconds since the epoch, or
// RFC 3339.
type sentryTime struct{ time.Time }

func (t *sentryTime) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := time.Parse(time.RFC3339Nano, s)
		t.Time = v
		return err
	}
	// A float64 of seconds now is only good to a few hundred
	// nanoseconds: read the fraction's digits instead.
	whole, frac, _ := strings.Cut(string(b), ".")
	secs, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || strings.ContainsAny(frac, "eE") {
		f, err := strconv.ParseFloat(string(b), 64)
		t.Time = time.Unix(0, int64(f*1e9))
		return err
	}
	frac = (frac + "000000000")[:9]
	nanos, err := strconv.ParseInt(frac, 10, 64)
	t.Time = time.Unix(secs, nanos)
	return err
}

// ExtractTransaction reads a "transaction" item; ok is false when it
// isn't one.
func ExtractTransaction(item Item) (t Transaction, ok bool) {
	var tx struct {
		Type        string `json:"type"`
		Transaction string `json:"transaction"`
		Info        struct {
			Source string `json:"source"`
		} `json:"transaction_info"`
		Start    sentryTime `json:"start_timestamp"`
		End      sentryTime `json:"timestamp"`
		Contexts struct {
			Trace struct {
				TraceID string `json:"trace_id"`
				Op      string `json:"op"`
				Status  string `json:"status"`
				Data    struct {
					HTTPStatus int `json:"http.response.status_code"`
				} `json:"data"`
			} `json:"trace"`
			Response struct {
				StatusCode int `json:"status_code"`
			} `json:"response"`
		} `json:"contexts"`
		Spans []struct {
			ID          string     `json:"span_id"`
			Parent      string     `json:"parent_span_id"`
			Op          string     `json:"op"`
			Description string     `json:"description"`
			Status      string     `json:"status"`
			Start       sentryTime `json:"start_timestamp"`
			End         sentryTime `json:"timestamp"`
		} `json:"spans"`
	}
	if err := json.Unmarshal(item.Payload, &tx); err != nil || tx.Type != "transaction" || tx.Start.IsZero() || tx.End.Before(tx.Start.Time) {
		return Transaction{}, false
	}
	trace := tx.Contexts.Trace
	t = Transaction{
		TraceID: trace.TraceID, Name: RouteName(tx.Transaction, tx.Info.Source), Op: trace.Op, Status: trace.Status,
		HTTPStatus: tx.Contexts.Response.StatusCode, Start: tx.Start.Time, Duration: tx.End.Sub(tx.Start.Time),
	}
	if t.HTTPStatus == 0 {
		t.HTTPStatus = trace.Data.HTTPStatus
	}
	// Requests no route matched are named after their URL: scanners would
	// make a route of each.
	if t.HTTPStatus == 404 && tx.Info.Source == "url" {
		t.Name = NoRoute
	}
	for _, s := range tx.Spans {
		if s.Start.IsZero() || s.End.Before(s.Start.Time) {
			continue
		}
		t.Spans = append(t.Spans, Span{
			ID: s.ID, Parent: s.Parent, Op: s.Op, Description: s.Description, Status: s.Status,
			Start: s.Start.Sub(tx.Start.Time), Duration: s.End.Sub(s.Start.Time),
		})
	}
	slices.SortStableFunc(t.Spans, func(a, b Span) int { return int(a.Start - b.Start) })
	return t, true
}

// NoRoute is the route of the requests that matched none (404).
const NoRoute = "(no route: 404)"

// maxRouteName caps a route's name; longer ones are cut.
const maxRouteName = 200

// idSegment is a path segment that names one thing rather than a route:
// a number, a UUID, a long hex or base64-ish token.
var idSegment = regexp.MustCompile(`^(\d+|[0-9a-fA-F-]{16,}|[0-9a-fA-F]{8,}|[A-Za-z0-9_-]*\d[A-Za-z0-9_-]{15,})$`)

// RouteName turns a transaction's name into its route. Names taken from
// the URL (source "url", or none) lose the scheme, host and query and get {id} for
// segments that look like IDs, so requests group by route and the names,
// stored as they are, don't carry the users' data.
func RouteName(name, source string) string {
	if source == "url" || (source == "" && strings.HasPrefix(name, "/")) {
		method, path, hasMethod := strings.Cut(name, " ")
		if !hasMethod {
			method, path = "", name
		}
		// A full URL names the host it reached, such as the container's
		// address: the path is the route.
		if u, err := url.Parse(path); err == nil && u.Scheme != "" && u.Host != "" {
			path = u.EscapedPath()
			if path == "" {
				path = "/"
			}
		}
		path, _, _ = strings.Cut(path, "?")
		path, _, _ = strings.Cut(path, "#")
		segments := strings.Split(path, "/")
		for i, seg := range segments {
			if idSegment.MatchString(seg) {
				segments[i] = "{id}"
			}
		}
		name = strings.Join(segments, "/")
		if hasMethod {
			name = method + " " + name
		}
	}
	if name == "" {
		name = "(unnamed)"
	}
	if len(name) > maxRouteName {
		name = name[:maxRouteName]
	}
	return name
}
