package cmd

import (
	"errors"
	"os"
	"testing"
)

// withStdin runs f with stdin reading input, as under ssh without -t.
func withStdin(t *testing.T, input string, f func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()
	f()
}

func TestReadSecretWithoutTerminal(t *testing.T) {
	withStdin(t, "tok-123\n", func() {
		if got, err := readSecret(); err != nil || got != "tok-123\n" {
			t.Errorf("readSecret() = %q, %v; want the piped line", got, err)
		}
	})
	withStdin(t, "", func() {
		if _, err := readSecret(); !errors.Is(err, errNoSecret) {
			t.Errorf("readSecret() on empty stdin = %v, want errNoSecret", err)
		}
	})
}
