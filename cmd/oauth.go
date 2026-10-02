package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

// hakobu is the OAuth authorization server for its own MCP endpoint: Claude
// (claude.ai, the desktop and mobile apps, Claude Code) connects to
// https://<panel>/mcp, the owner signs in with GitHub and approves it, and
// it gets tokens for the tools those scopes allow.
//
// Clients are public (no secret) and must use PKCE. They identify
// themselves with a Client ID Metadata Document, the URL of a JSON file
// they publish, or register through Dynamic Client Registration.
const (
	scopeRead   = "read"   // projects, apps, deploys, logs and errors
	scopeDeploy = "deploy" // deploy, roll back, restart workers
)

var oauthScopes = []string{scopeRead, scopeDeploy}

// maxOAuthClients caps registered clients awaiting approval, so anyone on
// the internet registering can't fill the database; the unapproved ones
// are pruned after a day.
const maxOAuthClients = 100

func oauthIssuer() string    { return "https://" + config.PublicHost() }
func mcpResourceURL() string { return oauthIssuer() + "/mcp" }

// oauthClient is a client asking for access: registered here, or described
// by the document at its ID.
type oauthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
}

func registerOAuthRoutes(mux *http.ServeMux, s *store.Store) {
	prm := func(w http.ResponseWriter, r *http.Request) {
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               mcpResourceURL(),
			AuthorizationServers:   []string{oauthIssuer()},
			ScopesSupported:        oauthScopes,
			BearerMethodsSupported: []string{"header"},
			ResourceName:           "Hakobu (" + config.PublicHost() + ")",
		}).ServeHTTP(w, r)
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", prm)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", prm)

	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		iss := oauthIssuer()
		w.Header().Set("Access-Control-Allow-Origin", "*")
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                         iss,
			"authorization_endpoint":                         iss + "/oauth/authorize",
			"token_endpoint":                                 iss + "/oauth/token",
			"registration_endpoint":                          iss + "/oauth/register",
			"scopes_supported":                               append(slices.Clone(oauthScopes), "offline_access"),
			"response_types_supported":                       []string{"code"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"code_challenge_methods_supported":               []string{"S256"},
			"client_id_metadata_document_supported":          true,
			"authorization_response_iss_parameter_supported": true,
		})
	})

	mux.HandleFunc("POST /oauth/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
			ClientName   string   `json:"client_name"`
			AuthMethod   string   `json:"token_endpoint_auth_method"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON")
			return
		}
		if req.AuthMethod != "" && req.AuthMethod != "none" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients (token_endpoint_auth_method none) are supported")
			return
		}
		if err := checkRedirectURIs(req.RedirectURIs); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
		name := clientName(req.ClientName, "")
		if n, err := s.CountOAuthClients(r.Context()); err != nil || n >= maxOAuthClients {
			oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many clients registered today, try again tomorrow")
			return
		}
		id, err := ops.RandomHex(16)
		if err == nil {
			id = "hakobu_client_" + id
			err = s.CreateOAuthClient(r.Context(), store.CreateOAuthClientParams{ID: id, Name: name, RedirectURIs: strings.Join(req.RedirectURIs, "\n")})
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id":                  id,
			"client_id_issued_at":        time.Now().Unix(),
			"client_name":                name,
			"redirect_uris":              req.RedirectURIs,
			"token_endpoint_auth_method": "none",
			"grant_types":                []string{"authorization_code", "refresh_token"},
			"response_types":             []string{"code"},
		})
	})

	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		ar, ok := parseAuthorizeRequest(w, r, s)
		if !ok {
			return
		}
		if !freshOwnerSession(w, r, s) {
			return
		}
		redirect, _ := url.Parse(ar.RedirectURI)
		// Browsers hold the redirect after the form is sent to form-action too.
		w.Header().Set("Content-Security-Policy", panelCSP(redirect.Scheme+"://"+redirect.Host))
		render(w, "oauth-consent", map[string]any{
			"Client": ar.Client, "Request": ar, "RedirectHost": redirect.Host,
			"Loopback": isLoopback(redirect.Hostname()), "Document": strings.HasPrefix(ar.Client.ID, "https://"),
			"Deploy": slices.Contains(ar.Scopes, scopeDeploy), "PublicHost": config.PublicHost(),
		})
	})

	mux.HandleFunc("POST /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		ar, ok := parseAuthorizeRequest(w, r, s)
		if !ok || !freshOwnerSession(w, r, s) {
			return
		}
		if r.FormValue("approve") == "" {
			ar.fail(w, r, "access_denied", "the owner declined")
			return
		}
		scopes := []string{scopeRead}
		if slices.Contains(ar.Scopes, scopeDeploy) && r.FormValue("deploy") != "" {
			scopes = append(scopes, scopeDeploy)
		}
		owner, err := s.Owner(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		grant, err := s.CreateOAuthGrant(r.Context(), store.CreateOAuthGrantParams{
			ClientID: ar.Client.ID, ClientName: ar.Client.Name, RedirectURI: ar.RedirectURI,
			Scope: strings.Join(scopes, " "), GitHubID: owner.GitHubID,
		})
		if err != nil {
			fail(w, err)
			return
		}
		code, _, err := s.NewOAuthToken(r.Context(), grant, "code", ar.Challenge, store.OAuthCodeTTL)
		if err != nil {
			fail(w, err)
			return
		}
		redirect, _ := url.Parse(ar.RedirectURI)
		ops.NoteOAuthConnection(s, ar.Client.Name, redirect.Host, scopes)
		ar.redirect(w, r, url.Values{"code": {code}})
	})

	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_request", "send the parameters form-urlencoded")
			return
		}
		if res := r.PostForm.Get("resource"); res != "" && res != mcpResourceURL() {
			oauthError(w, http.StatusBadRequest, "invalid_target", "the only resource here is "+mcpResourceURL())
			return
		}
		var grant store.OAuthGrant
		var err error
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			var code store.OAuthToken
			code, grant, err = s.RedeemOAuthToken(r.Context(), r.PostForm.Get("code"), "code")
			if err == nil && (grant.RedirectURI != r.PostForm.Get("redirect_uri") || !pkceMatches(code.CodeChallenge, r.PostForm.Get("code_verifier"))) {
				err = store.ErrOAuthToken
			}
		case "refresh_token":
			_, grant, err = s.RedeemOAuthToken(r.Context(), r.PostForm.Get("refresh_token"), "refresh")
		default:
			oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
			return
		}
		if err == nil && (grant.ClientID != r.PostForm.Get("client_id") || !mayAccess(r.Context(), s, grant.GitHubID)) {
			err = store.ErrOAuthToken
		}
		if errors.Is(err, store.ErrOAuthToken) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
			return
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		access, expires, err := s.NewOAuthToken(r.Context(), grant.ID, "access", "", store.OAuthAccessTTL)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		refresh, _, err := s.NewOAuthToken(r.Context(), grant.ID, "refresh", "", store.OAuthRefreshTTL)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  access,
			"token_type":    "Bearer",
			"expires_in":    int(time.Until(expires).Seconds()),
			"refresh_token": refresh,
			"scope":         grant.Scope,
		})
	})
}

// verifyMCPToken is the MCP endpoint's check of a bearer token: a live
// access token of a grant the current owner made.
func verifyMCPToken(s *store.Store) auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		g, expires, err := s.OAuthAccess(ctx, token)
		if errors.Is(err, store.ErrOAuthToken) || err == nil && !mayAccess(ctx, s, g.GitHubID) {
			return nil, auth.ErrInvalidToken
		}
		if err != nil {
			return nil, err
		}
		return &auth.TokenInfo{Scopes: strings.Fields(g.Scope), Expiration: expires, UserID: fmt.Sprint(g.ID)}, nil
	}
}

// authorizeRequest is a checked request to /oauth/authorize.
type authorizeRequest struct {
	Client      oauthClient
	RedirectURI string
	State       string
	Challenge   string
	Scopes      []string // asked for, of oauthScopes
	Query       string   // the request's parameters, carried through the consent form
}

// parseAuthorizeRequest checks an authorization request. Until the client
// and its redirect URI check out, errors are shown here (sending them to
// an unchecked redirect URI would make the panel an open redirector);
// after, they go back to the client.
func parseAuthorizeRequest(w http.ResponseWriter, r *http.Request, s *store.Store) (authorizeRequest, bool) {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		var err error
		if q, err = url.ParseQuery(r.FormValue("request")); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return authorizeRequest{}, false
		}
	}
	ar := authorizeRequest{RedirectURI: q.Get("redirect_uri"), State: q.Get("state"), Challenge: q.Get("code_challenge"), Query: q.Encode()}
	client, err := resolveOAuthClient(r.Context(), s, q.Get("client_id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		render(w, "oauth-error", "This app can't connect to hakobu: "+err.Error())
		return ar, false
	}
	ar.Client = client
	if !redirectAllowed(client.RedirectURIs, ar.RedirectURI) {
		w.WriteHeader(http.StatusBadRequest)
		render(w, "oauth-error", "This app can't connect to hakobu: "+ar.RedirectURI+" isn't one of its redirect URIs.")
		return ar, false
	}
	switch {
	case q.Get("response_type") != "code":
		ar.fail(w, r, "unsupported_response_type", "only response_type=code is supported")
	case ar.Challenge == "" || q.Get("code_challenge_method") != "S256":
		ar.fail(w, r, "invalid_request", "PKCE with code_challenge_method=S256 is required")
	case q.Get("resource") != "" && q.Get("resource") != mcpResourceURL():
		ar.fail(w, r, "invalid_target", "the only resource here is "+mcpResourceURL())
	default:
		for _, sc := range strings.Fields(q.Get("scope")) {
			if slices.Contains(oauthScopes, sc) && !slices.Contains(ar.Scopes, sc) {
				ar.Scopes = append(ar.Scopes, sc)
			}
		}
		if !slices.Contains(ar.Scopes, scopeRead) {
			// Every tool needs read; a client asking for nothing it knows
			// (or nothing at all) is offered everything.
			ar.Scopes = append([]string{scopeRead}, ar.Scopes...)
			if len(ar.Scopes) == 1 {
				ar.Scopes = slices.Clone(oauthScopes)
			}
		}
		return ar, true
	}
	return ar, false
}

// redirect sends the browser back to the client with params.
func (ar authorizeRequest) redirect(w http.ResponseWriter, r *http.Request, params url.Values) {
	u, _ := url.Parse(ar.RedirectURI)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if ar.State != "" {
		q.Set("state", ar.State)
	}
	q.Set("iss", oauthIssuer())
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (ar authorizeRequest) fail(w http.ResponseWriter, r *http.Request, code, description string) {
	ar.redirect(w, r, url.Values{"error": {code}, "error_description": {description}})
}

// freshOwnerSession lets through the owner signed in within keyFreshFor:
// tokens leave the server, so like the master key they're handed out right
// after a GitHub sign-in. Anyone else is sent to sign in, and back here.
func freshOwnerSession(w http.ResponseWriter, r *http.Request, s *store.Store) bool {
	if signedIn, ok := ownerSession(r, s); ok && time.Since(signedIn) <= keyFreshFor {
		return true
	}
	query := r.URL.RawQuery
	if r.Method == http.MethodPost {
		query = r.FormValue("request")
	}
	after := oauthAfterPrefix + base64.RawURLEncoding.EncodeToString([]byte(query))
	if len(after) > 3000 {
		http.Error(w, "authorization request too long", http.StatusBadRequest)
		return false
	}
	setCookie(w, afterCookie, after, 600)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	return false
}

// oauthAfterPrefix marks an afterCookie that returns to /oauth/authorize
// with the base64url-encoded query that follows it.
const oauthAfterPrefix = "oauth."

// resolveOAuthClient finds a registered client, or fetches the metadata
// document of a client whose ID is its URL.
func resolveOAuthClient(ctx context.Context, s *store.Store, id string) (oauthClient, error) {
	if id == "" {
		return oauthClient{}, fmt.Errorf("no client_id")
	}
	if strings.HasPrefix(id, "https://") {
		return fetchClientDocument(ctx, id)
	}
	c, err := s.GetOAuthClient(ctx, id)
	if err != nil {
		return oauthClient{}, fmt.Errorf("unknown client_id, register it first")
	}
	return oauthClient{ID: c.ID, Name: c.Name, RedirectURIs: strings.Split(c.RedirectURIs, "\n")}, nil
}

// clientDocHTTP fetches client metadata documents. The URL comes from
// whoever opens the authorize link, so it only reaches public addresses:
// not this server, the apps or the local network. Tests replace it.
var clientDocHTTP = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !isPublicAddr(ap.Addr()) {
				return fmt.Errorf("%s is not a public address", address)
			}
			return nil
		}}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10") // carrier-grade NAT

func isPublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !sharedAddressSpace.Contains(a)
}

// fetchClientDocument reads a Client ID Metadata Document: JSON at the
// client ID's https URL, naming that URL as its client_id.
func fetchClientDocument(ctx context.Context, id string) (oauthClient, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return oauthClient{}, fmt.Errorf("client_id %q is not a valid metadata document URL", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return oauthClient{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := clientDocHTTP.Do(req)
	if err != nil {
		return oauthClient{}, fmt.Errorf("couldn't fetch the app's description from %s: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oauthClient{}, fmt.Errorf("couldn't fetch the app's description from %s: %s", id, resp.Status)
	}
	var doc struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&doc); err != nil {
		return oauthClient{}, fmt.Errorf("the app's description at %s isn't valid JSON", id)
	}
	switch {
	case doc.ClientID != id:
		return oauthClient{}, fmt.Errorf("the app's description at %s names another client_id", id)
	case doc.AuthMethod != "" && doc.AuthMethod != "none":
		return oauthClient{}, fmt.Errorf("only public clients (token_endpoint_auth_method none) are supported")
	}
	if err := checkRedirectURIs(doc.RedirectURIs); err != nil {
		return oauthClient{}, err
	}
	return oauthClient{ID: id, Name: clientName(doc.ClientName, u.Host), RedirectURIs: doc.RedirectURIs}, nil
}

// clientName is the name the consent page shows: the client's own, made
// printable and short, or fallback.
func clientName(name, fallback string) string {
	name = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, name)), " ")
	if r := []rune(name); len(r) > 60 {
		name = string(r[:60]) + "…"
	}
	if name == "" {
		name = fallback
	}
	if name == "" {
		name = "Unnamed app"
	}
	return name
}

// checkRedirectURIs accepts https URLs and, for apps on the owner's own
// machine like Claude Code, http on a loopback address.
func checkRedirectURIs(uris []string) error {
	if len(uris) == 0 || len(uris) > 10 {
		return fmt.Errorf("give between 1 and 10 redirect_uris")
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil ||
			u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
			return fmt.Errorf("redirect URI %q must be https, or http on localhost", raw)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// redirectAllowed matches a redirect URI to the client's exactly, except
// that a loopback one may be on any port (RFC 8252 7.3): native apps like
// Claude Code listen on whatever port is free.
func redirectAllowed(registered []string, got string) bool {
	if got == "" {
		return false
	}
	g, err := url.Parse(got)
	if err != nil {
		return false
	}
	for _, raw := range registered {
		if raw == got {
			return true
		}
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "http" && g.Scheme == "http" && isLoopback(u.Hostname()) &&
			u.Hostname() == g.Hostname() && u.Path == g.Path && u.RawQuery == g.RawQuery && g.User == nil && g.Fragment == "" {
			return true
		}
	}
	return false
}

// pkceMatches checks a PKCE code_verifier against its S256 challenge.
func pkceMatches(challenge, verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
