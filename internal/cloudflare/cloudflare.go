// Package cloudflare connects hakobu to a Cloudflare account through OAuth
// (authorization code + PKCE, no client secret) and manages the tunnel and
// DNS records hakobu needs.
package cloudflare

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	authURL  = "https://dash.cloudflare.com/oauth2/auth"
	tokenURL = "https://dash.cloudflare.com/oauth2/token"
	// R2 holds the database backups: buckets (workers-r2) and their files
	// (workers-r2-bucket-item).
	scopes = "zone.read dns.write argotunnel.write workers-r2.write workers-r2-bucket-item.write offline_access"
)

// APIURL is the API's base; tests point it at a fake.
var APIURL = "https://api.cloudflare.com/client/v4"

// PKCE returns a random code verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func AuthorizeURL(clientID, redirectURI, state, challenge string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("scope", scopes)
	v.Set("state", state)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	return authURL + "?" + v.Encode()
}

type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Exchange trades an authorization code for tokens.
func Exchange(clientID, redirectURI, code, verifier string) (Token, error) {
	return tokenRequest(url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code":          {code},
		"code_verifier": {verifier},
	})
}

func Refresh(clientID, refreshToken string) (Token, error) {
	return tokenRequest(url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	})
}

func tokenRequest(form url.Values) (Token, error) {
	resp, err := http.PostForm(tokenURL, form)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &res) != nil || res.AccessToken == "" {
		return Token{}, fmt.Errorf("cloudflare token request failed (%d): %s", resp.StatusCode, body)
	}
	return Token{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(res.ExpiresIn) * time.Second),
	}, nil
}

// Poll asks the relay for the result of the login started with state; ok is
// false while the user hasn't authorized yet.
func Poll(relay, state string) (code string, ok bool, err error) {
	resp, err := http.Get(relay + "/cf/poll?state=" + url.QueryEscape(state))
	if err != nil {
		return "", false, nil // transient, keep polling
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	var res struct{ Code, Error string }
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("relay %s answered %d", relay, resp.StatusCode)
	}
	if res.Error != "" {
		return "", false, fmt.Errorf("Cloudflare login failed: %s", res.Error)
	}
	return res.Code, true, nil
}

// Client calls the Cloudflare API with an OAuth access token.
type Client struct{ Token string }

func (c Client) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.do(method, path, "application/json", body, -1)
	if err != nil {
		return err
	}
	return decode(method, path, resp, out)
}

// do sends a request; size is the body's length, -1 to let net/http work
// it out.
func (c Client) do(method, path, contentType string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequest(method, APIURL+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", contentType)
	return http.DefaultClient.Do(req)
}

// decode reads the API's JSON envelope and unmarshals its result into out.
func decode(method, path string, resp *http.Response, out any) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("cloudflare %s %s (%d): %s", method, path, resp.StatusCode, raw)
	}
	if !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("cloudflare %s %s: %s", method, path, strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

type Zone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

func (c Client) Zones() ([]Zone, error) {
	var zones []Zone
	err := c.call("GET", "/zones?per_page=50&status=active", nil, &zones)
	return zones, err
}

// ZoneFor returns the zone a hostname belongs to (the longest matching name).
func ZoneFor(zones []Zone, host string) (Zone, bool) {
	var best Zone
	for _, z := range zones {
		if (host == z.Name || strings.HasSuffix(host, "."+z.Name)) && len(z.Name) > len(best.Name) {
			best = z
		}
	}
	return best, best.ID != ""
}

// CreateTunnel creates a remotely managed tunnel that sends everything to
// service and returns its ID and run token.
func (c Client) CreateTunnel(accountID, name, service string) (id, token string, err error) {
	var t struct {
		ID string `json:"id"`
	}
	if err := c.call("POST", "/accounts/"+accountID+"/cfd_tunnel", map[string]any{"name": name, "config_src": "cloudflare"}, &t); err != nil {
		return "", "", err
	}
	if err := c.SetIngress(accountID, t.ID, []IngressRule{{Service: service}}); err != nil {
		return "", "", err
	}
	err = c.call("GET", "/accounts/"+accountID+"/cfd_tunnel/"+t.ID+"/token", nil, &token)
	return t.ID, token, err
}

// RotateTunnelSecret gives the tunnel a new secret and returns its new run
// token, then disconnects every connector: one run with the old token (a
// stolen copy, say) can't connect again, and cloudflared reconnects with
// the new one once it's restarted.
func (c Client) RotateTunnelSecret(accountID, tunnelID string) (token string, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	path := "/accounts/" + accountID + "/cfd_tunnel/" + tunnelID
	if err := c.call("PATCH", path, map[string]string{"tunnel_secret": base64.StdEncoding.EncodeToString(secret)}, nil); err != nil {
		return "", err
	}
	if err := c.call("GET", path+"/token", nil, &token); err != nil {
		return "", err
	}
	return token, c.call("DELETE", path+"/connections", nil, nil)
}

// IngressRule sends requests for Hostname (any, if empty) to Service. The
// last rule must have no hostname.
type IngressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

// SetIngress replaces the tunnel's routing; cloudflared picks it up within
// seconds.
func (c Client) SetIngress(accountID, tunnelID string, rules []IngressRule) error {
	return c.call("PUT", "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/configurations",
		map[string]any{"config": map[string]any{"ingress": rules}}, nil)
}

// RouteHost points host at the tunnel with a proxied CNAME and returns the record ID.
func (c Client) RouteHost(zoneID, host, tunnelID string) (string, error) {
	var rec struct {
		ID string `json:"id"`
	}
	err := c.call("POST", "/zones/"+zoneID+"/dns_records", map[string]any{
		"type": "CNAME", "name": host, "content": tunnelID + ".cfargotunnel.com", "proxied": true, "comment": "managed by hakobu",
	}, &rec)
	if err != nil {
		return "", fmt.Errorf("%s: %w (remove the existing DNS record for it in Cloudflare first)", host, err)
	}
	return rec.ID, nil
}

func (c Client) DeleteRecord(zoneID, recordID string) error {
	return c.call("DELETE", "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
}
