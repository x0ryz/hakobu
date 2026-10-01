// Package cloudflare connects hakobu to a Cloudflare account through an API
// token the owner creates for it and manages the tunnel and DNS records
// hakobu needs.
//
// Not OAuth: Cloudflare's token endpoint lives on dash.cloudflare.com,
// whose bot protection challenges many server networks (Hetzner,
// DigitalOcean, VPNs), so a server can't trade codes or refresh tokens
// itself, and doing it elsewhere means a third party sees the tokens. The
// API (api.cloudflare.com) isn't challenged, and an API token needs no
// refreshing.
package cloudflare

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// APIURL is the API's base; tests point it at a fake.
var APIURL = "https://api.cloudflare.com/client/v4"

// tokenPermissions are what hakobu needs: the account's domains, their DNS
// records, a tunnel, and R2 for the database backups.
var tokenPermissions = []struct{ Key, Type string }{
	{"zone", "read"},
	{"dns", "edit"},
	{"argotunnel", "edit"},
	{"workers_r2", "edit"},
}

// TokenTemplateURL opens the dashboard's form for a new account-owned API
// token with hakobu's permissions filled in; name is the token's name.
func TokenTemplateURL(name string) string {
	perms, _ := json.Marshal(func() []map[string]string {
		var out []map[string]string
		for _, p := range tokenPermissions {
			out = append(out, map[string]string{"key": p.Key, "type": p.Type})
		}
		return out
	}())
	return "https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys=" +
		url.QueryEscape(string(perms)) + "&name=" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}

// CheckToken tells what the token can't do of what hakobu needs, as
// advice for the owner; it only reads, so DNS editing and R2 show up when
// first used.
func (c Client) CheckToken() ([]Zone, error) {
	zones, err := c.Zones()
	if err != nil {
		return nil, fmt.Errorf("the token doesn't work: %w", err)
	}
	if len(zones) == 0 {
		return nil, fmt.Errorf("the token sees no domains: give it Zone Read and DNS Edit for the domains hakobu should use")
	}
	if err := c.call("GET", "/accounts/"+zones[0].Account.ID+"/cfd_tunnel?per_page=1", nil, nil); err != nil {
		return nil, fmt.Errorf("the token can't manage tunnels: give it Cloudflare Tunnel Edit (%w)", err)
	}
	return zones, nil
}

// Client calls the Cloudflare API with an API token.
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

// RouteHost points host at the tunnel with a proxied CNAME and returns the
// record ID. Every hakobu marks its records the same way, so a record that
// leads to another tunnel is taken over only when that tunnel is gone or
// has no connection: it may belong to another hakobu that is running. A
// record hakobu didn't create is never touched.
func (c Client) RouteHost(accountID, zoneID, host, tunnelID string) (string, error) {
	target := tunnelID + ".cfargotunnel.com"
	var existing []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Content string `json:"content"`
		Comment string `json:"comment"`
	}
	if err := c.call("GET", "/zones/"+zoneID+"/dns_records?name="+url.QueryEscape(host), nil, &existing); err != nil {
		return "", err
	}
	for _, r := range existing {
		if r.Comment != hakobuRecord || r.Type != "CNAME" || !strings.HasSuffix(r.Content, ".cfargotunnel.com") {
			return "", fmt.Errorf("%s: a DNS record that hakobu didn't create already exists (remove it in Cloudflare first)", host)
		}
	}
	if len(existing) > 1 {
		return "", fmt.Errorf("%s has %d DNS records; remove the extra ones in Cloudflare first", host, len(existing))
	}
	body := map[string]any{"type": "CNAME", "name": host, "content": target, "proxied": true, "comment": hakobuRecord}
	var rec struct {
		ID string `json:"id"`
	}
	if len(existing) == 1 {
		r := existing[0]
		if r.Content == target {
			return r.ID, nil
		}
		other := strings.TrimSuffix(r.Content, ".cfargotunnel.com")
		if name, alive, err := c.tunnelAlive(accountID, other); err != nil {
			return "", fmt.Errorf("%s: %w", host, err)
		} else if alive {
			return "", fmt.Errorf("%s is in use by the running tunnel %s (another hakobu?); pick another name or stop that one first", host, name)
		}
		if err := c.call("PUT", "/zones/"+zoneID+"/dns_records/"+r.ID, body, &rec); err != nil {
			return "", fmt.Errorf("%s: %w", host, err)
		}
		return rec.ID, nil
	}
	if err := c.call("POST", "/zones/"+zoneID+"/dns_records", body, &rec); err != nil {
		return "", fmt.Errorf("%s: %w", host, err)
	}
	return rec.ID, nil
}

const hakobuRecord = "managed by hakobu"

// tunnelAlive reports whether the tunnel exists and has a connection; a
// deleted or unknown tunnel isn't alive.
func (c Client) tunnelAlive(accountID, tunnelID string) (name string, alive bool, err error) {
	var t struct {
		Name      string  `json:"name"`
		Status    string  `json:"status"`
		DeletedAt *string `json:"deleted_at"`
	}
	if err := c.call("GET", "/accounts/"+accountID+"/cfd_tunnel/"+url.PathEscape(tunnelID), nil, &t); err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "Not Found") {
			return "", false, nil
		}
		return "", false, err
	}
	return t.Name, t.DeletedAt == nil && (t.Status == "healthy" || t.Status == "degraded"), nil
}

func (c Client) DeleteRecord(zoneID, recordID string) error {
	return c.call("DELETE", "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
}
