package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
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
		buildDir := workDir
		if p := strings.Trim(app.BuildPath, "/"); p != "" && p != "." {
			if strings.Contains(p, "..") {
				return fmt.Errorf("invalid build path %q", app.BuildPath)
			}
			buildDir += "/" + p
		}

		next := nextImageTag(app)
		// Drops the :next tag in every case: a failed build or health check
		// leaves nothing behind, a successful one is :latest by then.
		defer deploy.RemoveImage(ctx(), next)
		fmt.Fprintf(out, "building %s from %s (%s)\n", next, buildDir, app.BuildStrategy)
		if err := build.BuildWithStrategy(buildDir, next, app.BuildStrategy, out); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
		if err := rollOut(s, app, next, out); err != nil {
			return err
		}
		// Only a build that went live becomes :latest; the one it replaced
		// becomes the rollback target and the old rollback target is deleted.
		dropped := deploy.ImageID(ctx(), PreviousImageTag(app))
		if ok, _ := deploy.ImageExists(ctx(), ImageTag(app)); ok {
			if err := deploy.TagImage(ctx(), ImageTag(app), PreviousImageTag(app)); err != nil {
				fmt.Fprintln(out, "warning: failed to keep the previous image for rollback:", err)
			}
		}
		if err := deploy.TagImage(ctx(), next, ImageTag(app)); err != nil {
			return err
		}
		if dropped != "" && dropped != deploy.ImageID(ctx(), PreviousImageTag(app)) && dropped != deploy.ImageID(ctx(), ImageTag(app)) {
			deploy.RemoveImage(ctx(), dropped)
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil {
			if err := runWorker(s, app, w, out); err != nil {
				return fmt.Errorf("app deployed, but worker failed: %w", err)
			}
		}
		return nil
	})
}

// StartRollback redeploys the image that was live before the current one;
// the two swap places, so rolling back again returns to where it started.
func StartRollback(s *store.Store, appName string) error {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		return err
	}
	if ok, _ := deploy.ImageExists(ctx(), PreviousImageTag(app)); !ok {
		return fmt.Errorf("no previous build to roll back to")
	}
	return startJob(s, appName, "rollback", func(app store.App, out io.Writer) error {
		prev, latest, swap := PreviousImageTag(app), ImageTag(app), nextImageTag(app)
		defer deploy.RemoveImage(ctx(), swap)
		if err := rollOut(s, app, prev, out); err != nil {
			return err
		}
		for _, t := range [][2]string{{latest, swap}, {prev, latest}, {swap, prev}} {
			if err := deploy.TagImage(ctx(), t[0], t[1]); err != nil {
				return err
			}
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil {
			return runWorker(s, app, w, out)
		}
		return nil
	})
}

// rollOut is the blue/green swap: start the inactive slot, health-check it
// on the docker network, point the proxy at it, then remove the old slot.
// If the candidate never gets healthy the old slot keeps serving.
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
	candidate := app.Name + "-" + newSlot

	fmt.Fprintln(out, "starting", candidate)
	if _, err := deploy.RunAppContainer(ctx(), imageTag, candidate, env); err != nil {
		return err
	}
	ip, err := deploy.ContainerIP(ctx(), candidate)
	if err != nil {
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
	healthy := deploy.WaitHealthy(60, time.Second, func() bool {
		port = app.ContainerPort
		if port == 0 {
			var reachable []int
			reachable, loopbackOnly, _ = deploy.ListeningPorts(ctx(), candidate)
			port = pickPort(reachable, hint)
		}
		return port > 0 && deploy.HTTPCheck(fmt.Sprintf("http://%s:%d%s", ip, port, path), requireOK)
	})
	if !healthy {
		logs, _ := deploy.ContainerLogs(ctx(), candidate, 50)
		deploy.RemoveContainer(ctx(), candidate)
		reason := fmt.Sprintf("didn't answer on port %d within 60s", port)
		switch {
		case port == 0 && len(loopbackOnly) > 0:
			reason = fmt.Sprintf("listens only on 127.0.0.1 (port %v); bind it to 0.0.0.0", loopbackOnly)
		case port == 0:
			reason = "didn't listen on any port within 60s"
		case requireOK:
			reason = fmt.Sprintf("didn't return 2xx on port %d%s within 60s", port, path)
		}
		return fmt.Errorf("new version %s; previous version keeps running\n--- container output ---\n%s", reason, logs)
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
	if err := deploy.RemoveContainer(ctx(), app.Name+"-"+oldSlot); err != nil {
		fmt.Fprintln(out, "warning: failed to remove previous container:", err)
	}
	fmt.Fprintf(out, "live: container port %d, 127.0.0.1:%d (%s)\n", port, app.Port, imageTag)
	return nil
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
	fmt.Fprintln(out, "starting worker", w.ContainerName())
	_, err = deploy.RunWorkerContainer(ctx(), ImageTag(app), w.ContainerName(), w.Command, env)
	return err
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
