package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/build"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/proxy"
	"github.com/x0ryz/hakobu/internal/store"
)

func ctx() context.Context { return context.Background() }

// An app's images: :latest is live, :previous is the rollback target and
// :next is a build that hasn't passed its health check yet.
func ImageTag(app store.App) string         { return "hakobu/" + app.Name + ":latest" }
func PreviousImageTag(app store.App) string { return "hakobu/" + app.Name + ":previous" }
func nextImageTag(app store.App) string     { return "hakobu/" + app.Name + ":next" }

var (
	jobsMu  sync.Mutex
	running = map[string]bool{} // apps with a deploy, rollback or deletion in progress
	queued  = map[string]bool{} // a push arrived during a deploy; deploy again after it
)

func IsDeploying(name string) bool {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	return running[name]
}

// reserve marks the app busy; with queuePush a busy app gets a follow-up
// push deploy instead of an error.
func reserve(name string, queuePush bool) (ok bool, err error) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if running[name] {
		if queuePush {
			queued[name] = true
			return false, nil
		}
		return false, fmt.Errorf("a deploy of %s is in progress, try again when it finishes", name)
	}
	running[name] = true
	return true, nil
}

// release frees the app and reports whether a queued push should run now.
func release(name string) (runQueued bool) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	delete(running, name)
	runQueued = queued[name]
	delete(queued, name)
	return runQueued
}

// deployLog streams job output into its deploy_logs row, at most once a second.
type deployLog struct {
	s       *store.Store
	id      int64
	mu      sync.Mutex
	buf     bytes.Buffer
	flushed time.Time
}

func (l *deployLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	if time.Since(l.flushed) > time.Second {
		l.s.UpdateDeployLog(ctx(), store.UpdateDeployLogParams{ID: l.id, Status: "running", Output: l.buf.String()})
		l.flushed = time.Now()
	}
	return len(p), nil
}

func (l *deployLog) finish(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := "success"
	if err != nil {
		status = "failed"
		fmt.Fprintln(&l.buf, "error:", err)
	}
	l.s.UpdateDeployLog(ctx(), store.UpdateDeployLogParams{ID: l.id, Status: status, Output: l.buf.String()})
}

// startJob runs fn in the background with a deploy log; only one job per
// app runs at a time, and a push arriving meanwhile is deployed after it.
func startJob(s *store.Store, appName, trigger string, fn func(app store.App, out io.Writer) error) error {
	ok, err := reserve(appName, trigger == "push")
	if !ok {
		return err
	}
	app, err := s.GetApp(ctx(), appName)
	if err == nil {
		var id int64
		id, err = s.CreateDeployLog(ctx(), store.CreateDeployLogParams{AppName: appName, Trigger: trigger, Status: "running"})
		if err == nil {
			go func() {
				log := &deployLog{s: s, id: id}
				log.finish(fn(app, log))
				if release(appName) {
					StartDeploy(s, appName, "push")
				}
			}()
			return nil
		}
	}
	release(appName)
	return err
}

// StartDeploy clones the app's repo, builds a new image and rolls it out.
func StartDeploy(s *store.Store, appName, trigger string) error {
	return startJob(s, appName, trigger, func(app store.App, out io.Writer) error {
		token, err := repoToken(s, app.Repo)
		if err != nil {
			return err
		}
		workDir := workDir(app.Name)
		defer os.RemoveAll(workDir) // the next deploy clones again anyway
		fmt.Fprintln(out, "cloning", app.Repo)
		if err := build.CloneRepo(github.CloneURL(app.Repo), token, workDir, out); err != nil {
			return fmt.Errorf("clone failed: %w", err)
		}
		dir, err := buildDir(workDir, app.BuildPath)
		if err != nil {
			return err
		}

		next := nextImageTag(app)
		// Drops the :next tag in every case: a failed build or health check
		// leaves nothing behind, a successful one is :latest by then.
		defer deploy.RemoveImage(ctx(), next)
		fmt.Fprintf(out, "building %s from %s (%s)\n", next, dir, app.BuildStrategy)
		if err := build.BuildWithStrategy(dir, next, app.BuildStrategy, out); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
		snapshot := takeSnapshot(s, app, out)
		defer os.Remove(snapshot) // kept by keepSnapshot, which moves it
		if err := rollOut(s, app, next, out); err != nil {
			return err
		}
		if err := promote(app, out); err != nil {
			return err
		}
		if err := keepSnapshot(s, app.Name, app.LinkedDB, snapshot); err != nil {
			fmt.Fprintln(out, "warning: failed to keep the database snapshot:", err)
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil {
			if err := runWorker(s, app, w, out); err != nil {
				return fmt.Errorf("app deployed, but worker failed: %w", err)
			}
		}
		return nil
	})
}

// buildDir resolves the app's build path inside the clone. Symlinks are
// resolved first: a repo could otherwise make its build path point at a
// host directory and have it copied into the image.
func buildDir(clone, buildPath string) (string, error) {
	root, err := filepath.EvalSymlinks(clone)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, strings.Trim(buildPath, "/")))
	if err != nil {
		return "", fmt.Errorf("build path %q not found in the repo", buildPath)
	}
	if dir != root && !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid build path %q: it leads outside the repo", buildPath)
	}
	return dir, nil
}

// promote makes the :next build that just went live :latest; the image it
// replaced becomes the rollback target and the old rollback target is deleted.
func promote(app store.App, out io.Writer) error {
	latest, prev := ImageTag(app), PreviousImageTag(app)
	dropped := deploy.ImageID(ctx(), prev)
	if ok, _ := deploy.ImageExists(ctx(), latest); ok {
		if err := deploy.TagImage(ctx(), latest, prev); err != nil {
			fmt.Fprintln(out, "warning: failed to keep the previous image for rollback:", err)
		}
	}
	if err := deploy.TagImage(ctx(), nextImageTag(app), latest); err != nil {
		return err
	}
	if dropped != "" && dropped != deploy.ImageID(ctx(), prev) && dropped != deploy.ImageID(ctx(), latest) {
		deploy.RemoveImage(ctx(), dropped)
	}
	return nil
}

// StartRollback redeploys the image that was live before the current one;
// the two swap places, so rolling back again returns to where it started.
// withData also returns the database to its snapshot from before the last
// deploy, losing what was written since; the current data becomes the
// snapshot, so rolling back again returns it too.
func StartRollback(s *store.Store, appName string, withData bool) error {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		return err
	}
	if ok, _ := deploy.ImageExists(ctx(), PreviousImageTag(app)); !ok {
		return fmt.Errorf("no previous build to roll back to")
	}
	if reason := DataRollbackBlocker(s, app); withData && reason != "" {
		return fmt.Errorf("can't roll back the database: %s", reason)
	}
	trigger := "rollback"
	if withData {
		trigger = "rollback with data"
	}
	return startJob(s, appName, trigger, func(app store.App, out io.Writer) error {
		prev, latest, swap := PreviousImageTag(app), ImageTag(app), nextImageTag(app)
		defer deploy.RemoveImage(ctx(), swap)
		current := ""
		if withData {
			var err error
			if current, err = rollBackData(s, app, out); err != nil {
				return err
			}
			// Moved by keepSnapshot on success; kept if undoing fails.
			defer func() {
				if current != "" {
					os.Remove(current)
				}
			}()
		}
		if err := rollOut(s, app, prev, out); err != nil {
			if withData && !undoDataRollback(s, app, current, out) {
				current = ""
			}
			return err
		}
		for _, t := range [][2]string{{latest, swap}, {prev, latest}, {swap, prev}} {
			if err := deploy.TagImage(ctx(), t[0], t[1]); err != nil {
				return err
			}
		}
		// The snapshot must match the new :previous: the data it ran with,
		// or nothing after a code-only rollback.
		if err := keepSnapshot(s, app.Name, app.LinkedDB, current); err != nil {
			fmt.Fprintln(out, "warning: failed to keep the database snapshot:", err)
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil {
			return runWorker(s, app, w, out)
		}
		return nil
	})
}

// rollBackData saves the current data, stops the app and its worker so
// nothing writes during the swap, and puts the snapshot in place. It
// returns the dump of the current data.
func rollBackData(s *store.Store, app store.App, out io.Writer) (current string, err error) {
	d, err := s.GetDatabase(ctx(), app.LinkedDB)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(out, "saving the current data of", d.Name)
	if current, err = dumpTo(d); err != nil {
		return "", fmt.Errorf("couldn't save the current data, nothing changed: %w", err)
	}
	fmt.Fprintln(out, "stopping", app.Name, "and restoring", d.Name, "from", app.SnapshotAt)
	err = stopApp(app)
	if err == nil {
		err = replaceDatabase(d, snapshotPath(app.Name))
	}
	if err != nil {
		os.Remove(current)
		restartContainer(app, app.ContainerName(), out)
		restartWorker(s, app, out)
		return "", fmt.Errorf("the data is unchanged: %w", err)
	}
	return current, nil
}

// undoDataRollback puts the saved current data back when the previous
// version didn't start, then starts the current version again. If it
// can't, the dump is kept on disk and it returns false.
func undoDataRollback(s *store.Store, app store.App, current string, out io.Writer) bool {
	err := stopApp(app) // a recreate deploy may have started it again already
	if err == nil {
		var d store.Database
		if d, err = s.GetDatabase(ctx(), app.LinkedDB); err == nil {
			err = replaceDatabase(d, current)
		}
	}
	if err != nil {
		keep := filepath.Join(snapshotDir, app.Name+"-before-rollback.sql.gz")
		if rerr := os.Rename(current, keep); rerr != nil {
			keep = current
		}
		fmt.Fprintf(out, "ERROR: couldn't put the current data back (%v); it's saved in %s\n", err, keep)
		return false
	}
	fmt.Fprintln(out, "put the current data back")
	restartContainer(app, app.ContainerName(), out)
	restartWorker(s, app, out)
	return true
}

// stopApp stops the app and its worker so nothing writes to the database.
func stopApp(app store.App) error {
	if err := deploy.StopContainer(ctx(), app.ContainerName()); err != nil {
		return fmt.Errorf("couldn't stop %s: %w", app.Name, err)
	}
	if err := deploy.StopContainer(ctx(), app.Name+"-worker"); err != nil {
		return fmt.Errorf("couldn't stop the worker of %s: %w", app.Name, err)
	}
	return nil
}

func restartWorker(s *store.Store, app store.App, out io.Writer) {
	if w, err := s.GetWorker(ctx(), app.Name); err == nil {
		if err := runWorker(s, app, w, out); err != nil {
			fmt.Fprintln(out, "failed to start the worker again:", err)
		}
	}
}

// rollOut is the blue/green swap: start the inactive slot, health-check it
// on the docker network, point the proxy at it, then remove the old slot.
// If the candidate never gets healthy the old slot keeps serving.
//
// An app with volumes is recreated instead, unless it shares them: the old
// version is stopped first so two versions never write the same data, and
// started again if the new one fails.
func rollOut(s *store.Store, app store.App, imageTag string, out io.Writer) error {
	hint := portHint(app, int64(deploy.ExposedPort(ctx(), imageTag)))
	env, err := appEnv(s, app, hint)
	if err != nil {
		return err
	}
	oldSlot, newSlot := "blue", "green"
	if app.ActiveSlot == "green" {
		oldSlot, newSlot = "green", "blue"
	}
	candidate, old := app.Name+"-"+newSlot, app.Name+"-"+oldSlot

	binds, err := appBinds(s, app.Name)
	if err != nil {
		return err
	}
	recreate := len(binds) > 0 && app.ShareVolumes == 0
	if recreate {
		fmt.Fprintln(out, "the app has volumes: stopping", old, "before starting the new version")
		if err := deploy.StopContainer(ctx(), old); err != nil {
			return err
		}
	}
	restoreOld := func() {
		if recreate {
			restartContainer(app, old, out)
		}
	}

	fmt.Fprintln(out, "starting", candidate)
	if _, err := deploy.RunAppContainer(ctx(), imageTag, candidate, appOptions(app, env, binds)); err != nil {
		restoreOld()
		return err
	}
	ip, err := deploy.ContainerIP(ctx(), candidate)
	if err != nil {
		deploy.RemoveContainer(ctx(), candidate)
		restoreOld()
		return err
	}
	path := "/" + strings.TrimPrefix(app.HealthCheckPath, "/")
	requireOK := path != "/"
	if app.ContainerPort > 0 {
		fmt.Fprintf(out, "waiting for port %d to answer %s...\n", app.ContainerPort, path)
	} else {
		fmt.Fprintf(out, "waiting for the app to listen on a port (PORT=%d) and answer %s...\n", hint, path)
	}

	var port int64
	var loopbackOnly []int
	var crashed, oom bool
	healthy := deploy.WaitHealthy(60, time.Second, func() bool {
		// A container that exits or restarts will never answer; stop waiting.
		if st := deploy.ContainerState(ctx(), candidate); st.Status == "exited" || st.Status == "dead" || st.Restarts > 0 || st.OOMKilled {
			crashed, oom = true, st.OOMKilled
			return true
		}
		port = app.ContainerPort
		if port == 0 {
			var reachable []int
			reachable, loopbackOnly, _ = deploy.ListeningPorts(ctx(), candidate)
			port = pickPort(reachable, hint)
		}
		return port > 0 && deploy.HTTPCheck(fmt.Sprintf("http://%s:%d%s", ip, port, path), requireOK)
	})
	if !healthy || crashed {
		logs, _ := deploy.ContainerLogs(ctx(), candidate, 50)
		deploy.RemoveContainer(ctx(), candidate)
		restoreOld()
		reason := fmt.Sprintf("didn't answer on port %d within 60s", port)
		switch {
		case oom:
			reason = "was killed: " + oomText(app)
		case crashed:
			reason = "exited while starting (see its output below; a missing variable or an unreachable database are the usual causes)"
		case port == 0 && len(loopbackOnly) > 0:
			reason = fmt.Sprintf("listens only on 127.0.0.1 (port %v); bind it to 0.0.0.0", loopbackOnly)
		case port == 0:
			reason = "didn't listen on any port within 60s"
		case requireOK:
			reason = fmt.Sprintf("didn't return 2xx on port %d%s within 60s", port, path)
		}
		kept := "previous version keeps running"
		if recreate {
			kept = "previous version was started again"
		}
		return fmt.Errorf("new version %s; %s\n--- container output ---\n%s", reason, kept, logs)
	}

	// Healthy: keep it running, and let the tunnel find it under the app's
	// alias next to the old version.
	for _, step := range []func() error{
		func() error { return deploy.KeepRestarting(ctx(), candidate) },
		func() error { return deploy.ConnectEdge(ctx(), candidate, EdgeAlias(app.Name)) },
	} {
		if err := step(); err != nil {
			if rerr := deploy.RemoveContainer(ctx(), candidate); rerr != nil {
				fmt.Fprintln(out, "warning:", rerr)
			}
			restoreOld()
			return err
		}
	}
	if _, err := proxy.Ensure(app.Name, app.Port); err != nil {
		return err
	}
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, port))
	if err := proxy.SetTarget(app.Name, u); err != nil {
		return err
	}
	if err := s.SetAppLive(ctx(), store.SetAppLiveParams{Name: app.Name, ActiveSlot: newSlot, LivePort: port}); err != nil {
		return err
	}
	if err := SyncTunnel(s); err != nil {
		fmt.Fprintln(out, "warning: failed to update the tunnel's routes, retrying in the background:", err)
	}
	// The old version gets no new requests, then up to 10 seconds to
	// finish the ones in flight.
	if err := deploy.DisconnectEdge(ctx(), old); err != nil {
		fmt.Fprintln(out, "warning:", err)
	}
	if err := deploy.StopContainer(ctx(), old); err != nil {
		fmt.Fprintln(out, "warning:", err) // removing it below kills it anyway
	}
	if err := deploy.RemoveContainer(ctx(), old); err != nil {
		fmt.Fprintln(out, "warning: failed to remove previous container:", err)
	}
	fmt.Fprintf(out, "live: container port %d, 127.0.0.1:%d (%s)\n", port, app.Port, imageTag)
	return nil
}

// restartContainer brings a stopped previous version back after a failed
// recreate deploy and points the proxy at it again.
func restartContainer(app store.App, name string, out io.Writer) {
	if status, _ := deploy.ContainerStatus(ctx(), name); status == "not found" {
		return
	}
	if err := deploy.StartContainer(ctx(), name); err != nil {
		fmt.Fprintln(out, "failed to start the previous version again:", err)
		return
	}
	if ip, err := deploy.ContainerIP(ctx(), name); err == nil {
		u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, portHint(app, 0)))
		proxy.SetTarget(app.Name, u)
	}
	fmt.Fprintln(out, "started the previous version again:", name)
}

// portHint is the PORT the app is told to listen on: the configured port,
// else the one it used last time, else the image's EXPOSE, else 8080.
func portHint(app store.App, exposed int64) int64 {
	for _, p := range []int64{app.ContainerPort, app.LivePort, exposed} {
		if p > 0 {
			return p
		}
	}
	return 8080
}

// pickPort prefers the hinted port among the listening ones.
func pickPort(listening []int, hint int64) int64 {
	for _, p := range listening {
		if int64(p) == hint {
			return hint
		}
	}
	if len(listening) > 0 {
		return int64(listening[0])
	}
	return 0
}

// EnsureProxy restarts an app's proxy after an agent restart and points it
// at the active container.
func EnsureProxy(app store.App) {
	isNew, err := proxy.Ensure(app.Name, app.Port)
	if err != nil || !isNew {
		return
	}
	ip, err := deploy.ContainerIP(ctx(), app.ContainerName())
	if err != nil {
		return
	}
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, portHint(app, 0)))
	proxy.SetTarget(app.Name, u)
}

func RemoveProxy(name string) { proxy.Remove(name) }

// Workers.

func runWorker(s *store.Store, app store.App, w store.Worker, out io.Writer) error {
	env, err := WorkerEnv(s, app, w)
	if err != nil {
		return err
	}
	binds, err := appBinds(s, app.Name)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "starting worker", w.ContainerName())
	_, err = deploy.RunWorkerContainer(ctx(), ImageTag(app), w.ContainerName(), w.Command, appOptions(app, env, binds))
	return err
}

func appOptions(app store.App, env, binds []string) deploy.AppOptions {
	return deploy.AppOptions{App: app.Name, Env: env, Binds: binds, MemoryMB: app.MemoryMB, CPUs: app.Cpus}
}

// SaveWorker creates or updates the app's worker and restarts it if the
// app has been built.
func SaveWorker(s *store.Store, w store.Worker) error {
	app, err := s.GetApp(ctx(), w.AppName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(w.Command) == "" {
		return fmt.Errorf("worker command is required")
	}
	if err := s.SaveWorker(ctx(), store.SaveWorkerParams(w)); err != nil {
		return err
	}
	return RestartWorker(s, app)
}

func RestartWorker(s *store.Store, app store.App) error {
	w, err := s.GetWorker(ctx(), app.Name)
	if err != nil {
		return fmt.Errorf("%s has no worker", app.Name)
	}
	if ok, _ := deploy.ImageExists(ctx(), ImageTag(app)); !ok {
		return nil // started by the first successful deploy
	}
	return runWorker(s, app, w, io.Discard)
}

func DeleteWorker(s *store.Store, appName string) error {
	if err := deploy.RemoveContainer(ctx(), appName+"-worker"); err != nil {
		return err
	}
	return s.DeleteWorker(ctx(), appName)
}
