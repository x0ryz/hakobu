package ops

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Replacing all secrets, after a suspected break-in: everything hakobu can
// issue again is replaced, the apps restart with the new values, everyone
// is signed out and the master key is rotated. What only a person can
// replace is listed at the end.

type Rotation struct {
	Running  bool
	Started  string
	Log      string
	Manual   []string // what's left to replace by hand
	Failures int
}

var (
	rotationMu sync.Mutex
	rotation   Rotation
)

// LastRotation is the running or last rotation since the agent started.
func LastRotation() Rotation {
	rotationMu.Lock()
	defer rotationMu.Unlock()
	r := rotation
	r.Manual = append([]string(nil), rotation.Manual...)
	return r
}

type rotationLog struct{}

func (rotationLog) Write(p []byte) (int, error) {
	rotationMu.Lock()
	defer rotationMu.Unlock()
	rotation.Log += string(p)
	return len(p), nil
}

// StartRotation replaces all secrets in the background.
func StartRotation(s *store.Store) error {
	rotationMu.Lock()
	defer rotationMu.Unlock()
	if rotation.Running {
		return fmt.Errorf("secrets are being replaced already")
	}
	rotation = Rotation{Running: true, Started: time.Now().Format("2006-01-02 15:04")}
	go func() {
		manual, failures := rotateSecrets(s, rotationLog{})
		rotationMu.Lock()
		rotation.Running, rotation.Manual, rotation.Failures = false, manual, failures
		rotationMu.Unlock()
	}()
	return nil
}

// rotateSecrets does the work; each step reports and failures don't stop
// the others.
func rotateSecrets(s *store.Store, out io.Writer) (manual []string, failures int) {
	step := func(what string, err error) {
		if err != nil {
			failures++
			fmt.Fprintf(out, "FAILED  %s: %v\n", what, err)
			return
		}
		fmt.Fprintf(out, "done    %s\n", what)
	}
	restart := map[string]bool{} // apps to restart with new values

	// GitHub: the webhook secret can be replaced through the API, the
	// private key and client secret only on github.com.
	if app, err := s.GetGitHubApp(ctx()); err == nil {
		newSecret, err := RandomHex(32)
		if err == nil {
			if err = github.SetWebhookSecret(app.AppID, string(app.PrivateKey), newSecret); err == nil {
				err = s.SetGitHubWebhookSecret(ctx(), secret.String(newSecret))
			}
		}
		step("GitHub webhook secret", err)
		manual = append(manual, fmt.Sprintf("GitHub App %s: generate a new private key and client secret at https://github.com/settings/apps/%s (hakobu can't replace them itself)", app.Slug, app.Slug))
	}

	// Cloudflare: a new tunnel secret, which disconnects any other
	// connector. The API token is the owner's to roll.
	if CloudflareConnected(s) {
		if TunnelReady(s) {
			step("tunnel token (other connectors were disconnected)", rotateTunnel(s))
		}
		manual = append(manual, "Cloudflare API token: roll it in the Cloudflare dashboard (Manage Account → API Tokens → Roll) and give hakobu the new one with `cd /opt/hakobu && sudo -u hakobu ./hakobu setup --reconnect`")
	}

	// Databases: new passwords; their apps get them on restart.
	if dbs, err := s.ListDatabases(ctx()); err == nil {
		for _, d := range dbs {
			step("password of database "+d.Name, rotateDatabasePassword(s, d))
			if apps, err := s.AppsUsingDatabase(ctx(), d.Name); err == nil {
				for _, a := range apps {
					restart[a] = true
				}
			}
		}
	}

	// Storages: RustFS keys are hakobu's own; R2/S3 keys are the user's.
	var oldRustFS []store.Storage
	if storages, err := s.ListStorages(ctx()); err == nil {
		for _, st := range storages {
			if st.Provider != "rustfs" {
				manual = append(manual, fmt.Sprintf("storage %s (%s, bucket %s): create a new access key at the provider and add the storage again", st.Name, st.Provider, st.Bucket))
				continue
			}
			err := rotateRustFSKeys(s, st)
			step("keys of storage "+st.Name, err)
			if err == nil {
				oldRustFS = append(oldRustFS, st)
			}
			if apps, err := s.AppsUsingStorage(ctx(), st.Name); err == nil {
				for _, a := range apps {
					restart[a] = true
				}
			}
		}
	}

	// Sentry DSN keys, and the restarts that hand out every new value.
	if apps, err := s.ListApps(ctx()); err == nil {
		for _, a := range apps {
			key, err := RandomHex(16)
			if err == nil {
				err = s.SetAppSentryKey(ctx(), store.SetAppSentryKeyParams{Name: a.Name, SentryKey: secret.String(key)})
			}
			step("Sentry DSN key of "+a.Name, err)
			restart[a.Name] = true
		}
		names := make([]string, 0, len(restart))
		for name := range restart {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			step("restart "+name+" with the new values", restartApp(s, name))
		}
		manual = append(manual, userVariables(s, apps)...)
	}
	// Old RustFS users go once the apps run with the new keys.
	for _, st := range oldRustFS {
		if root, err := rustfsRoot(st.Bucket); err == nil {
			err = root.RemoveBucketUser(st.AccessKeyID)
			step("old keys of storage "+st.Name+" revoked", err)
		}
	}

	step("everyone signed out", s.DeleteAllSessions(ctx()))
	step("master key rotated (copies of the old key and database are worthless)", s.RotateMasterKey())
	if BackupBucket(s) != "" {
		// Older panel backups need the old key; this one is the first a
		// newly downloaded key file restores.
		step("panel backed up with the new key", BackupPanel(s))
	}
	return manual, failures
}

// rotateTunnel gives the tunnel a new token and restarts cloudflared with
// it; the panel and apps are unreachable for a few seconds.
func rotateTunnel(s *store.Store) error {
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	token, err := c.RotateTunnelSecret(cf.AccountID, cf.TunnelID)
	if err != nil {
		return err
	}
	if err := s.SetTunnelToken(ctx(), secret.String(token)); err != nil {
		return err
	}
	return StartTunnel(s)
}

func rotateDatabasePassword(s *store.Store, d store.Database) error {
	password, err := RandomHex(16)
	if err != nil {
		return err
	}
	if err := deploy.PostgresExec(ctx(), PostgresContainer, fmt.Sprintf(`ALTER USER "%s" WITH PASSWORD '%s'`, d.User, password)); err != nil {
		return err
	}
	return s.SetDatabasePassword(ctx(), store.SetDatabasePasswordParams{Name: d.Name, Password: secret.String(password)})
}

// rotateRustFSKeys gives a RustFS storage a new user; the old one is
// removed by the caller once the apps have the new keys.
func rotateRustFSKeys(s *store.Store, st store.Storage) error {
	root, err := rustfsRoot(st.Bucket)
	if err != nil {
		return err
	}
	access, err := RandomHex(9)
	if err != nil {
		return err
	}
	access = "hk" + access
	key, err := RandomHex(20)
	if err != nil {
		return err
	}
	if err := root.AddBucketUser(access, key); err != nil {
		return err
	}
	return s.SetStorageKeys(ctx(), store.SetStorageKeysParams{Name: st.Name, AccessKeyID: access, SecretAccessKey: secret.String(key)})
}

// restartApp redeploys the app's current image (and its worker) so they
// get the current variables, without a build.
func restartApp(s *store.Store, name string) error {
	if _, err := reserve(name, false); err != nil {
		return err
	}
	defer release(name)
	app, err := s.GetApp(ctx(), name)
	if err != nil {
		return err
	}
	if ok, _ := deploy.ImageExists(ctx(), ImageTag(app)); !ok {
		return nil // never deployed: the first deploy gets the new values
	}
	var out strings.Builder
	if err := rollOut(s, app, ImageTag(app), &out); err != nil {
		return fmt.Errorf("%w\n%s", err, out.String())
	}
	if w, err := s.GetWorker(ctx(), name); err == nil {
		return runWorker(s, app, w, &out)
	}
	return nil
}

// userVariables lists the variables the user set, which only they can
// replace at wherever those keys come from.
func userVariables(s *store.Store, apps []store.App) []string {
	var manual []string
	projects := map[string]bool{}
	for _, a := range apps {
		keys := sortedKeys(envKeys(string(a.Env)))
		keys = append(keys, SealedKeys(s, "app", a.Name)...)
		if w, err := s.GetWorker(ctx(), a.Name); err == nil {
			keys = append(keys, sortedKeys(envKeys(string(w.Env)))...)
			keys = append(keys, SealedKeys(s, "worker", a.Name)...)
		}
		if len(keys) > 0 {
			manual = append(manual, fmt.Sprintf("app %s: replace at their source any secrets among %s, then update them here", a.Name, strings.Join(keys, ", ")))
		}
		projects[a.ProjectName] = true
	}
	for p := range projects {
		pr, err := s.GetProject(ctx(), p)
		if err != nil {
			continue
		}
		keys := append(sortedKeys(envKeys(string(pr.SharedEnv))), SealedKeys(s, "project", p)...)
		if len(keys) > 0 {
			manual = append(manual, fmt.Sprintf("project %s shared variables: likewise for %s", p, strings.Join(keys, ", ")))
		}
	}
	sort.Strings(manual)
	return manual
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
