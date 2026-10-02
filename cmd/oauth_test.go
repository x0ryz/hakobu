package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
)

// TestOAuthAndMCP walks Claude's way in: discovery from a 401, a client
// described by a metadata document, the owner's consent, the code for
// tokens with PKCE, MCP tool calls, a refresh, and disconnecting.
func TestOAuthAndMCP(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := config.PrepareDataDir(); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("hakobu.example.com"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(config.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SetOwner(ctx, store.SetOwnerParams{GitHubID: 42, GitHubLogin: "me"}); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(ctx, "tok", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	keyFreshFor = time.Hour
	t.Cleanup(func() { keyFreshFor = 5 * time.Minute })

	// The client's metadata document, like the one Claude Code publishes.
	var clientID string
	docs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": clientID, "client_name": "Claude Code", "token_endpoint_auth_method": "none",
			"redirect_uris": []string{"http://localhost/callback", "http://127.0.0.1/callback"},
		})
	}))
	defer docs.Close()
	clientID = docs.URL + "/client.json"
	saved := clientDocHTTP
	clientDocHTTP = docs.Client()
	t.Cleanup(func() { clientDocHTTP = saved })

	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	h := panelHandler(mux)
	do := func(method, path string, body url.Values, header map[string]string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != nil {
			r = httptest.NewRequest(method, "https://hakobu.example.com"+path, strings.NewReader(body.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, "https://hakobu.example.com"+path, nil)
		}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	owner := map[string]string{"Cookie": sessionCookie + "=tok", "Sec-Fetch-Site": "same-origin"}

	// Discovery.
	w := do("POST", "/mcp", nil, nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), `resource_metadata="https://hakobu.example.com/.well-known/oauth-protected-resource"`) {
		t.Fatalf("no token: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	var prm struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	_ = json.NewDecoder(do("GET", "/.well-known/oauth-protected-resource", nil, nil).Body).Decode(&prm)
	if prm.Resource != "https://hakobu.example.com/mcp" || len(prm.Servers) != 1 || prm.Servers[0] != "https://hakobu.example.com" {
		t.Errorf("protected resource metadata: %+v", prm)
	}
	var asm map[string]any
	_ = json.NewDecoder(do("GET", "/.well-known/oauth-authorization-server", nil, nil).Body).Decode(&asm)
	if asm["client_id_metadata_document_supported"] != true || asm["token_endpoint"] != "https://hakobu.example.com/oauth/token" {
		t.Errorf("authorization server metadata: %v", asm)
	}

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"http://localhost:3118/callback"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"state": {"xyz"}, "scope": {"read deploy"}, "resource": {"https://hakobu.example.com/mcp"},
	}.Encode()

	// Signed out: to GitHub and back.
	w = do("GET", "/oauth/authorize?"+query, nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/auth/login" || !strings.Contains(w.Header().Get("Set-Cookie"), afterCookie+"="+oauthAfterPrefix) {
		t.Fatalf("signed out: %d %v", w.Code, w.Header())
	}

	// A redirect URI the client didn't publish is refused here, not followed.
	bad := strings.Replace(query, "localhost%3A3118", "evil.example", 1)
	if w := do("GET", "/oauth/authorize?"+bad, nil, owner); w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
		t.Errorf("foreign redirect URI: %d %v", w.Code, w.Header())
	}

	w = do("GET", "/oauth/authorize?"+query, nil, owner)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Claude Code") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self' http://localhost:3118;") {
		t.Fatalf("consent page: %d %v", w.Code, w.Header())
	}

	// Approved without deploys.
	w = do("POST", "/oauth/authorize", url.Values{"request": {query}, "approve": {"1"}}, owner)
	loc, _ := url.Parse(w.Header().Get("Location"))
	code := loc.Query().Get("code")
	if w.Code != http.StatusSeeOther || loc.Host != "localhost:3118" || code == "" || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != "https://hakobu.example.com" {
		t.Fatalf("approval: %d %v", w.Code, w.Header())
	}

	exchange := func(form url.Values) (int, map[string]any) {
		w := do("POST", "/oauth/token", form, map[string]string{"Origin": "https://claude.ai"})
		var out map[string]any
		_ = json.NewDecoder(w.Body).Decode(&out)
		return w.Code, out
	}
	codeForm := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {"http://localhost:3118/callback"}, "code_verifier": {verifier}}
	status, tokens := exchange(codeForm)
	if status != http.StatusOK || tokens["scope"] != "read" || tokens["token_type"] != "Bearer" {
		t.Fatalf("code exchange: %d %v", status, tokens)
	}
	if status, out := exchange(codeForm); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Errorf("code used twice: %d %v", status, out)
	}

	call := func(token, tool, args string) map[string]any {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		r := httptest.NewRequest("POST", "https://hakobu.example.com/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Origin", "https://claude.ai")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
			t.Fatalf("%s: %d, %v", tool, w.Code, err)
		}
		return out
	}
	access := tokens["access_token"].(string)
	if out := call(access, "list_apps", `{}`); out["result"] == nil || out["result"].(map[string]any)["isError"] == true {
		t.Errorf("list_apps: %v", out)
	}
	if out := call(access, "deploy", `{"app":"web"}`); !strings.Contains(jsonString(out), "may only read") {
		t.Errorf("deploy with a read-only token: %v", out)
	}

	status, refreshed := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {clientID}})
	if status != http.StatusOK || refreshed["access_token"] == access {
		t.Fatalf("refresh: %d %v", status, refreshed)
	}
	if status, _ := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshed["refresh_token"].(string)}, "client_id": {"someone-else"}}); status != http.StatusBadRequest {
		t.Errorf("refresh by another client: %d", status)
	}

	grants, err := s.LiveOAuthGrants(ctx)
	if err != nil || len(grants) != 1 || grants[0].ClientName != "Claude Code" {
		t.Fatalf("grants: %v %v", grants, err)
	}
	if w := do("DELETE", "/settings/ai-apps/"+jsonString(grants[0].ID), nil, owner); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/mcp", nil, map[string]string{"Authorization": "Bearer " + refreshed["access_token"].(string)}); w.Code != http.StatusUnauthorized {
		t.Errorf("token of a disconnected app: %d", w.Code)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRedirectURIs(t *testing.T) {
	registered := []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost/callback", "http://127.0.0.1/callback"}
	for got, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback":      true,
		"https://claude.ai/api/mcp/auth_callback?x=1":  false,
		"https://claude.ai:8443/api/mcp/auth_callback": false,
		"http://localhost:3118/callback":               true,
		"http://127.0.0.1:50000/callback":              true,
		"http://localhost:3118/other":                  false,
		"http://evil.example:3118/callback":            false,
		"https://localhost:3118/callback":              false,
		"http://user@localhost:3118/callback":          false,
		"":                                             false,
	} {
		if redirectAllowed(registered, got) != want {
			t.Errorf("redirectAllowed(%q) = %v", got, !want)
		}
	}
	for uris, ok := range map[string]bool{
		"https://claude.ai/cb":      true,
		"http://localhost/cb":       true,
		"http://[::1]:80/cb":        true,
		"http://example.com/cb":     false,
		"javascript:alert(1)":       false,
		"https://x.example/cb#frag": false,
		"":                          false,
	} {
		list := []string{uris}
		if uris == "" {
			list = nil
		}
		if err := checkRedirectURIs(list); (err == nil) != ok {
			t.Errorf("checkRedirectURIs(%q) = %v", uris, err)
		}
	}
}

func TestClientDocumentsOnlyFromPublicAddresses(t *testing.T) {
	for addr, public := range map[string]bool{
		"160.79.104.10": true, "2606:4700::1": true,
		"127.0.0.1": false, "10.0.0.1": false, "192.168.1.1": false, "172.17.0.2": false, "169.254.169.254": false,
		"100.64.0.1": false, "::1": false, "fe80::1": false, "fd00::1": false, "::ffff:127.0.0.1": false, "0.0.0.0": false,
	} {
		if isPublicAddr(netip.MustParseAddr(addr)) != public {
			t.Errorf("isPublicAddr(%s) = %v", addr, !public)
		}
	}
	// The real client refuses to dial this machine.
	if _, err := fetchClientDocument(context.Background(), "https://127.0.0.1:1/client.json"); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("fetching from loopback: %v", err)
	}
}
