package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/detect"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestTemplatesRender(t *testing.T) {
	project := &store.Project{ID: 1, Name: "demo", SharedEnv: "A=1"}
	app := appView{App: store.App{Name: "web", ProjectName: "demo", Repo: "o/r", Port: 8081, ContainerPort: 8080, Env: "B=2", LinkedDB: "main", MemoryMB: 512, Cpus: 0.5}, State: deploy.State{Status: "running", Restarts: 2, OOMKilled: true}}
	worker := &store.Worker{AppName: "web", Name: "worker", Command: "run", Env: "C=3"}
	db := &store.Database{Name: "main", User: "main_user"}
	storages := []store.Storage{{Name: "files", Provider: "rustfs", Bucket: "hakobu-files"}}

	cases := map[string]any{
		"login":   map[string]any{"Connected": true, "Error": "nope"},
		"setup":   map[string]any{"PublicHost": "p.example.com", "Manifest": `{"a":1}`, "State": "s"},
		"home":    map[string]any{"Projects": []store.Project{*project}},
		"project": map[string]any{"Project": project, "Apps": []appView{app}, "Databases": []store.Database{*db}, "Storages": storages},
		"new-app": map[string]any{"Project": "demo", "Repos": []string{"o/r"}, "InstallURL": "https://x", "Zones": []string{"example.com", "other.dev"}, "DefaultZone": "example.com"},
		"presets": map[string]any{"Presets": []detect.Preset{{Strategy: "dockerfile", Path: ".", Stack: "Python · FastAPI", Port: 8000}, {Strategy: "railpack", Path: "web", Stack: "Node.js"}}},
		"app":     map[string]any{"App": app, "Zones": []string{"example.com"}, "Sub": "web", "Zone": "example.com", "Project": project, "Databases": []store.Database{*db}, "Storages": storages, "Effective": splitEnv([]string{"A=1"}), "Worker": worker, "WorkerStatus": "running", "Volumes": []store.Volume{{AppName: "web", Name: "data", MountPath: "/app/data"}}, "LastOOM": "2026-09-29T10:00:00Z", "DataRollbackBlocker": "no snapshot"},
		"deploys": map[string]any{"App": "web", "Running": true, "Logs": []store.DeployLog{{Status: "running", Output: "x"}}},
		"output":  "log line",
		"errors":  []store.TelemetryEvent{{Kind: "error", Message: "boom"}},
		"database": map[string]any{"DB": db, "Project": project, "Ready": true, "Env": splitEnv([]string{"A=1"}), "BackupBucket": "hakobu-backups-1", "Backups": []store.Backup{{ObjectKey: "k", SizeBytes: 2048}, {ID: 2, VerifiedAt: "t", Tables: 3}, {ID: 3, VerifiedAt: "t", VerifyError: "boom"}}, "UsedBy": []string{"web"},
			"Keep": 7, "Job": ops.DBJob{Running: "backing up"}},
		"settings": map[string]any{"PublicHost": "p", "Owner": "me", "GitHubSlug": "hakobu-p", "Disk": "1.0 GB of 10.0 GB used (10%)", "DiskLow": true, "LastCleanup": "2026-09-27 12:00: freed 1.0 GB", "BackupBucket": "hakobu-backups-1"},
	}
	for name, data := range cases {
		if err := templates.ExecuteTemplate(io.Discard, name, data); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The other branches: no worker, no apps domain, scan error, backups off.
	for name, data := range map[string]any{
		"database": map[string]any{"DB": db, "Job": ops.DBJob{Last: "x", Failed: true}},
		"settings": map[string]any{"CloudflareConnected": true},
	} {
		if err := templates.ExecuteTemplate(io.Discard, name, data); err != nil {
			t.Errorf("%s without backups: %v", name, err)
		}
	}
	if err := templates.ExecuteTemplate(io.Discard, "app", map[string]any{"App": app, "Project": project}); err != nil {
		t.Errorf("app without worker: %v", err)
	}
	if err := templates.ExecuteTemplate(io.Discard, "new-app", map[string]any{"Project": "demo", "RepoError": "x"}); err != nil {
		t.Errorf("new-app without apps domain: %v", err)
	}
	if err := templates.ExecuteTemplate(io.Discard, "presets", map[string]any{"Error": "boom"}); err != nil {
		t.Errorf("presets error: %v", err)
	}
}

func TestDomainForm(t *testing.T) {
	zones := []string{"example.com", "shop.example.com", "other.dev"}
	for domain, want := range map[string][2]string{
		"web.example.com":      {"web", "example.com"},
		"example.com":          {"", "example.com"},
		"api.shop.example.com": {"api", "shop.example.com"},
		"":                     {"", ""},
	} {
		sub, zone := splitDomain(domain, zones)
		if sub != want[0] || zone != want[1] {
			t.Errorf("splitDomain(%q) = %q, %q", domain, sub, zone)
		}
		r, _ := http.NewRequest("POST", "/", strings.NewReader(url.Values{"sub": {sub}, "zone": {zone}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := formDomain(r); got != domain {
			t.Errorf("formDomain(%q, %q) = %q, want %q", sub, zone, got, domain)
		}
	}
}

// A domain outside the listed zones must stay editable as-is instead of the
// form falling back to "private" and dropping it on save.
func TestDomainFieldKeepsUnknownDomain(t *testing.T) {
	var b strings.Builder
	err := templates.ExecuteTemplate(&b, "domain-field", map[string]any{
		"Zones": []string{"example.com"}, "Sub": "", "Zone": "", "Domain": "shop.other.net",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `name="domain" value="shop.other.net"`) || strings.Contains(b.String(), `name="zone"`) {
		t.Errorf("unexpected field:\n%s", b.String())
	}
}

func TestPanelRejectsOtherOrigins(t *testing.T) {
	mux := http.NewServeMux()
	for _, p := range []string{"POST /settings/access", "POST /webhook/github", "POST /api/{app_id}/envelope/"} {
		mux.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	h := panelHandler(mux)
	for path, want := range map[string]int{"/settings/access": http.StatusForbidden, "/webhook/github": http.StatusOK, "/api/1/envelope/": http.StatusOK} {
		// A page on app.example.com posting to the panel on hakobu.example.com.
		r := httptest.NewRequest("POST", "https://hakobu.example.com"+path, nil)
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("Origin", "https://app.example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s from a sibling subdomain: %d, want %d", path, w.Code, want)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: no X-Frame-Options", path)
		}
	}
	r := httptest.NewRequest("POST", "https://hakobu.example.com/settings/access", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("same-origin POST: %d", w.Code)
	}
}

func TestStaticFiles(t *testing.T) {
	var page strings.Builder
	if err := templates.ExecuteTemplate(&page, "login", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page.String(), "https://") {
		t.Error("the login page loads something from another site")
	}
	h := staticHandler()
	for _, name := range []string{"app.css", "app.js", "htmx.min.js", "alpine.min.js"} {
		u := staticURL(name)
		if !strings.Contains(page.String(), u) {
			t.Errorf("page doesn't link %s", u)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		if w.Code != http.StatusOK || w.Body.Len() == 0 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d, %d bytes, %q", u, w.Code, w.Body.Len(), w.Header().Get("Cache-Control"))
		}
	}
	// The stylesheet's fonts are embedded too.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/static/fonts/inter.woff2", nil))
	if w.Code != http.StatusOK {
		t.Errorf("font: %d", w.Code)
	}
}
