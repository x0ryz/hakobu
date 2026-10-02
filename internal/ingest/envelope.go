// Package ingest parses Sentry's envelope format
// (https://develop.sentry.dev/sdk/data-model/envelopes/).
package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type Item struct {
	Type    string
	Payload []byte
}

// ParseEnvelope reads the header line, then (item header, payload) pairs. A
// header's "length" is the exact payload size; without it the payload is
// the next line.
func ParseEnvelope(body []byte) ([]Item, error) {
	r := bufio.NewReader(bytes.NewReader(body))

	if _, err := r.ReadBytes('\n'); err != nil && err != io.EOF {
		return nil, err
	}

	var items []Item
	for {
		headerLine, err := r.ReadBytes('\n')
		trimmed := bytes.TrimRight(headerLine, "\n")
		if len(trimmed) == 0 {
			break
		}

		var header struct {
			Type   string `json:"type"`
			Length *int   `json:"length"`
		}
		if jsonErr := json.Unmarshal(trimmed, &header); jsonErr != nil {
			return items, jsonErr
		}

		var payload []byte
		if header.Length != nil {
			// The length comes from the client: never allocate more than
			// the envelope could hold.
			if *header.Length < 0 || *header.Length > len(body) {
				return items, fmt.Errorf("item length %d out of range", *header.Length)
			}
			payload = make([]byte, *header.Length)
			if _, readErr := io.ReadFull(r, payload); readErr != nil {
				return items, readErr
			}
			r.ReadByte() // consume the payload's trailing newline, if any
		} else {
			payloadLine, readErr := r.ReadBytes('\n')
			payload = bytes.TrimRight(payloadLine, "\n")
			if readErr != nil && readErr != io.EOF {
				return items, readErr
			}
		}

		items = append(items, Item{Type: header.Type, Payload: payload})

		if err == io.EOF {
			break
		}
	}
	return items, nil
}

type Summary struct {
	Level   string
	Message string
	TraceID string // of the request or task it happened in, if traced
}

// ExtractEventSummary falls back to the first exception's "type: value"
// when the event has no message (uncaught exceptions).
func ExtractEventSummary(item Item) Summary {
	var e struct {
		Message   string `json:"message"`
		Level     string `json:"level"`
		Exception struct {
			Values []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"values"`
		} `json:"exception"`
		Contexts struct {
			Trace struct {
				TraceID string `json:"trace_id"`
			} `json:"trace"`
		} `json:"contexts"`
	}
	json.Unmarshal(item.Payload, &e)

	msg := e.Message
	if msg == "" && len(e.Exception.Values) > 0 {
		v := e.Exception.Values[0]
		msg = v.Type + ": " + v.Value
	}
	level := e.Level
	if level == "" {
		level = "error"
	}
	return Summary{Level: level, Message: msg, TraceID: e.Contexts.Trace.TraceID}
}

// LogEntry is one record of a "log" item, with its own JSON.
type LogEntry struct {
	Summary
	Payload []byte
}

// ExtractLogEntries reads a "log" item, which batches several records.
func ExtractLogEntries(item Item) []LogEntry {
	var l struct {
		Items []json.RawMessage `json:"items"`
	}
	json.Unmarshal(item.Payload, &l)

	entries := make([]LogEntry, 0, len(l.Items))
	for _, raw := range l.Items {
		var e struct {
			Level string `json:"level"`
			Body  string `json:"body"`
		}
		_ = json.Unmarshal(raw, &e) // a malformed record is kept, with no summary
		entries = append(entries, LogEntry{Summary{Level: e.Level, Message: e.Body}, raw})
	}
	return entries
}
