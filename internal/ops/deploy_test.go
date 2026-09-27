package ops

import (
	"path/filepath"
	"testing"

	"github.com/x0ryz/hakobu/internal/store"
)

func TestJobReservation(t *testing.T) {
	if ok, err := reserve("web", false); !ok || err != nil {
		t.Fatalf("first reserve = %v, %v", ok, err)
	}
	if ok, err := reserve("web", false); ok || err == nil {
		t.Error("a second manual deploy should be refused while one runs")
	}
	if ok, err := reserve("web", true); ok || err != nil {
		t.Errorf("a push during a deploy should be queued, got %v, %v", ok, err)
	}
	if !release("web") {
		t.Error("release should report the queued push")
	}
	if IsDeploying("web") {
		t.Error("app still marked busy after release")
	}
	if ok, _ := reserve("web", false); !ok || release("web") {
		t.Error("the queue should be empty after it was handed out")
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 64 << 30: "64.0 GB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDockerVolumeNamesAreUnambiguous(t *testing.T) {
	if dockerVolume("a-b", "c") == dockerVolume("a", "b-c") {
		t.Error("different app/volume pairs map to the same Docker volume")
	}
	if got := dockerVolume("web", "data"); got != "hakobu-vol-web_data" {
		t.Errorf("dockerVolume = %q", got)
	}
}

func TestAddVolumeValidation(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := AddVolume(s, "web", "data", "/app/data/"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path string }{
		{"data", "/other"},     // duplicate name
		{"files", "/app/data"}, // duplicate path
		{"files", "relative"},  // not absolute
		{"files", "/"},         // the whole filesystem
		{"files", "/a:/b"},     // bind syntax injection
		{"Bad_Name", "/files"}, // invalid name
	} {
		if err := AddVolume(s, "web", c.name, c.path); err == nil {
			t.Errorf("AddVolume(%q, %q) should fail", c.name, c.path)
		}
	}
	binds, _ := appBinds(s, "web")
	if len(binds) != 1 || binds[0] != "hakobu-vol-web_data:/app/data" {
		t.Errorf("binds = %v", binds)
	}
}
