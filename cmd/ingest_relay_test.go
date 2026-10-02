package cmd

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The relay passes envelopes on to the ingest socket, and nothing else.
func TestIngestRelay(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ingest.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	relay := httptest.NewServer(ingestRelay(sock))
	t.Cleanup(relay.Close)

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/api/7/envelope/", 200},
		{"POST", "/api/7/envelope", 200},
		{"GET", "/api/7/envelope/", 404},
		{"POST", "/login", 404},
		{"POST", "/api/7/envelope/../../settings", 404},
		{"GET", "/settings/master-key", 404},
	} {
		req, _ := http.NewRequest(c.method, relay.URL+c.path, strings.NewReader("env"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	if len(got) != 2 || got[0] != "POST /api/7/envelope/ env" {
		t.Errorf("passed on %q", got)
	}
}
