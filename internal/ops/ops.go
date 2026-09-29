// Package ops holds hakobu's operations on projects, apps, databases and
// storages, shared by the dashboard, the webhook and the agent loops.
package ops

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/detect"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Names end up in container names, URLs, bucket names and Postgres
// identifiers, so they are restricted to what all of those accept.
var (
	validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	// Database names are always double-quoted in SQL, where dashes are fine.
	validDBName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
)

func checkName(kind, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid %s name %q: use lowercase letters, digits and dashes, starting with a letter", kind, name)
	}
	return nil
}

// Projects.

func CreateProject(s *store.Store, name string) error {
	if err := checkName("project", name); err != nil {
		return err
	}
	return s.CreateProject(ctx(), name)
}

// DeleteProject removes every app, database and storage in the project.
func DeleteProject(s *store.Store, name string) error {
	p, err := s.GetProject(ctx(), name)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", name, err)
	}
	apps, err := s.ListAppsByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if err := DeleteApp(s, a.Name); err != nil {
			return err
		}
	}
	dbs, err := s.ListDatabasesByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if err := DeleteDatabase(s, d.Name); err != nil {
			return err
		}
	}
	storages, err := s.ListStoragesByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	for _, st := range storages {
		if err := DeleteStorage(s, st.Name); err != nil {
			return err
		}
	}
	return s.DeleteProject(ctx(), name)
}

// Apps.

// CreateApp stores the app, gives it its domain ("" keeps it private) and
// kicks off its first build and deploy.
func CreateApp(s *store.Store, projectName string, app store.CreateAppParams, domain string) error {
	if err := checkName("app", app.Name); err != nil {
		return err
	}
	p, err := s.GetProject(ctx(), projectName)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if err := CheckDomain(s, app.Name, domain); err != nil {
		return err
	}
	if app.BuildStrategy == "" {
		app.BuildStrategy = "railpack"
	}
	app.ProjectID = p.ID
	if err := s.CreateApp(ctx(), app); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("an app named %q already exists", app.Name)
		}
		return err
	}
	created, err := s.GetApp(ctx(), app.Name)
	if err != nil {
		return err
	}
	if err := SetAppDomain(s, created, domain); err != nil {
		s.DeleteAppCascade(ctx(), app.Name)
		return err
	}
	return StartDeploy(s, app.Name, "create")
}

// CheckDomain rejects a domain that the panel or another app already uses.
func CheckDomain(s *store.Store, appName, domain string) error {
	if domain == "" {
		return nil
	}
	if strings.EqualFold(domain, config.PublicHost()) {
		return fmt.Errorf("%s is the panel's own address", domain)
	}
	if other, err := s.GetAppByDomain(ctx(), domain); err == nil && other.Name != appName {
		return fmt.Errorf("%s is already used by %s", domain, other.Name)
	}
	return nil
}

func DeleteApp(s *store.Store, name string) error {
	// Hold the app like a deploy so no deploy recreates its container or
	// proxy while it's being removed.
	if _, err := reserve(name, false); err != nil {
		return err
	}
	defer release(name)
	app, err := s.GetApp(ctx(), name)
	if err != nil {
		return fmt.Errorf("app %q not found: %w", name, err)
	}
	vols, err := s.ListVolumes(ctx(), name)
	if err != nil {
		return err
	}
	if err := removeAppDNS(s, app); err != nil {
		fmt.Println("failed to delete the DNS record of", app.Domain+":", err)
	}
	if err := s.DeleteAppCascade(ctx(), name); err != nil {
		return err
	}
	RemoveProxy(app.Name)
	for _, c := range []string{app.Name + "-blue", app.Name + "-green", app.Name + "-worker"} {
		if err := deploy.RemoveContainer(ctx(), c); err != nil {
			return err
		}
	}
	removeAppImages(app.Name)
	os.Remove(snapshotPath(app.Name))
	for _, v := range vols {
		if err := deploy.RemoveVolume(ctx(), dockerVolume(name, v.Name)); err != nil {
			return err
		}
	}
	if err := SyncTunnel(s); err != nil {
		fmt.Println("tunnel routes not updated (retrying):", err)
	}
	return nil
}

// LinkDatabase links (or with dbName "" unlinks) one of the project's databases.
func LinkDatabase(s *store.Store, appName, dbName string) error {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		return err
	}
	if dbName != "" {
		d, err := s.GetDatabase(ctx(), dbName)
		if err != nil || d.ProjectID != app.ProjectID {
			return fmt.Errorf("database %q not found in project %s", dbName, app.ProjectName)
		}
	}
	return s.SetAppLinkedDB(ctx(), store.SetAppLinkedDBParams{Name: appName, LinkedDB: dbName})
}

// LinkStorage links (or with storageName "" unlinks) one of the project's storages.
func LinkStorage(s *store.Store, appName, storageName string) error {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		return err
	}
	if storageName != "" {
		st, err := s.GetStorage(ctx(), storageName)
		if err != nil || st.ProjectID != app.ProjectID {
			return fmt.Errorf("storage %q not found in project %s", storageName, app.ProjectName)
		}
	}
	return s.SetAppLinkedStorage(ctx(), store.SetAppLinkedStorageParams{Name: appName, LinkedStorage: storageName})
}

// Environment. Later entries win on duplicate keys, so the order is:
// PORT, linked database, linked storage, SENTRY_DSN, project shared
// variables, then the service's own variables.

// baseEnv starts with PORT unless port is 0 (workers).
func baseEnv(s *store.Store, app store.App, port int64) ([]string, error) {
	var env []string
	if port > 0 {
		env = append(env, fmt.Sprintf("PORT=%d", port))
	}
	if app.LinkedDB != "" {
		d, err := s.GetDatabase(ctx(), app.LinkedDB)
		if err != nil {
			return nil, fmt.Errorf("linked database %q not found: %w", app.LinkedDB, err)
		}
		env = append(env, DatabaseEnv(d)...)
	}
	if app.LinkedStorage != "" {
		st, err := s.GetStorage(ctx(), app.LinkedStorage)
		if err != nil {
			return nil, fmt.Errorf("linked storage %q not found: %w", app.LinkedStorage, err)
		}
		env = append(env, storageEnv(st)...)
	}
	if host := config.PublicHost(); host != "" {
		key := string(app.SentryKey)
		if key == "" {
			var err error
			if key, err = RandomHex(16); err != nil {
				return nil, err
			}
			if err := s.SetAppSentryKey(ctx(), store.SetAppSentryKeyParams{Name: app.Name, SentryKey: secret.String(key)}); err != nil {
				return nil, err
			}
		}
		env = append(env, fmt.Sprintf("SENTRY_DSN=https://%s@%s/%d", key, host, app.ID))
	}
	p, err := s.GetProject(ctx(), app.ProjectName)
	if err != nil {
		return nil, err
	}
	return append(env, ParseEnv(string(p.SharedEnv))...), nil
}

// AppEnv is the environment of the app's next deploy.
func AppEnv(s *store.Store, app store.App) ([]string, error) {
	return appEnv(s, app, portHint(app, 0))
}

func appEnv(s *store.Store, app store.App, port int64) ([]string, error) {
	env, err := baseEnv(s, app, port)
	if err != nil {
		return nil, err
	}
	return append(env, ParseEnv(string(app.Env))...), nil
}

func WorkerEnv(s *store.Store, app store.App, w store.Worker) ([]string, error) {
	env, err := baseEnv(s, app, 0)
	if err != nil {
		return nil, err
	}
	return append(env, ParseEnv(string(w.Env))...), nil
}

// ParseEnv turns "KEY=value" lines into a docker env slice, skipping blank
// lines and comments.
func ParseEnv(text string) []string {
	var env []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") || strings.Index(line, "=") <= 0 {
			continue
		}
		env = append(env, line)
	}
	return env
}

// GitHub.

func gitHubApp(s *store.Store) (store.GitHubApp, error) {
	app, err := s.GetGitHubApp(ctx())
	if err != nil {
		return app, fmt.Errorf("no GitHub App connected")
	}
	return app, nil
}

func repoToken(s *store.Store, repo string) (string, error) {
	app, err := gitHubApp(s)
	if err != nil {
		return "", err
	}
	return github.RepoToken(app.AppID, string(app.PrivateKey), repo)
}

func ListRepos(s *store.Store) ([]string, error) {
	app, err := gitHubApp(s)
	if err != nil {
		return nil, err
	}
	repos, err := github.ListRepos(app.AppID, string(app.PrivateKey))
	if err != nil {
		return nil, err
	}
	sort.Strings(repos)
	return repos, nil
}

// ScanRepoPresets detects buildable directories through the GitHub API,
// without cloning.
func ScanRepoPresets(s *store.Store, repo string) ([]detect.Preset, error) {
	token, err := repoToken(s, repo)
	if err != nil {
		return nil, err
	}
	files, err := github.RepoFiles(repo, token)
	if err != nil {
		return nil, err
	}
	presets := detect.Scan(files, func(path string) string {
		content, _ := github.FileContent(repo, path, token)
		return content
	})
	if len(presets) == 0 {
		return nil, fmt.Errorf("no Dockerfile or supported project files found in %s", repo)
	}
	return presets, nil
}

var envExampleFiles = []string{".env.example", ".env.sample", ".env.dist", ".env.template", "env.example"}

// FindEnvExampleKeys returns the variable names from the first
// .env.example-style file in the app's build path or the repo root.
func FindEnvExampleKeys(s *store.Store, app store.App) (file string, keys []string) {
	if app.Repo == "" {
		return "", nil
	}
	token, err := repoToken(s, app.Repo)
	if err != nil {
		return "", nil
	}
	dirs := []string{""}
	if p := strings.Trim(app.BuildPath, "/."); p != "" {
		dirs = []string{p + "/", ""}
	}
	for _, dir := range dirs {
		for _, name := range envExampleFiles {
			content, ok := github.FileContent(app.Repo, dir+name, token)
			if !ok {
				continue
			}
			for _, line := range ParseEnv(content) {
				key, _, _ := strings.Cut(line, "=")
				keys = append(keys, strings.TrimSpace(key))
			}
			return dir + name, keys
		}
	}
	return "", nil
}
