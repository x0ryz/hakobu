package ops

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestPanelBackupAndRestore(t *testing.T) {
	objects, _ := fakeR2(t)
	t.Chdir(t.TempDir()) // data/tmp, data/public_host
	if err := os.MkdirAll("data", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("panel.example.com"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join("data", "hakobu.db"))
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
	if err := BackupPanel(s); err == nil {
		t.Error("backed up before backups were set up")
	}
	must(SetupBackups(s))
	must(s.CreateProject(ctx(), "shop"))
	must(BackupPanel(s))
	if LastPanelBackup() == "" || strings.HasPrefix(LastPanelBackup(), "failed") {
		t.Errorf("last panel backup = %q", LastPanelBackup())
	}
	for k, v := range objects {
		if strings.HasPrefix(k, panelPrefix) && bytes.Contains(v, []byte("shop")) {
			t.Errorf("%s holds the database in the clear", k)
		}
	}

	kf, err := CurrentKeyFile(s)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKeyFile(bytes.NewReader(kf.Marshal()))
	if err != nil || parsed != kf || kf.Bucket == "" || kf.PublicHost != "panel.example.com" {
		t.Fatalf("key file round trip: %+v, %v (want %+v)", parsed, err, kf)
	}

	// A new server: no database, no key, no address yet.
	newServer := t.TempDir()
	t.Chdir(newServer)
	must(os.MkdirAll("data", 0o700))
	must(os.MkdirAll("key", 0o700))
	c := cloudflare.Client{Token: "tok"}
	if _, err := RestorePanel(c, parsed, "data/hakobu.db", "key/master.key", "19990101-000000"); err == nil {
		t.Error("restored a backup that doesn't exist")
	}
	if _, err := os.Stat("key/master.key"); err == nil {
		t.Error("a failed restore left the key behind")
	}
	if _, err := RestorePanel(c, parsed, "data/hakobu.db", "key/master.key", ""); err != nil {
		t.Fatal(err)
	}
	restored, err := store.OpenWithKey("data/hakobu.db", "key/master.key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.GetProject(ctx(), "shop"); err != nil {
		t.Errorf("restored panel lost its project: %v", err)
	}
	if cf, err := restored.GetCloudflare(ctx()); err != nil || string(cf.ApiToken) != "tok" {
		t.Errorf("restored Cloudflare token = %q, %v", cf.ApiToken, err)
	}
	if config.PublicHost() != "panel.example.com" {
		t.Errorf("restored address = %q", config.PublicHost())
	}
	if _, err := RestorePanel(c, parsed, "data/hakobu.db", "key/master.key", ""); err == nil {
		t.Error("restored over an existing panel")
	}
}

func TestPanelBackupRotation(t *testing.T) {
	objects, _ := fakeR2(t)
	t.Chdir(t.TempDir())
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareToken(ctx(), "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := SetupBackups(s); err != nil {
		t.Fatal(err)
	}
	// One backup a day for 20 days, the newest today, in two parts each.
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for d := range 20 {
		key := panelPrefix + now.AddDate(0, 0, -d).Format(panelTimeFormat) + ".db.enc"
		objects[partKey(key, 0)], objects[partKey(key, 1)] = []byte("a"), []byte("b")
	}
	objects["panel/notes.txt/000"] = []byte("not ours")
	if err := rotatePanelBackups(s, now); err != nil {
		t.Fatal(err)
	}
	c, cf, _ := cfClient(s)
	list, err := listPanelBackups(c, cf.AccountID, cf.BackupBucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != keepPanelBackups || list[len(list)-1].parts != 2 { // the week under the lock
		var at []string
		for _, b := range list {
			at = append(at, fmt.Sprint(b.at.Format("01-02"), "/", b.parts))
		}
		t.Errorf("kept %d backups: %v", len(list), at)
	}
	if _, ok := objects["panel/notes.txt/000"]; !ok {
		t.Error("deleted a file that isn't a panel backup")
	}
}
