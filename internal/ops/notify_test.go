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
// to the owner goes.
type fakeEmail struct {
	mu       sync.Mutex
	apexOn   bool
	routed   []string // names Email Routing was turned on for
	verified bool
	added    []string
	subjects []string
}

func newFakeEmail(t *testing.T) *fakeEmail {
	f := &fakeEmail{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		switch r.Method + " " + r.URL.Path {
		case "GET /zones":
			fmt.Fprint(w, `{"success":true,"result":[{"id":"z1","name":"example.com","account":{"id":"acc"}}]}`)
		case "GET /zones/z1/email/routing":
			fmt.Fprintf(w, `{"success":true,"result":{"enabled":%v}}`, f.apexOn)
		case "POST /zones/z1/email/routing/dns":
			f.routed = append(f.routed, body["name"])
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		case "GET /accounts/acc/email/routing/addresses":
			var list []map[string]any
			for _, a := range f.added {
				v := any(nil)
				if f.verified {
					v = "2026-10-02T00:00:00Z"
				}
				list = append(list, map[string]any{"id": a, "email": a, "verified": v})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": list})
		case "POST /accounts/acc/email/routing/addresses":
			f.added = append(f.added, body["email"])
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		case "POST /accounts/acc/email/sending/send":
			var m struct{ Subject string }
			_ = json.Unmarshal(b, &m)
			f.subjects = append(f.subjects, m.Subject)
			fmt.Fprint(w, `{"success":true,"result":{}}`)
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

	if err := SetupNotifications(s, "not an address", ""); err == nil {
		t.Error("took a malformed address")
	}
	// Email Routing is off for example.com: the panel's own subdomain gets
	// it, never the apex.
	if err := SetupNotifications(s, "me@example.org", ""); err != nil {
		t.Fatal(err)
	}
	if info := Notifications(s); info.From != "hakobu@mail.panel.example.com" || info.Email != "me@example.org" || info.Verified {
		t.Errorf("before confirming: %+v", info)
	}
	if len(f.routed) != 1 || f.routed[0] != "mail.panel.example.com" || len(f.added) != 1 {
		t.Errorf("routing turned on for %v, addresses added %v", f.routed, f.added)
	}
	if err := SetupNotifications(s, "me@example.org", "example.com"); err == nil {
		t.Error("turned Email Routing on for the apex")
	}
	f.verified = true
	if info := Notifications(s); !info.Verified {
		t.Errorf("after confirming: %+v", info)
	}

	// With Email Routing already on for the apex, that's the sender.
	f.apexOn = true
	if err := SetupNotifications(s, "me@example.org", ""); err != nil {
		t.Fatal(err)
	}
	if info := Notifications(s); info.From != "hakobu@example.com" || len(f.routed) != 1 {
		t.Errorf("apex sender: %+v, routing turned on for %v", info, f.routed)
	}
	if err := SetupNotifications(s, "me@example.org", "other.dev"); err == nil {
		t.Error("sent from a domain outside the account")
	}

	if err := SendTestEmail(s); err != nil || len(f.sent()) != 1 {
		t.Errorf("test email: %v, sent %v", err, f.sent())
	}
	if err := TurnOffNotifications(s); err != nil {
		t.Fatal(err)
	}
	if err := SendTestEmail(s); !errors.Is(err, errNotifyOff) {
		t.Errorf("test email with notifications off: %v", err)
	}
}

func TestProblemsAreMailedOnce(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	boom := errors.New("boom")

	NoteBackup(s, "shop", boom) // notifications are off: nothing
	if err := SetupNotifications(s, "me@example.org", ""); err != nil {
		t.Fatal(err)
	}
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
