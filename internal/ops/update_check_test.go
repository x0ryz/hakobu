package ops

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x0ryz/hakobu/internal/update"
)

// The button asks GitHub right away, not after the hourly check.
func TestCheckForUpdates(t *testing.T) {
	tag := "v0.9.0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
	}))
	t.Cleanup(srv.Close)
	old := update.APIURL
	update.APIURL = srv.URL
	t.Cleanup(func() { update.APIURL = old })

	if err := CheckForUpdates(); err != nil {
		t.Fatal(err)
	}
	tag = "v0.9.1"
	if err := CheckForUpdates(); err != nil {
		t.Fatal(err)
	}
	info := Updates("v0.9.0")
	if info.Latest != "v0.9.1" || !info.Available || info.CheckedAt == "" {
		t.Errorf("updates %+v", info)
	}
}
