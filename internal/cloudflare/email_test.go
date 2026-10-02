package cloudflare

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestEmail(t *testing.T) {
	var calls []string
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		switch r.Method + " " + r.URL.Path {
		case "GET /zones/z1/email/routing":
			// On for a subdomain only: the zone says enabled, the apex isn't.
			fmt.Fprint(w, `{"success":true,"result":{"enabled":true,"status":"misconfigured","subdomains":[{"name":"mail.example.com","enabled":true,"status":"ready"},{"name":"old.example.com","enabled":true,"status":"unconfigured"}]}}`)
		case "POST /zones/z1/email/routing/dns":
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Email Routing is already enabled"}]}`)
		case "DELETE /zones/z1/email/routing/dns", "DELETE /accounts/acc/email/routing/addresses/a2":
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		case "GET /accounts/acc/email/routing/addresses":
			fmt.Fprint(w, `{"success":true,"result":[{"id":"a1","email":"Me@Example.org","verified":"2026-10-02T00:00:00Z"},{"id":"a2","email":"new@example.org","verified":null}]}`)
		case "POST /accounts/acc/email/routing/addresses":
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		case "GET /zones/z1/dns_records":
			fmt.Fprint(w, `{"success":true,"result":[{"id":"r1","content":"\"v=spf1 include:_spf.mx.cloudflare.net ~all\""},{"id":"r2","content":"\"google-site-verification=x\""}]}`)
		case "POST /accounts/acc/email/sending/send":
			_ = json.Unmarshal(body, &sent)
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()
	c := Client{Token: "tok"}

	if r, err := c.EmailRouting("z1"); err != nil || r.ApexReady || !slices.Equal(r.Subdomains, []string{"mail.example.com"}) {
		t.Errorf("EmailRouting = %+v, %v", r, err)
	}
	if err := c.EnableEmailRouting("z1", "mail.example.com"); err != nil {
		t.Errorf("enabling it again: %v", err)
	}
	if err := c.DisableEmailRouting("z1", ""); err == nil {
		t.Error("disabled Email Routing for the whole zone")
	}
	if d, err := c.Destination("acc", "me@example.org"); err != nil || !d.Verified {
		t.Errorf("confirmed address: %+v, %v", d, err)
	}
	if d, err := c.Destination("acc", "new@example.org"); err != nil || d.Verified {
		t.Errorf("unconfirmed address: %+v, %v", d, err)
	}
	if _, err := c.Destination("acc", "nobody@example.org"); err == nil {
		t.Error("found an address that isn't there")
	}
	if added, err := c.AddDestination("acc", "me@example.org"); err != nil || added {
		t.Errorf("adding a known address: added %v, %v", added, err)
	}
	if added, err := c.AddDestination("acc", "other@example.org"); err != nil || !added {
		t.Errorf("adding a new address: added %v, %v", added, err)
	}
	if err := c.DeleteDestination("acc", "new@example.org"); err != nil {
		t.Error(err)
	}
	if err := c.DeleteDestination("acc", "gone@example.org"); err != nil {
		t.Errorf("deleting an address that isn't there: %v", err)
	}
	if spf, err := c.SPFRecords("z1", "example.com"); err != nil || len(spf) != 1 || spf["r1"] == "" {
		t.Errorf("SPF records = %v, %v", spf, err)
	}

	err := c.SendEmail("acc", Email{FromAddress: "hakobu@mail.example.com", FromName: "Hakobu", To: "me@example.org", Subject: "s", Text: "t"})
	from, _ := sent["from"].(map[string]any)
	if err != nil || from["address"] != "hakobu@mail.example.com" || from["name"] != "Hakobu" || sent["to"] != "me@example.org" || sent["text"] != "t" {
		t.Errorf("sent %v, %v", sent, err)
	}
}
