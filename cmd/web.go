package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

//go:embed web.html
var templatesSrc string

// The panel loads nothing from other sites: scripts, styles and fonts are
// in the binary. Rebuild static/app.css after changing classes in web.html.
//
//go:generate bunx --bun tailwindcss@3 -c tailwind.config.js -i styles.css -o static/app.css --minify
//go:embed static
var staticFiles embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"mb":        func(b int64) string { return fmt.Sprintf("%.1f MB", float64(b)/(1<<20)) },
	"list":      func(items ...string) []string { return items },
	"static":    staticURL,
	"publicURL": ops.PublicURL,
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
}).Parse(templatesSrc))

// staticURL links a file in static/ with a hash of its content, so it can
// be cached for good and a new hakobu still loads its new version.
func staticURL(name string) string {
	b, err := staticFiles.ReadFile("static/" + name)
	if err != nil {
		panic(err) // a template names a file that isn't embedded
	}
	sum := sha256.Sum256(b)
	return "/static/" + name + "?v=" + hex.EncodeToString(sum[:6])
}

func staticHandler() http.Handler {
	files := http.FileServerFS(staticFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("v") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400") // fonts, linked from app.css
		}
		files.ServeHTTP(w, r)
	})
}

// Cookies use the __Host- prefix: apps live on sibling subdomains of the
// panel, and the prefix stops them from planting cookies for it.
const (
	sessionCookie    = "__Host-hakobu_session"
	setupCookie      = "__Host-hakobu_setup"
	manifestCookie   = "__Host-hakobu_gh_state"
	oauthStateCookie = "__Host-hakobu_oauth_state"
	sessionTTL       = 30 * 24 * time.Hour
)

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// panelHandler guards the panel against other sites. Apps are served from
// subdomains of the same domain, which SameSite cookies treat as the same
// site, so state-changing requests must also come from the panel's own
// origin. The webhook and the Sentry endpoint are called cross-origin on
// purpose and check their own secrets.
func panelHandler(mux http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.AddInsecureBypassPattern("POST /webhook/github")
	cop.AddInsecureBypassPattern("POST /api/{app_id}/envelope/")
	h := cop.Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		// Only the panel's own files run. Alpine evaluates its x-* attributes
		// with new Function, hence unsafe-eval; the GitHub App manifest is
		// posted to github.com.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self' https://github.com; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

func cookieMatches(r *http.Request, name, want string) bool {
	c, err := r.Cookie(name)
	return err == nil && want != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fail sends a plain-text error; the page shows it as a toast.
func fail(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// done tells htmx to reload the page, or to go to redirect if given.
func done(w http.ResponseWriter, redirect string) {
	if redirect != "" {
		w.Header().Set("HX-Redirect", redirect)
	} else {
		w.Header().Set("HX-Refresh", "true")
	}
}

func registerWebRoutes(mux *http.ServeMux, s *store.Store) {
	registerAuthRoutes(mux, s)
	mux.Handle("GET /static/", staticHandler())

	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// The session's account is checked against the owner on every
			// request, so a change of owner takes effect at once.
			if c, err := r.Cookie(sessionCookie); err == nil {
				if id, err := s.SessionUser(r.Context(), c.Value); err == nil {
					if mayAccess(r.Context(), s, id) {
						h(w, r)
						return
					}
					if err := s.EndSession(r.Context(), c.Value); err != nil {
						fmt.Println("failed to end a session:", err)
					}
				}
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
	}
	handle := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, authed(h)) }

	// action wraps a mutating handler: an error becomes a toast, success reloads the page.
	action := func(pattern string, h func(r *http.Request) (redirect string, err error)) {
		handle(pattern, func(w http.ResponseWriter, r *http.Request) {
			redirect, err := h(r)
			if err != nil {
				fmt.Println(r.Method, r.URL.Path, "failed:", err)
				fail(w, err)
				return
			}
			done(w, redirect)
		})
	}

	// Projects.

	handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		projects, err := s.ListProjects(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		render(w, "home", map[string]any{"Projects": projects})
	})

	action("POST /projects", func(r *http.Request) (string, error) {
		name := strings.TrimSpace(r.FormValue("name"))
		return "/projects/" + name, ops.CreateProject(s, name)
	})

	handle("GET /projects/{p}", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.GetProject(r.Context(), r.PathValue("p"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		apps, _ := s.ListAppsByProject(r.Context(), p.ID)
		var rows []appView
		for _, a := range apps {
			rows = append(rows, newAppView(a))
		}
		dbs, _ := s.ListDatabasesByProject(r.Context(), p.ID)
		storages, _ := s.ListStoragesByProject(r.Context(), p.ID)
		render(w, "project", map[string]any{"Project": p, "Apps": rows, "Databases": dbs, "Storages": storages, "Sealed": ops.SealedKeys(s, "project", p.Name), "SuggestedDB": ops.SuggestDatabaseName(s, p)})
	})

	action("DELETE /projects/{p}", func(r *http.Request) (string, error) {
		return "/", ops.DeleteProject(s, r.PathValue("p"))
	})

	action("POST /projects/{p}/env", func(r *http.Request) (string, error) {
		return "", ops.SetSharedEnv(s, r.PathValue("p"), r.FormValue("env"))
	})

	handle("GET /projects/{p}/new-app", func(w http.ResponseWriter, r *http.Request) {
		repos, err := ops.ListRepos(s)
		data := map[string]any{
			"Project":     r.PathValue("p"),
			"Repos":       repos,
			"Zones":       zoneNames(s),
			"DefaultZone": config.AppsDomain(),
		}
		if err != nil {
			data["RepoError"] = err.Error()
		}
		if app, err := s.GetGitHubApp(r.Context()); err == nil {
			data["InstallURL"] = "https://github.com/apps/" + app.Slug + "/installations/new"
		}
		render(w, "new-app", data)
	})

	// The scan result is swapped into the new-app form, so errors are
	// rendered inline instead of as a toast.
	handle("POST /projects/{p}/apps/scan", func(w http.ResponseWriter, r *http.Request) {
		presets, err := ops.ScanRepoPresets(s, strings.TrimSpace(r.FormValue("repo")))
		data := map[string]any{"Presets": presets}
		if err != nil {
			data["Error"] = err.Error()
		}
		render(w, "presets", data)
	})

	action("POST /projects/{p}/apps", func(r *http.Request) (string, error) {
		path, strategy, ok := strings.Cut(r.FormValue("preset"), "::")
		if !ok {
			return "", fmt.Errorf("pick a repository and how to build it")
		}
		app := store.CreateAppParams{
			Name:          strings.TrimSpace(r.FormValue("name")),
			Repo:          r.FormValue("repo"),
			BuildPath:     path,
			BuildStrategy: strategy,
		}
		return "/apps/" + app.Name, ops.CreateApp(s, r.PathValue("p"), app, formDomain(r))
	})

	action("POST /projects/{p}/databases", func(r *http.Request) (string, error) {
		return "", ops.CreateDatabase(s, r.PathValue("p"), strings.TrimSpace(r.FormValue("name")))
	})

	action("POST /projects/{p}/storages", func(r *http.Request) (string, error) {
		return "", ops.CreateStorage(s, r.PathValue("p"), store.Storage{
			Name:            strings.TrimSpace(r.FormValue("name")),
			Provider:        r.FormValue("provider"),
			AccountID:       strings.TrimSpace(r.FormValue("account_id")),
			Endpoint:        strings.TrimSpace(r.FormValue("endpoint")),
			AccessKeyID:     strings.TrimSpace(r.FormValue("access_key_id")),
			SecretAccessKey: secret.String(strings.TrimSpace(r.FormValue("secret_access_key"))),
			Bucket:          strings.TrimSpace(r.FormValue("bucket")),
			Region:          strings.TrimSpace(r.FormValue("region")),
		})
	})

	action("DELETE /storages/{st}", func(r *http.Request) (string, error) {
		return "", ops.DeleteStorage(s, r.PathValue("st"))
	})

	// Apps.

	getApp := func(w http.ResponseWriter, r *http.Request) (store.App, bool) {
		app, err := s.GetApp(r.Context(), r.PathValue("a"))
		if err != nil {
			http.NotFound(w, r)
		}
		return app, err == nil
	}

	handle("GET /apps/{a}", func(w http.ResponseWriter, r *http.Request) {
		app, ok := getApp(w, r)
		if !ok {
			return
		}
		p, _ := s.GetProject(r.Context(), app.ProjectName)
		dbs, _ := s.ListDatabasesByProject(r.Context(), app.ProjectID)
		storages, _ := s.ListStoragesByProject(r.Context(), app.ProjectID)
		env, err := ops.EffectiveEnv(s, app)
		envErr := ""
		if err != nil {
			envErr = err.Error()
		}
		data := map[string]any{
			"App": newAppView(app), "Project": p, "Databases": dbs, "Storages": storages,
			"Effective": env, "EnvError": envErr, "Zones": zoneNames(s),
			"SealedApp": ops.SealedKeys(s, "app", app.Name), "SealedWorker": ops.SealedKeys(s, "worker", app.Name),
			"SealedProject": ops.SealedKeys(s, "project", app.ProjectName),
		}
		data["Sub"], data["Zone"] = splitDomain(app.Domain, data["Zones"].([]string))
		data["Volumes"], _ = s.ListVolumes(r.Context(), app.Name)
		data["LastOOM"] = ops.LastOOM(s, app.Name)
		data["DataRollbackBlocker"] = ops.DataRollbackBlocker(s, app)
		if w, err := s.GetWorker(r.Context(), app.Name); err == nil {
			data["Worker"] = w
			data["WorkerStatus"], _ = deploy.ContainerStatus(r.Context(), w.ContainerName())
		}
		render(w, "app", data)
	})

	handle("GET /apps/{a}/status", func(w http.ResponseWriter, r *http.Request) {
		if app, ok := getApp(w, r); ok {
			render(w, "app-status", newAppView(app))
		}
	})

	action("POST /apps/{a}/deploy", func(r *http.Request) (string, error) {
		return "", ops.StartDeploy(s, r.PathValue("a"), "manual")
	})

	action("POST /apps/{a}/rollback", func(r *http.Request) (string, error) {
		return "", ops.StartRollback(s, r.PathValue("a"), r.FormValue("data") != "")
	})

	action("DELETE /apps/{a}", func(r *http.Request) (string, error) {
		app, err := s.GetApp(r.Context(), r.PathValue("a"))
		if err != nil {
			return "", err
		}
		return "/projects/" + app.ProjectName, ops.DeleteApp(s, app.Name)
	})

	action("POST /apps/{a}/settings", func(r *http.Request) (string, error) {
		name := r.PathValue("a")
		var port int64
		if v := strings.TrimSpace(r.FormValue("container_port")); v != "" {
			var err error
			if port, err = strconv.ParseInt(v, 10, 64); err != nil || port <= 0 || port > 65535 {
				return "", fmt.Errorf("port must be empty (detect automatically) or between 1 and 65535")
			}
		}
		app, err := s.GetApp(r.Context(), name)
		if err != nil {
			return "", err
		}
		if err := ops.SetAppDomain(s, app, formDomain(r)); err != nil {
			return "", err
		}
		return "", s.SetAppSettings(r.Context(), store.SetAppSettingsParams{
			Name:            name,
			ContainerPort:   port,
			HealthCheckPath: strings.TrimSpace(r.FormValue("health_check_path")),
		})
	})

	action("POST /apps/{a}/build", func(r *http.Request) (string, error) {
		return "", ops.SetAppBuild(s, r.PathValue("a"), r.FormValue("build_path"), r.FormValue("build_strategy"))
	})

	action("POST /apps/{a}/volumes", func(r *http.Request) (string, error) {
		return "", ops.AddVolume(s, r.PathValue("a"), strings.TrimSpace(r.FormValue("name")), r.FormValue("mount_path"))
	})

	action("DELETE /apps/{a}/volumes/{v}", func(r *http.Request) (string, error) {
		return "", ops.RemoveVolume(s, r.PathValue("a"), r.PathValue("v"))
	})

	action("POST /apps/{a}/limits", func(r *http.Request) (string, error) {
		var memory int64
		var cpus float64
		var err error
		if v := strings.TrimSpace(r.FormValue("memory_mb")); v != "" {
			if memory, err = strconv.ParseInt(v, 10, 64); err != nil {
				return "", fmt.Errorf("memory limit must be a whole number of MB")
			}
		}
		if v := strings.TrimSpace(r.FormValue("cpus")); v != "" {
			if cpus, err = strconv.ParseFloat(v, 64); err != nil {
				return "", fmt.Errorf("CPU limit must be a number, e.g. 0.5")
			}
		}
		return "", ops.SetLimits(s, r.PathValue("a"), memory, cpus)
	})

	action("POST /apps/{a}/share-volumes", func(r *http.Request) (string, error) {
		return "", ops.SetShareVolumes(s, r.PathValue("a"), r.FormValue("share") != "")
	})

	action("POST /apps/{a}/links", func(r *http.Request) (string, error) {
		name := r.PathValue("a")
		if err := ops.LinkDatabase(s, name, r.FormValue("database")); err != nil {
			return "", err
		}
		return "", ops.LinkStorage(s, name, r.FormValue("storage"))
	})

	action("POST /apps/{a}/env", func(r *http.Request) (string, error) {
		return "", ops.SetAppEnv(s, r.PathValue("a"), r.FormValue("env"))
	})

	// Sealed variables of a project ("project", its name), app ("app") or
	// worker ("worker", its app's name).
	action("POST /sealed/{scope}/{owner}", func(r *http.Request) (string, error) {
		return "", ops.SealVar(s, r.PathValue("scope"), r.PathValue("owner"), strings.TrimSpace(r.FormValue("key")), r.FormValue("value"))
	})

	action("DELETE /sealed/{scope}/{owner}/{key}", func(r *http.Request) (string, error) {
		return "", ops.RemoveSealedVar(s, r.PathValue("scope"), r.PathValue("owner"), r.PathValue("key"))
	})

	handle("GET /apps/{a}/env/suggest", func(w http.ResponseWriter, r *http.Request) {
		app, ok := getApp(w, r)
		if !ok {
			return
		}
		file, keys := ops.FindEnvExampleKeys(s, app)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"file": file, "keys": keys})
	})

	handle("GET /apps/{a}/deploys", func(w http.ResponseWriter, r *http.Request) {
		logs, err := s.ListDeployLogs(r.Context(), store.ListDeployLogsParams{AppName: r.PathValue("a"), Limit: 20})
		if err != nil {
			fail(w, err)
			return
		}
		running := false
		for _, l := range logs {
			running = running || l.Status == "running"
		}
		render(w, "deploys", map[string]any{"App": r.PathValue("a"), "Logs": logs, "Running": running})
	})

	handle("GET /apps/{a}/output", func(w http.ResponseWriter, r *http.Request) {
		app, ok := getApp(w, r)
		if !ok {
			return
		}
		container := app.ContainerName()
		if r.URL.Query().Get("worker") != "" {
			container = app.Name + "-worker"
		}
		logs, err := deploy.ContainerLogs(r.Context(), container, 300)
		if err != nil {
			fail(w, err)
			return
		}
		render(w, "output", logs)
	})

	handle("GET /apps/{a}/errors", func(w http.ResponseWriter, r *http.Request) {
		events, err := s.ListTelemetryEvents(r.Context(), store.ListTelemetryEventsParams{AppName: r.PathValue("a"), Limit: 50})
		if err != nil {
			fail(w, err)
			return
		}
		render(w, "errors", events)
	})

	action("POST /apps/{a}/worker", func(r *http.Request) (string, error) {
		return "", ops.SaveWorker(s, store.Worker{
			AppName: r.PathValue("a"),
			Name:    strings.TrimSpace(r.FormValue("name")),
			Command: secret.String(strings.TrimSpace(r.FormValue("command"))),
			Env:     secret.String(r.FormValue("env")),
		})
	})

	action("POST /apps/{a}/worker/restart", func(r *http.Request) (string, error) {
		app, err := s.GetApp(r.Context(), r.PathValue("a"))
		if err != nil {
			return "", err
		}
		return "", ops.RestartWorker(s, app)
	})

	action("DELETE /apps/{a}/worker", func(r *http.Request) (string, error) {
		return "", ops.DeleteWorker(s, r.PathValue("a"))
	})

	// Databases.

	handle("GET /databases/{d}", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.GetDatabase(r.Context(), r.PathValue("d"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		project, _ := s.GetProjectByID(r.Context(), d.ProjectID)
		backups, _ := s.ListBackups(r.Context(), store.ListBackupsParams{Database: d.Name, Limit: 30})
		usedBy, _ := s.AppsUsingDatabase(r.Context(), d.Name)
		render(w, "database", map[string]any{
			"DB": d, "Project": project, "Ready": ops.DatabaseReady(), "Env": splitEnv(ops.DatabaseEnv(d)),
			"Backups": backups, "UsedBy": usedBy, "BackupBucket": ops.BackupBucket(s),
			"Job": ops.DatabaseJob(d.Name), "Keep": config.BackupKeep,
		})
	})

	action("DELETE /databases/{d}", func(r *http.Request) (string, error) {
		d, err := s.GetDatabase(r.Context(), r.PathValue("d"))
		if err != nil {
			return "", err
		}
		redirect := "/"
		if p, err := s.GetProjectByID(r.Context(), d.ProjectID); err == nil {
			redirect = "/projects/" + p.Name
		}
		return redirect, ops.DeleteDatabase(s, d.Name)
	})

	action("POST /databases/{d}/backups", func(r *http.Request) (string, error) {
		return "", ops.StartBackup(s, r.PathValue("d"))
	})

	backupAction := func(pattern string, start func(*store.Store, string, int64) error) {
		action(pattern, func(r *http.Request) (string, error) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				return "", fmt.Errorf("no such backup")
			}
			return "", start(s, r.PathValue("d"), id)
		})
	}
	backupAction("POST /databases/{d}/backups/{id}/restore", ops.StartRestore)
	backupAction("POST /databases/{d}/backups/{id}/verify", ops.StartVerify)

	// Settings.

	handle("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		owner, _ := s.Owner(r.Context())
		disk, diskLow := ops.DiskUsage()
		data := map[string]any{
			"PublicHost": config.PublicHost(), "Owner": owner.GitHubLogin,
			"Disk": disk, "DiskLow": diskLow, "LastCleanup": ops.LastCleanup(),
			"BackupBucket": ops.BackupBucket(s), "CloudflareConnected": ops.CloudflareConnected(s),
			"Rotation": ops.LastRotation(),
		}
		if app, err := s.GetGitHubApp(r.Context()); err == nil {
			data["GitHubSlug"] = app.Slug
		}
		render(w, "settings", data)
	})

	action("POST /settings/cleanup", func(r *http.Request) (string, error) {
		return "", ops.Cleanup(s)
	})

	action("POST /settings/backups", func(r *http.Request) (string, error) {
		return "", ops.SetupBackups(s)
	})

	action("POST /settings/rotate-secrets", func(r *http.Request) (string, error) {
		return "", ops.StartRotation(s)
	})
}

// registerAuthRoutes wires first-run setup and GitHub sign-in. The first
// person to sign in with the installer's setup link becomes the owner;
// after that only the owner can sign in.
func registerAuthRoutes(mux *http.ServeMux, s *store.Store) {
	setupAllowed := func(r *http.Request) bool {
		return cookieMatches(r, setupCookie, config.SetupToken())
	}

	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if owner, _ := s.Owner(r.Context()); owner.GitHubID != 0 {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if token := r.URL.Query().Get("token"); token != "" {
			want := config.SetupToken()
			if want == "" || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
				http.Error(w, "invalid setup link", http.StatusForbidden)
				return
			}
			setCookie(w, setupCookie, token, 3600)
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if !setupAllowed(r) {
			http.Error(w, "open the setup link printed by the installer (journalctl -u hakobu | grep setup)", http.StatusForbidden)
			return
		}
		if _, err := s.GetGitHubApp(r.Context()); err == nil {
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		data := map[string]any{"PublicHost": config.PublicHost()}
		if host := config.PublicHost(); host != "" {
			manifest, err := github.BuildManifest(host)
			if err != nil {
				fail(w, err)
				return
			}
			state, _ := ops.RandomHex(16)
			setCookie(w, manifestCookie, state, 600)
			data["Manifest"] = string(manifest)
			data["State"] = state
		}
		render(w, "setup", data)
	})

	mux.HandleFunc("GET /github-app/callback", func(w http.ResponseWriter, r *http.Request) {
		if !cookieMatches(r, manifestCookie, r.URL.Query().Get("state")) || !setupAllowed(r) {
			http.Error(w, "invalid or expired setup session, open the setup link again", http.StatusBadRequest)
			return
		}
		setCookie(w, manifestCookie, "", -1)
		mc, err := github.ConvertManifestCode(r.URL.Query().Get("code"))
		if err != nil {
			fail(w, err)
			return
		}
		if err := s.SaveGitHubApp(r.Context(), store.SaveGitHubAppParams{
			AppID: mc.ID, Slug: mc.Slug, PrivateKey: secret.String(mc.PEM), WebhookSecret: secret.String(mc.WebhookSecret),
			ClientID: mc.ClientID, ClientSecret: secret.String(mc.ClientSecret),
		}); err != nil {
			fail(w, err)
			return
		}
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		_, err := s.GetGitHubApp(r.Context())
		render(w, "login", map[string]any{"Connected": err == nil, "Error": r.URL.Query().Get("error")})
	})

	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetGitHubApp(r.Context())
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		state, _ := ops.RandomHex(16)
		setCookie(w, oauthStateCookie, state, 600)
		http.Redirect(w, r, github.AuthorizeURL(app.ClientID, "https://"+config.PublicHost()+"/auth/callback", state), http.StatusSeeOther)
	})

	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		deny := func(msg string) {
			http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusSeeOther)
		}
		if !cookieMatches(r, oauthStateCookie, r.URL.Query().Get("state")) {
			deny("sign-in expired, try again")
			return
		}
		setCookie(w, oauthStateCookie, "", -1)
		app, err := s.GetGitHubApp(r.Context())
		if err != nil {
			deny("GitHub is not connected yet")
			return
		}
		user, err := github.SignIn(app.ClientID, string(app.ClientSecret), r.URL.Query().Get("code"), "https://"+config.PublicHost()+"/auth/callback")
		if err != nil {
			fail(w, err)
			return
		}

		owner, err := s.Owner(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if owner.GitHubID == 0 {
			if !setupAllowed(r) {
				deny("the panel has no owner yet, sign in through the installer's setup link")
				return
			}
			if err := s.SetOwner(r.Context(), store.SetOwnerParams{GitHubID: user.ID, GitHubLogin: user.Login}); err != nil {
				fail(w, err)
				return
			}
			config.ClearSetupToken()
			setCookie(w, setupCookie, "", -1)
		} else if owner.GitHubID == user.ID && owner.GitHubLogin != user.Login {
			// The owner renamed their account; the panel shows the new name.
			if err := s.SetOwnerLogin(r.Context(), user.Login); err != nil {
				fmt.Println("failed to update the owner's login:", err)
			}
		}
		if !mayAccess(r.Context(), s, user.ID) {
			deny("GitHub account " + user.Login + " has no access to this panel")
			return
		}

		id, err := ops.RandomHex(32)
		if err == nil {
			err = s.NewSession(r.Context(), id, user.ID, sessionTTL)
		}
		if err != nil {
			fail(w, err)
			return
		}
		setCookie(w, sessionCookie, id, int(sessionTTL.Seconds()))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		// Signing out must end the session on the server too: a copied
		// cookie would otherwise keep working.
		if c, err := r.Cookie(sessionCookie); err == nil {
			if err := s.EndSession(r.Context(), c.Value); err != nil {
				http.Error(w, "couldn't sign out: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		setCookie(w, sessionCookie, "", -1)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

type appView struct {
	store.App
	deploy.State
	Deploying bool
}

func newAppView(a store.App) appView {
	return appView{App: a, State: deploy.ContainerState(context.Background(), a.ContainerName()), Deploying: ops.IsDeploying(a.Name)}
}

type envVar struct{ Key, Value string }

func splitEnv(env []string) []envVar {
	out := make([]envVar, 0, len(env))
	for _, line := range env {
		k, v, _ := strings.Cut(line, "=")
		out = append(out, envVar{k, v})
	}
	return out
}

// zoneNames lists the connected Cloudflare account's domains, nil if none.
func zoneNames(s *store.Store) []string {
	zones, err := ops.Zones(s)
	if err != nil {
		return nil
	}
	names := make([]string, len(zones))
	for i, z := range zones {
		names[i] = z.Name
	}
	return names
}

// formDomain builds the domain from the "sub" and "zone" fields (an empty
// zone keeps the app private), or takes a plain "domain" field.
func formDomain(r *http.Request) string {
	r.ParseForm()
	if !r.Form.Has("zone") {
		return r.FormValue("domain")
	}
	zone := r.FormValue("zone")
	sub := strings.Trim(strings.TrimSpace(r.FormValue("sub")), ".")
	if zone == "" || sub == "" {
		return zone
	}
	return sub + "." + zone
}

// splitDomain is formDomain's inverse for the app settings form.
func splitDomain(domain string, zones []string) (sub, zone string) {
	for _, z := range zones {
		if domain == z {
			return "", z
		}
		if strings.HasSuffix(domain, "."+z) && len(z) > len(zone) {
			sub, zone = strings.TrimSuffix(domain, "."+z), z
		}
	}
	return sub, zone
}

// mayAccess reports whether a GitHub user ID is the owner's.
func mayAccess(ctx context.Context, s *store.Store, githubID int64) bool {
	owner, err := s.Owner(ctx)
	return err == nil && owner.GitHubID != 0 && githubID == owner.GitHubID
}
