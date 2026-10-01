package secret

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var loadOnce sync.Once

// testKey loads one master key for the whole test binary, as a process
// uses one key.
func testKey(t *testing.T) string {
	t.Helper()
	loadOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hakobu-secret")
		if err != nil {
			t.Fatal(err)
		}
		keyPathForTests = filepath.Join(dir, "master.key")
		if err := LoadKey(keyPathForTests); err != nil {
			t.Fatal(err)
		}
	})
	return keyPathForTests
}

var keyPathForTests string

func seal(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewFileWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// Uneven writes, so chunk boundaries fall inside them.
	for p := plain; len(p) > 0; {
		n := min(len(p), 10_000)
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func open(sealed []byte) ([]byte, error) {
	r, err := NewFileReader(bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestSealedFileRoundtrip(t *testing.T) {
	testKey(t)
	for _, size := range []int{0, 1, fileChunk - 1, fileChunk, fileChunk + 1, 3*fileChunk + 5} {
		plain := make([]byte, size)
		rand.Read(plain)
		sealed := seal(t, plain)
		if bytes.Contains(sealed, plain[:min(size, 64)]) && size >= 16 {
			t.Errorf("size %d: plaintext visible in the sealed file", size)
		}
		got, err := open(sealed)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("size %d: roundtrip failed (%v, %d bytes back)", size, err, len(got))
		}
	}
}

func TestSealedFileRejectsTampering(t *testing.T) {
	testKey(t)
	plain := make([]byte, 2*fileChunk+100)
	rand.Read(plain)
	sealed := seal(t, plain)
	header := len(fileMagic) + wrappedKeySize + noncePrefixSize
	chunk := fileChunk + 16

	cases := map[string][]byte{
		"cut at a chunk boundary": sealed[:header+2*chunk],
		"cut inside a chunk":      sealed[:header+chunk+500],
		"last chunk dropped":      sealed[:header+chunk],
		"header only":             sealed[:header],
	}
	flipped := bytes.Clone(sealed)
	flipped[header+chunk+7] ^= 1
	cases["a byte flipped"] = flipped
	swapped := bytes.Clone(sealed)
	copy(swapped[header:], sealed[header+chunk:header+2*chunk])
	copy(swapped[header+chunk:], sealed[header:header+chunk])
	cases["chunks swapped"] = swapped

	for name, bad := range cases {
		if _, err := open(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := open([]byte("plain gzip, not sealed")); err != errNotSealed {
		t.Errorf("unsealed file: %v, want errNotSealed", err)
	}
}

func TestSealedFileSurvivesRotation(t *testing.T) {
	keyPath := testKey(t)
	plain := []byte("pg_dump output")
	path := filepath.Join(t.TempDir(), "app.sql.gz.enc")
	if err := os.WriteFile(path, seal(t, plain), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other.sql.gz.enc")
	if err := os.WriteFile(other, seal(t, plain), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	if err := BeginRotation(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := RewrapFile(path); err != nil {
		t.Fatal(err)
	}
	if err := FinishRotation(keyPath); err != nil {
		t.Fatal(err)
	}

	after, _ := os.ReadFile(path)
	header := len(fileMagic) + wrappedKeySize
	if !bytes.Equal(before[header:], after[header:]) || bytes.Equal(before[:header], after[:header]) {
		t.Error("rewrapping should change only the header")
	}
	if got, err := open(after); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("rewrapped file: %v %q", err, got)
	}
	// A file not rewrapped is sealed with a key that no longer exists.
	stale, _ := os.ReadFile(other)
	if _, err := open(stale); err == nil {
		t.Error("a file sealed with the retired key still opened")
	}
}
