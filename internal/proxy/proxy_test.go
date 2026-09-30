package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRemoveDropsKeptAliveConnections(t *testing.T) {
	transport = http.DefaultTransport // the backends are on loopback, not container networks
	backend := func(body string) *url.URL {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		return u
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := int64(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	get := func() string {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port)) // keeps the connection alive
		if err != nil {
			return err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if _, err := Ensure("old", port); err != nil {
		t.Fatal(err)
	}
	if err := SetTarget("old", backend("old app")); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "old app" {
		t.Fatalf("got %q", got)
	}
	// The port goes to another app, as when the app with the highest port
	// is deleted and a new one created.
	Remove("old")
	if _, err := Ensure("new", port); err != nil {
		t.Fatal(err)
	}
	defer Remove("new")
	if err := SetTarget("new", backend("new app")); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "new app" {
		t.Errorf("after the port changed hands: %q", got)
	}
}
