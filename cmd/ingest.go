package cmd

import (
	"compress/gzip"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/ingest"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// maxEnvelopeBytes caps the decompressed size, guarding against gzip bombs.
const maxEnvelopeBytes = 10 << 20

// maxEventsPerEnvelope caps the rows one envelope can add: SDKs batch at
// most 100 log records, and each record is a row of its own.
const maxEventsPerEnvelope = 1000

var sentryKeyPattern = regexp.MustCompile(`sentry_key=([a-zA-Z0-9]+)`)

func sentryKey(r *http.Request) string {
	if m := sentryKeyPattern.FindStringSubmatch(r.Header.Get("X-Sentry-Auth")); m != nil {
		return m[1]
	}
	return r.URL.Query().Get("sentry_key")
}

func readEnvelope(r *http.Request) ([]byte, error) {
	reader := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	return io.ReadAll(io.LimitReader(reader, maxEnvelopeBytes+1))
}

// limiter is a per-app token bucket for the public ingest endpoint. Apps on
// the same server share an outgoing IP, so limiting by IP would let one
// noisy app starve the others.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	ingestRate  = 10.0
	ingestBurst = 30.0
)

func newLimiter() *limiter {
	l := &limiter{buckets: map[string]*bucket{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			l.mu.Lock()
			for key, b := range l.buckets {
				if time.Since(b.last) > 10*time.Minute {
					delete(l.buckets, key)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: ingestBurst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(ingestBurst, b.tokens+now.Sub(b.last).Seconds()*ingestRate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ingestHandler accepts Sentry envelopes at POST /api/{app_id}/envelope/,
// the DSN every app gets as SENTRY_DSN; errors, structured logs and traces
// are stored, other items dropped. It's served both on the panel and, for
// the apps on this server, on the ingest socket (ingest_relay.go), with
// the same rate limits.
func ingestHandler(s *store.Store) http.HandlerFunc {
	lim := newLimiter()
	return func(w http.ResponseWriter, r *http.Request) {
		appID, _ := strconv.ParseInt(r.PathValue("app_id"), 10, 64)
		app, err := s.GetAppByID(r.Context(), appID)
		if err != nil || app.SentryKey == "" || subtle.ConstantTimeCompare([]byte(app.SentryKey), []byte(sentryKey(r))) != 1 {
			http.Error(w, "invalid dsn", http.StatusUnauthorized)
			return
		}
		body, err := readEnvelope(r)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if len(body) > maxEnvelopeBytes {
			http.Error(w, "envelope too large", http.StatusRequestEntityTooLarge)
			return
		}
		items, err := ingest.ParseEnvelope(body)
		if err != nil {
			http.Error(w, "invalid envelope", http.StatusBadRequest)
			return
		}
		// Traces come with every sampled request: they get a budget of
		// their own, so a busy app's traces don't crowd out its errors.
		if !lim.allow(limitKey(app.Name, items)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}

		saved := 0
		save := func(kind string, sum ingest.Summary, payload []byte) {
			if saved++; saved > maxEventsPerEnvelope {
				return
			}
			if err := s.Tel.CreateTelemetryEvent(r.Context(), teldb.CreateTelemetryEventParams{AppName: app.Name, Kind: kind, Level: sum.Level, Message: secret.String(sum.Message), Payload: secret.String(payload), TraceID: sum.TraceID}); err != nil {
				fmt.Println("ingest: failed to store event:", err)
			}
		}
		for _, item := range items {
			switch item.Type {
			case "event":
				save("error", ingest.ExtractEventSummary(item), item.Payload)
			case "log":
				for _, entry := range ingest.ExtractLogEntries(item) {
					save("log", entry.Summary, entry.Payload)
				}
			case "transaction":
				if tx, ok := ingest.ExtractTransaction(item); ok {
					if err := ops.RecordTransaction(s, app.Name, tx, item.Payload); err != nil {
						fmt.Println("ingest: failed to store a trace:", err)
					}
				}
			}
		}

		id, _ := ops.RandomHex(16)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"%s"}`, id)
	}
}

// limitKey is the rate limit an envelope counts against: the app's own,
// or its traces' when it holds nothing but transactions.
func limitKey(app string, items []ingest.Item) string {
	for _, item := range items {
		if item.Type != "transaction" {
			return app
		}
	}
	if len(items) == 0 {
		return app
	}
	return app + " traces"
}
