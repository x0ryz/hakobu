package ingest

import (
	"strings"
	"testing"
)

func TestParseEnvelopeRejectsBadLengths(t *testing.T) {
	// A huge length used to be allocated as is and crash the agent.
	for _, length := range []string{"-1", "1099511627776", "100"} {
		body := "{}\n{\"type\":\"event\",\"length\":" + length + "}\n{}\n"
		if _, err := ParseEnvelope([]byte(body)); err == nil {
			t.Errorf("length %s accepted", length)
		}
	}
	items, err := ParseEnvelope([]byte("{}\n{\"type\":\"event\",\"length\":2}\n{}\n"))
	if err != nil || len(items) != 1 || string(items[0].Payload) != "{}" {
		t.Errorf("valid envelope: %v, %v", items, err)
	}
}

func TestExtractLogEntriesKeepsEachRecordsOwnPayload(t *testing.T) {
	payload := `{"items":[{"level":"info","body":"a"},{"level":"warn","body":"` + strings.Repeat("b", 1000) + `"}]}`
	entries := ExtractLogEntries(Item{Type: "log", Payload: []byte(payload)})
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Level != "info" || entries[0].Message != "a" || string(entries[0].Payload) != `{"level":"info","body":"a"}` {
		t.Errorf("first entry = %+v", entries[0])
	}
	if len(entries[1].Payload) > 1100 {
		t.Errorf("second entry carries %d bytes, not just its own record", len(entries[1].Payload))
	}
}
