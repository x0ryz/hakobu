package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/detect"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/update"
)

func TestTemplatesRender(t *testing.T) {
	project := &store.Project{ID: 1, Name: "demo", SharedEnv: "A=1"}
	app := appView{App: store.App{Name: "web", ProjectName: "demo", Repo: "o/r", Port: 8081, ContainerPort: 8080, Env: "B=2", LinkedDB: "main", MemoryMB: 512, Cpus: 0.5}, State: deploy.State{Status: "running", Restarts: 2, OOMKilled: true}}
	worker := &store.Worker{AppName: "web", Name: "worker", Command: "run", Env: "C=3"}
	db := &store.Database{Name: "main", User: "main_user"}
	storages := []store.Storage{{Name: "files", Provider: "r2", Bucket: "hakobu-files-1", AccountID: "acc"}, {Name: "media", Provider: "s3", Bucket: "m", AccessKeyID: "k"}}

	cases := map[string]any{
		"login":   map[string]any{"Connected": true, "Error": "nope"},
		"setup":   map[string]any{"PublicHost": "p.example.com", "Manifest": `{"a":1}`, "State": "s"},
		"home":    map[string]any{"Projects": []store.Project{*project}},
		"project": map[string]any{"Project": project, "Apps": []appView{app}, "Databases": []store.Database{*db}, "Storages": storages, "Sealed": []string{"P"}},
		"new-app": map[string]any{"Project": "demo", "Repos": []string{"o/r"}, "InstallURL": "https://x", "Zones": []string{"example.com", "other.dev"}, "DefaultZone": "example.com"},
		"presets": map[string]any{"Presets": []detect.Preset{{Strategy: "dockerfile", Path: ".", Stack: "Python · FastAPI", Port: 8000}, {Strategy: "railpack", Path: "web", Stack: "Node.js"}}},
		"app": map[string]any{"App": app, "Zones": []string{"example.com"}, "Sub": "web", "Zone": "example.com", "Project": project, "Databases": []store.Database{*db}, "Storages": storages, "Effective": []ops.EnvVar{{Key: "A", Value: "1"}, {Key: "S", Sealed: true}}, "SealedApp": []string{"S"}, "SealedWorker": []string{"W"}, "SealedProject": []string{"P"}, "Worker": worker, "WorkerStatus": "running", "Volumes": []store.Volume{{AppName: "web", Name: "data", MountPath: "/app/data"}}, "LastOOM": "2026-09-29T10:00:00Z", "DataRollbackBlocker": "no snapshot",
			"BackupBucket": "hakobu-backups-1", "VolumeJobRunning": true, "VolumeBackups": []volumeBackups{
				{Volume: "data", Job: ops.DBJob{Running: "backing up"}, Backups: []store.VolumeBackup{{ID: 1, ObjectKey: "k", SizeBytes: 10}, {ID: 2, VerifiedAt: "t", Files: 3}, {ID: 3, VerifiedAt: "t", VerifyError: "boom"}}},
				{Volume: "cache", Job: ops.DBJob{Last: "x", Failed: true}},
			}},
		"deploys": map[string]any{"App": "web", "Running": true, "Logs": []store.DeployLog{{Status: "running", Output: "x"}}},
		"output":  "log line",
		"errors":  []store.TelemetryEvent{{Kind: "error", Message: "boom"}},
		"database": map[string]any{"DB": db, "Project": project, "Ready": true, "Env": splitEnv([]string{"A=1"}), "BackupBucket": "hakobu-backups-1", "Backups": []store.Backup{{ObjectKey: "k", SizeBytes: 2048}, {ID: 2, VerifiedAt: "t", Tables: 3}, {ID: 3, VerifiedAt: "t", VerifyError: "boom"}}, "UsedBy": []string{"web"},
			"Keep": 7, "Job": ops.DBJob{Running: "backing up"}},
		"settings": map[string]any{"PublicHost": "p", "Owner": "me", "GitHubSlug": "hakobu-p", "Disk": "1.0 GB of 10.0 GB used (10%)", "DiskLow": true, "LastCleanup": "2026-09-27 12:00: freed 1.0 GB", "BackupBucket": "hakobu-backups-1",
			"Rotation":            ops.Rotation{Started: "2026-09-30 10:00", Log: "done    x\n", Manual: []string{"GitHub App ..."}, Failures: 1},
			"CloudflareConnected": true, "Notify": ops.NotifyInfo{On: true, Email: "me@example.org", From: "hakobu@mail.p"},
			"OAuthGrants": []store.OAuthGrant{{ID: 1, ClientName: "Claude", Scope: "read deploy", CreatedAt: "t", LastUsedAt: "u"}},
			"Update":      ops.UpdateInfo{Current: "v0.6.0", Latest: "v0.7.0", Available: true, Updater: true, HasLast: true, Last: update.Status{State: "running", To: "v0.7.0", Message: "downloading"}}},
		"oauth-consent": map[string]any{"Client": oauthClient{ID: "https://claude.ai/oauth/claude-code-client-metadata", Name: "Claude Code"}, "Request": authorizeRequest{Query: "a=b"},
			"RedirectHost": "localhost:3118", "Loopback": true, "Document": true, "Deploy": true, "PublicHost": "p"},
		"oauth-error": "boom",
	}
	for name, data := range cases {
		if err := templates.ExecuteTemplate(io.Discard, name, data); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The other branches: no worker, no apps domain, scan error, backups off.
	for name, data := range map[string]any{
		"database": map[string]any{"DB": db, "Job": ops.DBJob{Last: "x", Failed: true}},
		"settings": map[string]any{"CloudflareConnected": true, "Notify": ops.NotifyInfo{On: true, Err: "no token"}, "Update": ops.UpdateInfo{Current: "dev"}},
	} {
		if err := templates.ExecuteTemplate(io.Discard, name, data); err != nil {
			t.Errorf("%s without backups: %v", name, err)
		}
	}
	for _, st := range []string{"updated", "up to date", "failed", "rolled back"} {
		data := map[string]any{"Update": ops.UpdateInfo{Current: "v0.7.0", Latest: "v0.7.0", Updater: true, HasLast: true, Last: update.Status{State: st, From: "v0.6.0", To: "v0.7.0"},
			Previous: "v0.6.0", UpdatedAt: "t", CanRollBack: true, RollbackLosesData: st == "failed", RollbackBlocked: map[bool]string{true: "gone"}[st == "rolled back"]}}
		if err := templates.ExecuteTemplate(io.Discard, "settings", data); err != nil {
			t.Errorf("settings after an update that %s: %v", st, err)
		}
	}
	if err := templates.ExecuteTemplate(io.Discard, "app", map[string]any{"App": app, "Project": project}); err != nil {
		t.Errorf("app without worker: %v", err)
	}
	if err := templates.ExecuteTemplate(io.Discard, "new-app", map[string]any{"Project": "demo", "RepoError": "x"}); err != nil {
		t.Errorf("new-app without apps domain: %v", err)
	}
	for _, data := range []map[string]any{
		{"PublicHost": "p", "Name": "alerts", "Notify": ops.NotifyInfo{On: true}, "To": []ops.NotifyChoice{{Value: "a@b.c", Note: "n", Selected: true}},
			"From": []ops.NotifyChoice{{Value: "mail.p", Note: "n"}}},
		{"PublicHost": "p", "Error": "boom", "OtherValue": "x@y.z", "Why": []string{"example.com: its mail goes to mx.example"}},
	} {
		if err := templates.ExecuteTemplate(io.Discard, "notify-form", data); err != nil {
			t.Errorf("notify-form: %v", err)
		}
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

func TestMasterKeyNeedsFreshSignIn(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := config.PrepareDataDir(); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("hakobu.example.com"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(config.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SetOwner(ctx, store.SetOwnerParams{GitHubID: 42, GitHubLogin: "me"}); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(ctx, "tok", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	get := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://hakobu.example.com/settings/master-key", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		w := httptest.NewRecorder()
		panelHandler(mux).ServeHTTP(w, r)
		return w
	}

	keyFreshFor = -time.Second // signed in too long ago
	t.Cleanup(func() { keyFreshFor = 5 * time.Minute })
	w := get()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/auth/login" || !strings.Contains(w.Header().Get("Set-Cookie"), afterCookie+"=master-key") {
		t.Errorf("stale session: %d %v, body %q", w.Code, w.Header(), w.Body)
	}
	if ops.KeyDownloaded() {
		t.Error("key counted as downloaded")
	}

	keyFreshFor = time.Hour
	w = get()
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(w.Body.String(), "HAKOBU_MASTER_KEY=") {
		t.Errorf("fresh session: %d %v, body %q", w.Code, w.Header(), w.Body)
	}
	if !ops.KeyDownloaded() {
		t.Error("download not noted")
	}
}
