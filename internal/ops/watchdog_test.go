package ops

import (
	"strings"
	"testing"
)

// The watchdog follows the notifications: deployed to mail their address,
// deployed again when it changes, removed with them.
func TestWatchdog(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	if err := EnableWatchdog(s); err == nil {
		t.Error("turned on with nobody to mail")
	}
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := EnableWatchdog(s); err != nil {
		t.Fatal(err)
	}
	name := watchdogName()
	w, ok := f.workers[name]
	if !ok || !Watchdog(s).On {
		t.Fatalf("no Worker %s: %v", name, f.workers)
	}
	if !strings.Contains(w.module, "async scheduled(") || len(w.crons) != 1 || w.crons[0] != "* * * * *" {
		t.Errorf("Worker %q with crons %v", w.module[:min(len(w.module), 40)], w.crons)
	}
	binding := func(w workerUpload, name string) map[string]string {
		for _, b := range w.bindings {
			if b["name"] == name {
				return b
			}
		}
		return nil
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
	if b := binding(f.workers[name], "TO"); b["text"] != "other@example.org" {
		t.Errorf("after the address changed, TO is %v", b)
	}
	if len(f.kv) != 1 {
		t.Errorf("namespaces %v, want the one reused", f.kv)
	}

	if err := TurnOffNotifications(s); err != nil {
		t.Fatal(err)
	}
	if len(f.workers) != 0 || len(f.kv) != 0 || Watchdog(s).On {
		t.Errorf("left after turning emails off: workers %v, namespaces %v", f.workers, f.kv)
	}
}
