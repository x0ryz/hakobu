package ops

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
)

// TestDockerDeploys runs real containers: HAKOBU_DOCKER_TEST=1 go test ./internal/ops -run Docker -v
func TestDockerDeploys(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() { DeleteApp(s, app.Name) })

	reload := func() store.App {
		a, err := s.GetApp(ctx(), app.Name)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	deployVersion := func(v string, wantErr bool) string {
		t.Helper()
		buildTestImage(t, nextImageTag(app), v)
		var out strings.Builder
		err := rollOut(s, reload(), nextImageTag(app), &out)
		if wantErr {
			if err == nil {
				t.Fatalf("deploy %s should fail:\n%s", v, out.String())
			}
			return out.String() + err.Error()
		}
		if err != nil {
			t.Fatalf("deploy %s: %v\n%s", v, err, out.String())
		}
		if err := promote(reload(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if err := AddVolume(s, app.Name, "data", "/data"); err != nil {
		t.Fatal(err)
	}

	// First deploy, port detected automatically.
	deployVersion("v1", false)
	expectServing(t, app, "v1")
	if a := reload(); a.LivePort != 8080 {
		t.Errorf("detected port %d, want 8080", a.LivePort)
	}
	v1 := deploy.ImageID(ctx(), ImageTag(app))

	// Volumes: recreate mode, data survives.
	out := deployVersion("v2", false)
	if !strings.Contains(out, "stopping") {
		t.Errorf("app with volumes should stop the old version first:\n%s", out)
	}
	expectServing(t, app, "v2")
	if got := dataLines(t, reload()); got != 2 {
		t.Errorf("volume has %d start entries, want 2 (data lost between deploys?)", got)
	}
	if n := countContainers(t, app.Name+"-"); n != 1 {
		t.Errorf("%d app containers after deploy, want 1", n)
	}
	if deploy.ImageID(ctx(), PreviousImageTag(app)) != v1 {
		t.Error(":previous should be the v1 image")
	}

	// The worker mounts the same volume.
	if err := SaveWorker(s, store.Worker{AppName: app.Name, Name: "w", Command: "sleep 3600"}); err != nil {
		t.Fatal(err)
	}
	if m := dockerOut(t, "inspect", "-f", "{{range .Mounts}}{{.Name}}{{end}}", app.Name+"-worker"); m != dockerVolume(app.Name, "data") {
		t.Errorf("worker mounts %q", m)
	}

	// A third build drops v1: only :latest and :previous are kept.
	deployVersion("v3", false)
	if deploy.ImageID(ctx(), v1) != "" {
		t.Error("the image that fell out of the rotation should be deleted")
	}

	// A broken build is rejected and the old version is started again.
	out = deployVersion("broken", true)
	if !strings.Contains(out, "started the previous version again") {
		t.Errorf("expected the old version to be restarted:\n%s", out)
	}
	expectServing(t, app, "v3")

	// A crash on start fails the deploy at once, with the app's output.
	start := time.Now()
	out = deployVersion("crashing", true)
	if time.Since(start) > 30*time.Second {
		t.Errorf("a crashing app took %v to be rejected", time.Since(start))
	}
	expectServing(t, app, "v3")

	// Limits reach the container; going over the memory limit fails the
	// deploy with the reason.
	if err := SetLimits(s, app.Name, 32, 0.5); err != nil {
		t.Fatal(err)
	}
	out = deployVersion("hungry", true)
	if !strings.Contains(out, "more than its 32 MB memory limit") {
		t.Errorf("expected an out-of-memory reason:\n%s", out)
	}
	expectServing(t, app, "v3")
	deployVersion("v3b", false)
	if got := dockerOut(t, "inspect", "-f", "{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.HostConfig.RestartPolicy.Name}}", reload().ContainerName()); got != "33554432 500000000 unless-stopped" {
		t.Errorf("container limits and restart policy = %q", got)
	}
	if err := SetLimits(s, app.Name, 0, 0); err != nil {
		t.Fatal(err)
	}

	// Shared volumes keep the zero-downtime switch.
	if err := SetShareVolumes(s, app.Name, true); err != nil {
		t.Fatal(err)
	}
	if out := deployVersion("v4", false); strings.Contains(out, "stopping") {
		t.Errorf("shared volumes should not stop the old version:\n%s", out)
	}
	expectServing(t, app, "v4")

	// Rollback goes to v3b, rolling back again returns to v4.
	for _, want := range []string{"v3b", "v4"} {
		if err := StartRollback(s, app.Name, false); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, app.Name)
		expectServing(t, app, want)
	}

	// Deleting the app removes containers, images and volumes.
	if err := DeleteApp(s, app.Name); err != nil {
		t.Fatal(err)
	}
	if n := countContainers(t, app.Name+"-"); n != 0 {
		t.Errorf("%d containers left after delete", n)
	}
	if ok, _ := deploy.ImageExists(ctx(), ImageTag(app)); ok {
		t.Error("image left after delete")
	}
	if vols, _ := deploy.VolumeNames(ctx(), dockerVolume(app.Name, "")); len(vols) != 0 {
		t.Errorf("volumes left after delete: %v", vols)
	}
}

// newDockerTestApp creates an app whose proxy port is free on this machine
// (a real hakobu may be using the first ones).
func newDockerTestApp(t *testing.T, s *store.Store) store.App {
	suffix, _ := RandomHex(3)
	project := "zt" + suffix
	if err := s.CreateProject(ctx(), project); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx(), project)
	// Runs after the tests' own cleanups (LIFO), once the apps are gone.
	t.Cleanup(func() {
		if err := removeProjectNetworks(project); err != nil {
			t.Error(err)
		}
	})
	for i := 0; ; i++ {
		name := fmt.Sprintf("zt%s-%d", suffix, i)
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: p.ID, Name: name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		app, _ := s.GetApp(ctx(), name)
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.Port)); err == nil {
			ln.Close()
			return app
		}
	}
}

func buildTestImage(t *testing.T, tag, version string) {
	t.Helper()
	cmd := `mkdir -p /data && date >> /data/log && exec httpd -f -p 8080 -h /www`
	switch version {
	case "broken":
		cmd = "sleep 3600" // never listens
	case "crashing":
		cmd = "echo missing POSTGRES_PASSWORD >&2; exit 1"
	case "hungry":
		cmd = "exec dd if=/dev/zero of=/dev/null bs=256M count=1"
	}
	dir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM busybox:1.36\nRUN mkdir /www && echo -n %s > /www/index.html\nCMD [\"sh\", \"-c\", %q]\n", version, cmd)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerOut(t, "build", "-q", "-t", tag, dir)
}

func expectServing(t *testing.T, app store.App, want string) {
	t.Helper()
	// What cloudflared sees: the app's alias on the edge network, served
	// by the live container only.
	edge := dockerOut(t, "run", "--rm", "--quiet", "--network", projectEdge(app.ProjectName), "busybox:1.36",
		"sh", "-c", fmt.Sprintf("nslookup %s 127.0.0.11 | grep -c '^Address' ; wget -qO- http://%s:8080/", EdgeAlias(app.Name), EdgeAlias(app.Name)))
	if edge != "2\n"+want { // the resolver's own address, then exactly one container
		t.Errorf("edge network serves %q, want one address and %q", edge, want)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", app.Port))
	if err != nil {
		t.Fatalf("app proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != want {
		t.Errorf("serving %q, want %q", body, want)
	}
}

func dataLines(t *testing.T, app store.App) int {
	return len(strings.Split(strings.TrimSpace(dockerOut(t, "exec", app.ContainerName(), "cat", "/data/log")), "\n"))
}

func countContainers(t *testing.T, prefix string) int {
	n := 0
	for _, name := range strings.Fields(dockerOut(t, "ps", "-a", "--format", "{{.Names}}")) {
		if strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n
}

func waitIdle(t *testing.T, app string) {
	for deadline := time.Now().Add(2 * time.Minute); IsDeploying(app); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job still running")
		}
	}
}

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDockerDataRollback runs its own Postgres container.
func TestDockerDataRollback(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	suffix, _ := RandomHex(3)
	old := PostgresContainer
	PostgresContainer = "zt-pg-" + suffix
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx(), PostgresContainer)
		_ = deploy.RemoveVolume(ctx(), PostgresContainer+"_data")
		PostgresContainer = old
	})
	dir := t.TempDir()
	t.Chdir(dir) // snapshots go to data/snapshots

	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() {
		if err := DeleteApp(s, app.Name); err != nil {
			t.Error(err)
		}
	})
	if err := CreateDatabase(s, app.ProjectName, "zt"+suffix); err != nil {
		t.Fatal(err)
	}
	if err := LinkDatabase(s, app.Name, "zt"+suffix); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDatabase(ctx(), "zt"+suffix)
	sql := func(q string) string {
		t.Helper()
		return dockerOut(t, "exec", PostgresContainer, "psql", "-U", d.User, "-d", d.Name, "-Atc", q)
	}
	reload := func() store.App { a, _ := s.GetApp(ctx(), app.Name); return a }
	// StartDeploy's steps after the build.
	deployVersion := func(v string) {
		t.Helper()
		a := reload()
		buildTestImage(t, nextImageTag(a), v)
		var out strings.Builder
		snapshot := takeSnapshot(s, a, &out)
		if err := rollOut(s, a, nextImageTag(a), &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if err := promote(a, &out); err != nil {
			t.Fatal(err)
		}
		if err := keepSnapshot(s, a.Name, a.LinkedDB, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	rollBack := func(withData bool) {
		t.Helper()
		if err := StartRollback(s, app.Name, withData); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, app.Name)
		logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppName: app.Name, Limit: 1})
		if logs[0].Status != "success" {
			t.Fatalf("rollback failed:\n%s", logs[0].Output)
		}
	}

	sql(`CREATE TABLE notes (s text); INSERT INTO notes VALUES ('v1 data')`)
	deployVersion("v1")
	// v2's "migration" breaks the data.
	deployVersion("v2")
	sql(`DROP TABLE notes; CREATE TABLE notes2 (s text); INSERT INTO notes2 VALUES ('v2 data')`)
	if reason := DataRollbackBlocker(s, reload()); reason != "" {
		t.Fatalf("data rollback blocked: %s", reason)
	}

	// Code and data go back to before v2; the v2 data becomes the snapshot.
	rollBack(true)
	expectServing(t, reload(), "v1")
	if got := sql(`SELECT string_agg(tablename, ',') FROM pg_tables WHERE schemaname = 'public'`); got != "notes" {
		t.Errorf("tables after data rollback: %q", got)
	}
	// And forward again.
	rollBack(true)
	expectServing(t, reload(), "v2")
	if got := sql(`SELECT s FROM notes2`); got != "v2 data" {
		t.Errorf("data after rolling forward: %q", got)
	}
	if got := sql(`SELECT count(*) FROM pg_database WHERE datname LIKE 'hakobu.%'`); got != "0" {
		t.Errorf("%s scratch databases left", got)
	}

	// A code-only rollback keeps the data and drops the snapshot, which no
	// longer matches :previous.
	rollBack(false)
	expectServing(t, reload(), "v1")
	if got := sql(`SELECT s FROM notes2`); got != "v2 data" {
		t.Errorf("code-only rollback changed the data: %q", got)
	}
	if DataRollbackBlocker(s, reload()) == "" {
		t.Error("data rollback offered without a snapshot")
	}

	// If the previous version doesn't start, the current data and version
	// come back.
	deployVersion("v3")
	sql(`INSERT INTO notes2 VALUES ('v3 data')`)
	a := reload()
	if err := s.SetAppSettings(ctx(), store.SetAppSettingsParams{Name: a.Name, HealthCheckPath: "/missing"}); err != nil { // 404 for every version
		t.Fatal(err)
	}
	if err := StartRollback(s, a.Name, true); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a.Name)
	if logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppName: a.Name, Limit: 1}); logs[0].Status != "failed" || !strings.Contains(string(logs[0].Output), "put the current data back") {
		t.Errorf("failed rollback:\n%s", logs[0].Output)
	}
	if err := s.SetAppSettings(ctx(), store.SetAppSettingsParams{Name: a.Name, HealthCheckPath: "/"}); err != nil {
		t.Fatal(err)
	}
	expectServing(t, reload(), "v3")
	if got := sql(`SELECT count(*) FROM notes2`); got != "2" {
		t.Errorf("rows after a failed data rollback: %s, want 2", got)
	}

	// A database another app uses too is never rolled back.
	if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: app.ProjectID, Name: app.Name + "-b", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := DeleteApp(s, app.Name+"-b"); err != nil {
			t.Error(err)
		}
	})
	if err := LinkDatabase(s, app.Name+"-b", d.Name); err != nil {
		t.Fatal(err)
	}
	if reason := DataRollbackBlocker(s, reload()); !strings.Contains(reason, "other apps") {
		t.Errorf("shared database: %q", reason)
	}
}

// TestDockerRotateSecrets runs its own Postgres.
func TestDockerRotateSecrets(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	suffix, _ := RandomHex(3)
	oldPG := PostgresContainer
	PostgresContainer = "zt-pg-" + suffix
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx(), PostgresContainer)
		_ = deploy.RemoveVolume(ctx(), PostgresContainer+"_data")
		PostgresContainer = oldPG
	})
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() {
		if err := DeleteApp(s, app.Name); err != nil {
			t.Error(err)
		}
	})
	must(CreateDatabase(s, app.ProjectName, "zt"+suffix))
	must(LinkDatabase(s, app.Name, "zt"+suffix))
	p, _ := s.GetProject(ctx(), app.ProjectName)
	must(s.CreateStorage(ctx(), store.CreateStorageParams{Name: "zt" + suffix, ProjectID: p.ID, Provider: "s3", Endpoint: "https://s3.example.com",
		AccessKeyID: "key", SecretAccessKey: "secret", Bucket: "files", Region: "auto"}))
	must(LinkStorage(s, app.Name, "zt"+suffix))
	must(SetAppEnv(s, app.Name, "API_KEY=abc"))
	must(SealVar(s, "app", app.Name, "SEALED", "shh"))
	must(s.SetAppSentryKey(ctx(), store.SetAppSentryKeyParams{Name: app.Name, SentryKey: "oldsentrykey"}))
	must(s.NewSession(ctx(), "tok", 42, time.Hour))

	a, _ := s.GetApp(ctx(), app.Name)
	buildTestImage(t, nextImageTag(a), "v1")
	var out strings.Builder
	must(rollOut(s, a, nextImageTag(a), &out))
	must(promote(a, &out))

	dbBefore, _ := s.GetDatabase(ctx(), "zt"+suffix)
	keyBefore, _ := os.ReadFile("master.key")

	var log strings.Builder
	manual, failures := rotateSecrets(s, &log)
	if failures != 0 {
		t.Fatalf("%d failures:\n%s", failures, log.String())
	}

	dbAfter, _ := s.GetDatabase(ctx(), "zt"+suffix)
	a, _ = s.GetApp(ctx(), app.Name)
	if dbAfter.Password == dbBefore.Password || a.SentryKey == "oldsentrykey" {
		t.Error("a secret wasn't replaced")
	}
	// The new database password works, the old one doesn't, connecting
	// like an app does (inside its container Postgres trusts localhost).
	login := func(pw string) error {
		return exec.Command("docker", "run", "--rm", "--quiet", "--network", ProjectNetwork(app.ProjectName), "-e", "PGPASSWORD="+pw, "postgres:18",
			"psql", "-h", PostgresContainer, "-U", dbAfter.User, "-d", dbAfter.Name, "-c", "SELECT 1").Run()
	}
	if login(string(dbAfter.Password)) != nil || login(string(dbBefore.Password)) == nil {
		t.Error("database password not rotated")
	}
	// The app was restarted with the new values and still serves.
	expectServing(t, a, "v1")
	env, _ := deploy.ContainerEnv(ctx(), a.ContainerName())
	if !strings.Contains(env["DATABASE_URL"], string(dbAfter.Password)) || env["S3_ACCESS_KEY_ID"] != "key" || env["SEALED"] != "shh" {
		t.Errorf("the app runs with old values: DATABASE_URL=%q S3_ACCESS_KEY_ID=%q", env["DATABASE_URL"], env["S3_ACCESS_KEY_ID"])
	}
	if _, _, err := s.SessionUser(ctx(), "tok"); err == nil {
		t.Error("sessions survived")
	}
	if keyAfter, _ := os.ReadFile("master.key"); string(keyAfter) == string(keyBefore) {
		t.Error("master key not rotated")
	}
	if !slices.ContainsFunc(manual, func(m string) bool { return strings.Contains(m, "API_KEY") && strings.Contains(m, "SEALED") }) {
		t.Errorf("the user's variables aren't listed to replace: %v", manual)
	}
	if !slices.ContainsFunc(manual, func(m string) bool { return strings.Contains(m, "storage zt"+suffix) }) {
		t.Errorf("the storage's keys aren't listed to replace: %v", manual)
	}
}

func TestDockerProjectIsolation(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	// newDockerTestApp puts each app in a project of its own.
	a, b := newDockerTestApp(t, s), newDockerTestApp(t, s)
	for _, app := range []store.App{a, b} {
		buildTestImage(t, nextImageTag(app), app.Name)
		var out strings.Builder
		if err := rollOut(s, app, nextImageTag(app), &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
	}
	a, _ = s.GetApp(ctx(), a.Name)
	b, _ = s.GetApp(ctx(), b.Name)
	bIP, err := deploy.ContainerIP(ctx(), b.ContainerName(), ProjectNetwork(b.ProjectName))
	if err != nil {
		t.Fatal(err)
	}
	// From a container on a's networks, what answers?
	reach := func(network, target string) bool {
		return exec.Command("docker", "run", "--rm", "--quiet", "--network", network, "busybox:1.36",
			"wget", "-q", "-T", "3", "-O", "/dev/null", "http://"+target+":8080/").Run() == nil
	}
	if !reach(ProjectNetwork(a.ProjectName), a.ContainerName()) {
		t.Error("an app isn't reachable in its own project")
	}
	for _, target := range []string{b.ContainerName(), bIP} {
		if reach(ProjectNetwork(a.ProjectName), target) {
			t.Errorf("another project's app is reachable at %s", target)
		}
	}
	if reach(projectEdge(a.ProjectName), EdgeAlias(b.Name)) {
		t.Error("another project's app is reachable on the edge network")
	}

	// Deleting a project removes its networks.
	if err := DeleteProject(s, b.ProjectName); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{ProjectNetwork(b.ProjectName), projectEdge(b.ProjectName)} {
		if exec.Command("docker", "network", "inspect", n).Run() == nil {
			t.Errorf("network %s left after deleting its project", n)
		}
	}
	if err := DeleteProject(s, a.ProjectName); err != nil {
		t.Fatal(err)
	}
}

// TestDockerRestoreOnNewServer restores a database backup into a Postgres
// that has never seen the database, as after the panel was restored on a
// new server: the role and database are made first.
func TestDockerRestoreOnNewServer(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	fakeR2(t)
	suffix, _ := RandomHex(3)
	old := PostgresContainer
	PostgresContainer = "zt-pg-" + suffix
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx(), PostgresContainer)
		_ = deploy.RemoveVolume(ctx(), PostgresContainer+"_data")
		PostgresContainer = old
	})
	dir := t.TempDir()
	t.Chdir(dir) // data/tmp
	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}))
	must(SetupBackups(s))
	project := "zt" + suffix
	must(CreateProject(s, project))
	t.Cleanup(func() { _ = removeProjectNetworks(project) })
	must(CreateDatabase(s, project, "zt"+suffix))
	d, _ := s.GetDatabase(ctx(), "zt"+suffix)
	dockerOut(t, "exec", PostgresContainer, "psql", "-U", d.User, "-d", d.Name, "-c", "CREATE TABLE t (n int); INSERT INTO t VALUES (7)")
	id, err := BackupDatabase(s, d.Name)
	must(err)

	// The new server's Postgres: nothing of this database or its role.
	must(deploy.PostgresExec(ctx(), PostgresContainer, `DROP DATABASE "`+d.Name+`" WITH (FORCE)`))
	must(deploy.PostgresExec(ctx(), PostgresContainer, `DROP USER "`+d.User+`"`))
	b, err := s.GetBackup(ctx(), id)
	must(err)
	must(restoreBackup(s, b))
	if n := dockerOut(t, "exec", PostgresContainer, "psql", "-U", d.User, "-d", d.Name, "-Atc", "SELECT n FROM t"); n != "7" {
		t.Errorf("restored row = %q", n)
	}
	// The recreated role logs in with the password apps were given.
	dockerOut(t, "exec", "-e", "PGPASSWORD="+string(d.Password), PostgresContainer, "psql", "-h", "127.0.0.1", "-U", d.User, "-d", d.Name, "-c", "SELECT 1")
	must(DeleteDatabase(s, d.Name))
}

// TestDockerVolumeBackup backs a volume up while the app writes to it,
// changes the volume and restores the backup.
func TestDockerVolumeBackup(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	objects, _ := fakeR2(t)
	dir := t.TempDir()
	t.Chdir(dir) // data/tmp
	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc"}))
	must(SetupBackups(s))
	app := newDockerTestApp(t, s)
	t.Cleanup(func() { _ = DeleteApp(s, app.Name) })
	must(AddVolume(s, app.Name, "data", "/data"))
	buildTestImage(t, nextImageTag(app), "v1")
	var out strings.Builder
	must(rollOut(s, app, nextImageTag(app), &out))
	must(promote(app, &out))
	app, _ = s.GetApp(ctx(), app.Name)
	live := app.ContainerName()
	dockerOut(t, "exec", live, "sh", "-c", "echo secret-contents > /data/file && mkdir -p /data/sub && echo x > /data/sub/y && chown 1234:5678 /data/file && chmod 600 /data/file")

	id, err := BackupVolume(s, app.Name, "data")
	must(err)
	if st := dockerOut(t, "inspect", "-f", "{{.State.Status}}", live); st != "running" {
		t.Errorf("app is %s after the backup, want running", st)
	}
	for k, v := range objects {
		if strings.Contains(string(v), "secret-contents") {
			t.Errorf("%s holds the volume in the clear", k)
		}
	}
	must(VerifyVolumeBackup(s, id))
	b, _ := s.GetVolumeBackup(ctx(), id)
	if b.VerifyError != "" || b.Files < 3 { // log, file, sub/y
		t.Errorf("check: %d files, %q", b.Files, b.VerifyError)
	}

	dockerOut(t, "exec", live, "sh", "-c", "rm -r /data/sub && echo changed > /data/file && touch /data/new")
	must(StartVolumeRestore(s, app.Name, "data", id))
	waitIdle(t, app.Name)
	if j := VolumeJob(app.Name, "data"); j.Failed {
		t.Fatalf("restore: %s", j.Last)
	}
	got := dockerOut(t, "exec", live, "sh", "-c", "cat /data/file /data/sub/y; ls /data; stat -c '%u:%g %a' /data/file")
	if want := "secret-contents\nx\nfile\nlog\nsub\n1234:5678 600"; got != want {
		t.Errorf("after restore:\n%s\nwant:\n%s", got, want)
	}
	expectServing(t, app, "v1")
}
