package config

import (
	"os"
	"testing"
)

func TestPrepareDataDir(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("data", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDataDir(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat("data"); fi.Mode().Perm() != 0o700 {
		t.Errorf("data is %v, want 0700", fi.Mode().Perm())
	}
	f, err := os.Create("data/new")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, _ := os.Stat("data/new"); fi.Mode().Perm() != 0o600 {
		t.Errorf("new file is %v, want 0600", fi.Mode().Perm())
	}
}
