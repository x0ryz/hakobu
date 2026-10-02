package cloudflare

import (
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

func TestTokenTemplateURL(t *testing.T) {
	u, err := url.Parse(TokenTemplateURL("hakobu box"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "dash.cloudflare.com" || u.Query().Get("to") != "/:account/api-tokens" || u.Query().Get("name") != "hakobu box" {
		t.Errorf("template URL = %s", u)
	}
	var perms []struct{ Key, Type string }
	if err := json.Unmarshal([]byte(u.Query().Get("permissionGroupKeys")), &perms); err != nil {
		t.Fatal(err)
	}
	want := []struct{ Key, Type string }{{"zone", "read"}, {"dns", "edit"}, {"argotunnel", "edit"}, {"workers_r2", "edit"},
		{"zone_settings", "edit"}, {"email_routing_address", "edit"}, {"email_sending", "edit"}}
	if !slices.Equal(perms, want) {
		t.Errorf("permissions = %v, want %v", perms, want)
	}
}

func TestCheckToken(t *testing.T) {
	var zones, tunnels string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			fmt.Fprint(w, `{"success":false,"errors":[{"message":"Invalid API Token"}]}`)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/zones"):
			fmt.Fprint(w, zones)
		case strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			fmt.Fprint(w, tunnels)
		}
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	ok := `{"success":true,"errors":[],"result":[]}`
	denied := `{"success":false,"errors":[{"message":"Authentication error"}]}`
	oneZone := `{"success":true,"errors":[],"result":[{"id":"z1","name":"example.com","account":{"id":"acc"}}]}`
	for _, c := range []struct {
		name, token, zones, tunnels, wantErr string
	}{
		{"works", "tok", oneZone, ok, ""},
		{"bad token", "nope", oneZone, ok, "doesn't work"},
		{"no zones", "tok", ok, ok, "sees no domains"},
		{"no tunnel permission", "tok", oneZone, denied, "Cloudflare Tunnel Edit"},
	} {
		zones, tunnels = c.zones, c.tunnels
		got, err := Client{Token: c.token}.CheckToken()
		switch {
		case c.wantErr == "" && (err != nil || len(got) != 1):
			t.Errorf("%s: %v %v", c.name, got, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: error %v, want %q", c.name, err, c.wantErr)
		}
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

func TestRouteHost(t *testing.T) {
	const hk = `"comment":"managed by hakobu"`
	cases := []struct {
		name, records, tunnel string // the record GET's result, the other tunnel's GET answer
		wantCall, wantErr     string // the write expected, or the refusal
	}{
		{"new", `[]`, "", "POST /zones/z/dns_records", ""},
		{"already ours", `[{"id":"r1","type":"CNAME","content":"mine.cfargotunnel.com",` + hk + `}]`, "", "", ""},
		{"left by a dead tunnel", `[{"id":"r1","type":"CNAME","content":"old.cfargotunnel.com",` + hk + `}]`,
			`{"success":true,"errors":[],"result":{"name":"hakobu-old","status":"inactive"}}`, "PUT /zones/z/dns_records/r1", ""},
		{"tunnel deleted", `[{"id":"r1","type":"CNAME","content":"old.cfargotunnel.com",` + hk + `}]`,
			`{"success":false,"errors":[{"message":"Tunnel not found"}]}`, "PUT /zones/z/dns_records/r1", ""},
		{"another running hakobu", `[{"id":"r1","type":"CNAME","content":"live.cfargotunnel.com",` + hk + `}]`,
			`{"success":true,"errors":[],"result":{"name":"hakobu-laptop","status":"healthy"}}`, "", "running tunnel hakobu-laptop"},
		{"someone else's", `[{"id":"r1","type":"A","content":"1.2.3.4","comment":""}]`, "", "", "didn't create"},
	}
	for _, c := range cases {
		var writes []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/zones/"):
				fmt.Fprintf(w, `{"success":true,"errors":[],"result":%s}`, c.records)
			case r.Method == "GET":
				fmt.Fprint(w, c.tunnel)
			default:
				writes = append(writes, r.Method+" "+r.URL.Path)
				fmt.Fprint(w, `{"success":true,"errors":[],"result":{"id":"r9"}}`)
			}
		}))
		old := APIURL
		APIURL = srv.URL
		_, err := Client{Token: "t"}.RouteHost("acc", "z", "hakobu.example.com", "mine")
		APIURL = old
		srv.Close()
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || len(writes) > 0 {
				t.Errorf("%s: err %v, writes %v; want refusal %q and no writes", c.name, err, writes, c.wantErr)
			}
			continue
		}
		if err != nil || (c.wantCall == "" && len(writes) > 0) || (c.wantCall != "" && (len(writes) != 1 || writes[0] != c.wantCall)) {
			t.Errorf("%s: err %v, writes %v, want %q", c.name, err, writes, c.wantCall)
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
