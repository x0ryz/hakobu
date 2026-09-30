package cloudflare

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestPKCE(t *testing.T) {
	verifier, challenge := PKCE()
	sum := sha256.Sum256([]byte(verifier))
	if len(verifier) < 43 || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("bad PKCE pair %q %q", verifier, challenge)
	}
	u, _ := url.Parse(AuthorizeURL("id", "https://relay/cf/callback", "st", challenge))
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("scope") != scopes || q.Get("redirect_uri") != "https://relay/cf/callback" {
		t.Errorf("authorize URL = %s", u)
	}
}

func TestZoneFor(t *testing.T) {
	zones := []Zone{{ID: "1", Name: "example.com"}, {ID: "2", Name: "shop.example.com"}, {ID: "3", Name: "other.dev"}}
	for host, want := range map[string]string{
		"example.com":          "1",
		"a.example.com":        "1",
		"a.shop.example.com":   "2",
		"x.other.dev":          "3",
		"notexample.com":       "",
		"example.com.evil.net": "",
	} {
		z, ok := ZoneFor(zones, host)
		if z.ID != want || ok != (want != "") {
			t.Errorf("ZoneFor(%q) = %q, %v; want %q", host, z.ID, ok, want)
		}
	}
}

func TestRotateTunnelSecret(t *testing.T) {
	var calls []string
	var secret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == "PATCH" {
			var body struct {
				TunnelSecret string `json:"tunnel_secret"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			secret = body.TunnelSecret
		}
		result := `{}`
		if strings.HasSuffix(r.URL.Path, "/token") {
			result = `"new-token"`
		}
		fmt.Fprintf(w, `{"success":true,"errors":[],"result":%s}`, result)
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	token, err := Client{Token: "t"}.RotateTunnelSecret("acc", "tun")
	if err != nil || token != "new-token" {
		t.Fatalf("token %q, %v", token, err)
	}
	if raw, err := base64.StdEncoding.DecodeString(secret); err != nil || len(raw) < 32 {
		t.Errorf("tunnel secret %q isn't 32+ bytes of base64", secret)
	}
	want := []string{"PATCH /accounts/acc/cfd_tunnel/tun", "GET /accounts/acc/cfd_tunnel/tun/token", "DELETE /accounts/acc/cfd_tunnel/tun/connections"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls %v, want %v", calls, want)
	}
}
