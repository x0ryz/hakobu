package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
)

// fakeEmail is Cloudflare's API for one zone, example.com, as far as email
// to the owner goes, with its quirks: turning Email Routing on for a
// subdomain marks the zone enabled and writes an SPF record at the apex.
type fakeEmail struct {
	mu        sync.Mutex
	apexReady bool
	zoneOn    bool                 // stays on once anything turned it on, as Cloudflare's does
	routed    map[string]bool      // subdomains with Email Routing
	addresses map[string]bool      // address → verified
	txt       map[string][2]string // record ID → name, content
	mx        []string             // the apex's mail servers
	subjects  []string
	nextID    int
}

func newFakeEmail(t *testing.T) *fakeEmail {
	f := &fakeEmail{routed: map[string]bool{}, addresses: map[string]bool{}, txt: map[string][2]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		ok := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}) }
		route := r.Method + " " + r.URL.Path
		switch {
		case route == "GET /zones":
			ok([]map[string]any{{"id": "z1", "name": "example.com", "account": map[string]string{"id": "acc"}}})
		case route == "GET /zones/z1/email/routing":
			var subs []map[string]any
			for n := range f.routed {
				subs = append(subs, map[string]any{"name": n, "enabled": true, "status": "ready"})
			}
			status := "misconfigured"
			if f.apexReady {
				status = "ready"
			}
			ok(map[string]any{"enabled": f.zoneOn || f.apexReady, "status": status, "subdomains": subs})
		case route == "POST /zones/z1/email/routing/dns":
			f.zoneOn = true
			switch body["name"] {
			case "example.com":
				t.Error("named the apex, which the API refuses")
			case "":
				f.apexReady, f.mx = true, []string{"route1.mx.cloudflare.net"}
			default:
				f.routed[body["name"]] = true
				f.addTXT(body["name"], cloudflare.CloudflareSPF)
			}
			if len(f.spf("example.com")) == 0 {
				f.addTXT("example.com", cloudflare.CloudflareSPF)
			}
			ok(map[string]any{})
		case route == "DELETE /zones/z1/email/routing/dns":
			if body["name"] == "" || body["name"] == "example.com" {
				t.Error("turned Email Routing off for the whole zone")
			}
			delete(f.routed, body["name"])
			ok(map[string]any{})
		case route == "GET /zones/z1/dns_records" && r.URL.Query().Get("type") == "MX":
			var list []map[string]string
			if r.URL.Query().Get("name") == "example.com" {
				for _, m := range f.mx {
					list = append(list, map[string]string{"content": m})
				}
			}
			ok(list)
		case route == "GET /zones/z1/dns_records":
			var list []map[string]string
			for _, id := range f.spf(r.URL.Query().Get("name")) {
				list = append(list, map[string]string{"id": id, "content": f.txt[id][1]})
			}
			ok(list)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/zones/z1/dns_records/"):
			delete(f.txt, strings.TrimPrefix(r.URL.Path, "/zones/z1/dns_records/"))
			ok(map[string]any{})
		case route == "GET /accounts/acc/email/routing/addresses":
			var list []map[string]any
			for a, verified := range f.addresses {
				v := any(nil)
				if verified {
					v = "2026-10-02T00:00:00Z"
				}
				list = append(list, map[string]any{"id": a, "email": a, "verified": v})
			}
			ok(list)
		case route == "POST /accounts/acc/email/routing/addresses":
			f.addresses[body["email"]] = false
			ok(map[string]any{})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/accounts/acc/email/routing/addresses/"):
			delete(f.addresses, strings.TrimPrefix(r.URL.Path, "/accounts/acc/email/routing/addresses/"))
			ok(map[string]any{})
		case route == "POST /accounts/acc/email/sending/send":
			var m struct{ Subject string }
			_ = json.Unmarshal(b, &m)
			f.subjects = append(f.subjects, m.Subject)
			ok(map[string]any{})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	old := cloudflare.APIURL
	cloudflare.APIURL = srv.URL
	t.Cleanup(func() { cloudflare.APIURL = old })
	return f
}

func (f *fakeEmail) addTXT(name, content string) {
	f.nextID++
	f.txt[fmt.Sprint("r", f.nextID)] = [2]string{name, content}
}

// spf returns the IDs of name's SPF records.
func (f *fakeEmail) spf(name string) []string {
	var ids []string
	for id, r := range f.txt {
		if r[0] == name && strings.Contains(r[1], "v=spf1") {
			ids = append(ids, id)
		}
	}
	return ids
}

func (f *fakeEmail) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subjects...)
}

// notifyStore is a panel at panel.example.com with Cloudflare connected.
func notifyStore(t *testing.T) *store.Store {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("data", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("panel.example.com"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join("data", "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareToken(ctx(), "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}); err != nil {
		t.Fatal(err)
	}
	old := async
	async = func(f func()) { f() }
	t.Cleanup(func() { async = old })
	problems.Lock()
	problems.mailed = nil
	problems.Unlock()
	return s
}

func TestSetupNotifications(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	f.addresses["old@example.org"] = true // confirmed in Cloudflare before
	f.mx = []string{"mx1.other.example"}  // the domain's mail goes elsewhere
	if err := s.SetOwner(ctx(), store.SetOwnerParams{GitHubID: 1, GitHubLogin: "me"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwnerEmail(ctx(), "me@example.org"); err != nil {
		t.Fatal(err)
	}

	// Email Routing is off: the panel's host is offered, the apex isn't,
	// and why is said.
	to, from, why, err := NotifyChoices(s)
	if err != nil || len(to) != 2 || to[0].Value != "old@example.org" || to[1].Value != "me@example.org" ||
		len(from) != 1 || from[0].Value != "panel.example.com" || !strings.Contains(from[0].Note, "mail.panel.example.com") ||
		len(why) != 1 || !strings.Contains(why[0], "mx1.other.example") {
		t.Fatalf("choices: to %+v, from %+v, why %v, %v", to, from, why, err)
	}
	if err := SetupNotifications(s, "not an address", "", ""); err == nil {
		t.Error("took a malformed address")
	}
	if err := SetupNotifications(s, "me@example.org", "", "example.com"); err == nil {
		t.Error("turned Email Routing on for an apex that gets mail")
	}

	// It's turned on through mail.<panel host>, and the apex keeps no SPF
	// record it didn't have; emails come from the panel's host.
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	if info := Notifications(s); info.From != "alerts@panel.example.com" || info.Email != "me@example.org" || info.Verified {
		t.Errorf("before confirming: %+v", info)
	}
	if !f.routed["mail.panel.example.com"] || len(f.spf("example.com")) != 0 {
		t.Errorf("routed %v, apex SPF %v", f.routed, f.spf("example.com"))
	}
	f.addresses["me@example.org"] = true
	if info := Notifications(s); !info.Verified {
		t.Errorf("after confirming: %+v", info)
	}
	if _, from, _, _ := NotifyChoices(s); len(from) != 1 || !from[0].Selected || !strings.Contains(from[0].Note, "is on") {
		t.Errorf("the panel's host isn't the current one: %+v", from)
	}

	// Another address and name: the address hakobu added goes; the
	// subdomain keeping the zone's Email Routing on stays.
	if err := SetupNotifications(s, "old@example.org", "Ops", ""); err != nil {
		t.Fatal(err)
	}
	if info := Notifications(s); info.From != "ops@panel.example.com" {
		t.Errorf("with a name of its own: %+v", info)
	}
	for _, bad := range []string{"a b", "-x", "x@y", "ünï"} {
		if err := SetupNotifications(s, "old@example.org", bad, ""); err == nil {
			t.Errorf("took %q before the @", bad)
		}
	}
	if _, ok := f.addresses["me@example.org"]; ok || !f.routed["mail.panel.example.com"] {
		t.Errorf("after changing the address: addresses %v, routed %v", f.addresses, f.routed)
	}
	if err := SendTestEmail(s); err != nil || len(f.sent()) != 1 {
		t.Errorf("test email: %v, sent %v", err, f.sent())
	}

	// Off: what hakobu set up goes, what was there before stays.
	if err := TurnOffNotifications(s); err != nil {
		t.Fatal(err)
	}
	if len(f.routed) != 0 || len(f.spf("mail.panel.example.com")) != 0 || !f.addresses["old@example.org"] {
		t.Errorf("after turning off: routed %v, SPF %v, addresses %v", f.routed, f.spf("mail.panel.example.com"), f.addresses)
	}
	if err := SendTestEmail(s); !errors.Is(err, errNotifyOff) {
		t.Errorf("test email with notifications off: %v", err)
	}

	// The zone's Email Routing on already: nothing to set up.
	if err := SetupNotifications(s, "old@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	if info := Notifications(s); info.From != "alerts@panel.example.com" || len(f.routed) != 0 {
		t.Errorf("zone on already: %+v, routed %v", info, f.routed)
	}

	// The domain gets no mail now: its apex is offered, and sending from it
	// turns Email Routing on there, Cloudflare's SPF included; turning the
	// emails off leaves that on.
	f.mx = nil
	if _, from, why, _ := NotifyChoices(s); len(from) != 2 || from[1].Value != "example.com" || len(why) != 0 {
		t.Errorf("choices without MX: from %+v, why %v", from, why)
	}
	if err := SetupNotifications(s, "old@example.org", "", "example.com"); err != nil {
		t.Fatal(err)
	}
	if info := Notifications(s); info.From != "alerts@example.com" || !f.apexReady || len(f.spf("example.com")) != 1 {
		t.Errorf("apex sender: %+v, ready %v, SPF %v", info, f.apexReady, f.spf("example.com"))
	}
	if err := TurnOffNotifications(s); err != nil || !f.apexReady {
		t.Errorf("turning off took the apex's Email Routing: %v, ready %v", err, f.apexReady)
	}
	if err := SetupNotifications(s, "old@example.org", "", "other.dev"); err == nil {
		t.Error("sent from a domain outside the account")
	}
}

func TestProblemsAreMailedOnce(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	boom := errors.New("boom")

	NoteBackup(s, "shop", boom) // notifications are off: nothing
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	f.addresses["me@example.org"] = true
	NoteBackup(s, "shop", boom)
	NoteBackup(s, "shop", boom) // still failing: quiet
	NoteBackup(s, "panel", boom)
	NoteBackup(s, "shop", nil)
	NoteBackup(s, "shop", nil)           // nothing open any more
	noteDeploy(s, "web", "manual", boom) // the owner is watching
	noteDeploy(s, "web", "push", boom)
	noteDeploy(s, "web", "manual", nil)

	want := []string{
		"Backup of database shop failed",
		"Backup of the panel failed",
		"Backups of database shop work again",
		"web: deploy failed",
		"web: deploys work again",
	}
	if got := f.sent(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sent\n  %q\nwant\n  %q", got, want)
	}
}
