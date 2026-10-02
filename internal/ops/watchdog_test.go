package ops

import (
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/cloudflare"
)

func binding(w workerUpload, name string) map[string]string {
	for _, b := range w.bindings {
		if b["name"] == name {
			return b
		}
	}
	return nil
}

// The watchdog follows the notifications on its own: deployed with them,
// deployed again when their address changes or it's gone from Cloudflare,
// removed with them; the owner can keep it off.
func TestWatchdog(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	if err := EnableWatchdog(s); err == nil {
		t.Error("turned on with nobody to mail")
	}
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	name := watchdogName()
	w, ok := f.workers[name]
	if !ok || !Watchdog(s).On {
		t.Fatalf("no Worker %s after turning emails on: %v", name, f.workers)
	}
	if !strings.Contains(w.module, "async scheduled(") || len(w.crons) != 1 || w.crons[0] != "* * * * *" {
		t.Errorf("Worker with crons %v", w.crons)
	}
	if b := binding(w, "EMAIL"); b["type"] != "send_email" || b["destination_address"] != "me@example.org" {
		t.Errorf("EMAIL binding %v", b)
	}
	if b := binding(w, "STATE"); b["type"] != "kv_namespace" || f.kv[b["namespace_id"]] != name {
		t.Errorf("STATE binding %v, namespaces %v", b, f.kv)
	}
	if b := binding(w, "PANEL"); b["text"] != "https://panel.example.com" {
		t.Errorf("PANEL %v", b)
	}

	if err := SetupNotifications(s, "other@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	if b := binding(f.workers[name], "TO"); b["text"] != "other@example.org" || len(f.kv) != 1 {
		t.Errorf("after the address changed, TO is %v, namespaces %v", b, f.kv)
	}

	delete(f.workers, name) // removed in the dashboard
	CheckForOwner(s)
	if _, ok := f.workers[name]; !ok {
		t.Error("not put back by the hourly check")
	}

	if err := TurnWatchdogOff(s); err != nil {
		t.Fatal(err)
	}
	CheckForOwner(s)
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.workers) != 0 || !Watchdog(s).TurnedOff {
		t.Errorf("deployed again after the owner turned it off: %v", f.workers)
	}
	if err := TurnWatchdogOn(s); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.workers[name]; !ok || Watchdog(s).TurnedOff {
		t.Error("not deployed when turned on again")
	}

	if err := TurnOffNotifications(s); err != nil {
		t.Fatal(err)
	}
	if len(f.workers) != 0 || len(f.kv) != 0 || Watchdog(s).On {
		t.Errorf("left after turning emails off: workers %v, namespaces %v", f.workers, f.kv)
	}
}

// A token without the Workers permissions leaves the watchdog out and says
// so; a new token from the panel brings it.
func TestWatchdogWaitsForToken(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	f.noWorkers = true
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.workers) != 0 || Watchdog(s).On || Watchdog(s).Err != "" {
		t.Errorf("watchdog %+v without the permissions", Watchdog(s))
	}
	var lacking []string
	for _, p := range cloudflare.Lacking(TokenPermissions(s, false)) {
		lacking = append(lacking, p.Name)
	}
	if strings.Join(lacking, ",") != "Workers Scripts Edit,Workers KV Storage Edit" {
		t.Errorf("lacking %v", lacking)
	}

	f.noWorkers = false
	if err := ReplaceCloudflareToken(s, "tok2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.workers[watchdogName()]; !ok || len(cloudflare.Lacking(CachedTokenPermissions())) != 0 {
		t.Errorf("after the new token: workers %v, lacking %v", f.workers, cloudflare.Lacking(CachedTokenPermissions()))
	}
}
