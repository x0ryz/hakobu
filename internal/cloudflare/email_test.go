package cloudflare

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmail(t *testing.T) {
	var posted []string
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "GET /zones/z1/email/routing":
			fmt.Fprint(w, `{"success":true,"result":{"enabled":true,"name":"example.com"}}`)
		case "POST /zones/z1/email/routing/dns":
			posted = append(posted, string(body))
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Email Routing is already enabled"}]}`)
		case "GET /accounts/acc/email/routing/addresses":
			fmt.Fprint(w, `{"success":true,"result":[{"id":"a1","email":"Me@Example.org","verified":"2026-10-02T00:00:00Z"},{"id":"a2","email":"new@example.org","verified":null}]}`)
		case "POST /accounts/acc/email/routing/addresses":
			posted = append(posted, string(body))
			fmt.Fprint(w, `{"success":true,"result":{}}`)
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

	if on, err := c.EmailRouting("z1"); err != nil || !on {
		t.Errorf("EmailRouting = %v, %v", on, err)
	}
	if err := c.EnableEmailRouting("z1", "mail.example.com"); err != nil {
		t.Errorf("enabling it again: %v", err)
	}
	if _, verified, err := c.Destination("acc", "me@example.org"); err != nil || !verified {
		t.Errorf("confirmed address: verified %v, %v", verified, err)
	}
	if _, verified, err := c.Destination("acc", "new@example.org"); err != nil || verified {
		t.Errorf("unconfirmed address: verified %v, %v", verified, err)
	}
	if _, _, err := c.Destination("acc", "nobody@example.org"); err == nil {
		t.Error("found an address that isn't there")
	}
	posted = nil
	if err := c.AddDestination("acc", "me@example.org"); err != nil || len(posted) != 0 {
		t.Errorf("adding a known address: %v, posted %v", err, posted)
	}
	if err := c.AddDestination("acc", "other@example.org"); err != nil || len(posted) != 1 {
		t.Errorf("adding a new address: %v, posted %v", err, posted)
	}

	err := c.SendEmail("acc", Email{FromAddress: "hakobu@mail.example.com", FromName: "Hakobu", To: "me@example.org", Subject: "s", Text: "t"})
	from, _ := sent["from"].(map[string]any)
	if err != nil || from["address"] != "hakobu@mail.example.com" || from["name"] != "Hakobu" || sent["to"] != "me@example.org" || sent["text"] != "t" {
		t.Errorf("sent %v, %v", sent, err)
	}
}
