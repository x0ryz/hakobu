package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestCanList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key/") || r.URL.Query().Get("list-type") != "2" {
			t.Errorf("unsigned or not a listing: %s %s", r.Header.Get("Authorization"), r.URL)
		}
		switch r.URL.Path {
		case "/mine":
			w.WriteHeader(http.StatusOK)
		case "/backups":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewClient(store.Storage{Provider: "s3", Endpoint: srv.URL, AccessKeyID: "key", SecretAccessKey: secret.String("secret")})
	for bucket, want := range map[string]bool{"mine": true, "backups": false} {
		if got, err := c.CanList(bucket); err != nil || got != want {
			t.Errorf("CanList(%s) = %v, %v; want %v", bucket, got, err, want)
		}
	}
	if _, err := c.CanList("broken"); err == nil {
		t.Error("a server error read as an answer")
	}
}
