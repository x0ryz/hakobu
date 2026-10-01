// Package secret encrypts secrets at rest in hakobu's database with
// AES-256-GCM. The key lives in data/master.key (or HAKOBU_MASTER_KEY), so a
// copy of the database alone — a backup, a stray download — reveals nothing.
// It doesn't help against someone who can read the whole data directory.
//
// The key can be rotated: the new key goes to master.key.new and encrypts
// from then on while the old one still decrypts, the store re-encrypts
// every secret, and master.key.new replaces master.key. A rotation cut short
// is finished the next time the key is loaded.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// prefix marks an encrypted value and its format.
const prefix = "enc:v1:"

const envKey = "HAKOBU_MASTER_KEY"

type key struct {
	raw  []byte
	aead cipher.AEAD
}

var (
	mu sync.RWMutex
	// keys[0] encrypts; any of them decrypts. There are two only while a
	// rotation is in progress.
	keys []key
)

func newKey(raw []byte) (key, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return key{}, err
	}
	aead, err := cipher.NewGCM(block)
	return key{raw, aead}, err
}

// LoadKey reads the master key from HAKOBU_MASTER_KEY (base64) or path,
// creating path with a new random key if neither exists, plus the key of an
// unfinished rotation (path.new). A process uses one key: loading a
// different one later is an error.
func LoadKey(path string) error {
	primary, err := readKey(path)
	if err != nil {
		return err
	}
	loaded := [][]byte{primary}
	if os.Getenv(envKey) == "" {
		next, err := readFile(path + ".new")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if next != nil {
			loaded = [][]byte{next, primary}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if keys != nil {
		if string(keys[0].raw) != string(loaded[0]) {
			return fmt.Errorf("%s holds a different master key than the one already in use", path)
		}
		return nil
	}
	return setKeys(loaded)
}

func setKeys(raw [][]byte) error {
	ks := make([]key, len(raw))
	for i, r := range raw {
		k, err := newKey(r)
		if err != nil {
			return err
		}
		ks[i] = k
	}
	keys = ks
	return nil
}

// Rotating reports whether a rotation is in progress: secrets may be
// encrypted with either key until the store has re-encrypted them.
func Rotating() bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(keys) > 1
}

// BeginRotation makes a new key encrypt from now on, saved as path.new,
// while the current one still decrypts.
func BeginRotation(path string) error {
	if os.Getenv(envKey) != "" {
		return fmt.Errorf("the master key comes from %s: set a new one there and re-encrypt by hand", envKey)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 1 {
		return errors.New("secret: a key rotation is already in progress")
	}
	next := make([]byte, 32)
	if _, err := rand.Read(next); err != nil {
		return err
	}
	if err := writeKey(path+".new", next); err != nil {
		return err
	}
	return setKeys([][]byte{next, keys[0].raw})
}

// FinishRotation replaces path with path.new once every secret is
// encrypted with the new key; the old key is forgotten.
func FinishRotation(path string) error {
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		return errors.New("secret: no key rotation in progress")
	}
	if err := os.Rename(path+".new", path); err != nil {
		return err
	}
	return setKeys([][]byte{keys[0].raw})
}

func readKey(path string) ([]byte, error) {
	if v := os.Getenv(envKey); v != "" {
		return decodeKey(v, envKey)
	}
	k, err := readFile(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return k, err
	}
	mu.RLock()
	if keys != nil {
		k = keys[0].raw
	}
	mu.RUnlock()
	if k == nil {
		k = make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}
	}
	return k, writeKey(path, k)
}

func readFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeKey(strings.TrimSpace(string(b)), path)
}

// writeKey creates path with k; O_EXCL never overwrites a key another
// process just wrote.
func writeKey(path string, k []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func decodeKey(s, source string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("%s must be 32 bytes, base64-encoded", source)
	}
	return k, nil
}

func loadedKeys() ([]key, error) {
	mu.RLock()
	defer mu.RUnlock()
	if keys == nil {
		return nil, errors.New("secret: master key not loaded")
	}
	return keys, nil
}

// KeyMissing reports whether path holds no key, none comes from
// HAKOBU_MASTER_KEY and none is loaded yet: LoadKey would make a new one.
func KeyMissing(path string) bool {
	if os.Getenv(envKey) != "" {
		return false
	}
	mu.RLock()
	loaded := keys != nil
	mu.RUnlock()
	if loaded {
		return false
	}
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// IsEncrypted reports whether a stored value is already encrypted.
func IsEncrypted(stored string) bool { return strings.HasPrefix(stored, prefix) }

// Encrypt seals plain; an empty string stays empty so "not set" checks keep
// working on the raw column.
func Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	ks, err := loadedKeys()
	if err != nil {
		return "", err
	}
	a := ks[0].aead
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + base64.StdEncoding.EncodeToString(a.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Decrypt opens a value from Encrypt.
func Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !IsEncrypted(stored) {
		return "", errors.New("secret: value isn't encrypted")
	}
	ks, err := loadedKeys()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	for _, k := range ks {
		n := k.aead.NonceSize()
		if err != nil || len(raw) < n {
			return "", errors.New("secret: corrupt value")
		}
		if plain, err := k.aead.Open(nil, raw[:n], raw[n:], nil); err == nil {
			return string(plain), nil
		}
	}
	return "", errors.New("secret: can't decrypt, wrong master key?")
}

// String is a database column holding a secret: it's encrypted when written
// and decrypted when read, and code uses it like a string.
type String string

func (s String) Value() (driver.Value, error) {
	return Encrypt(string(s))
}

func (s *String) Scan(src any) error {
	var stored string
	switch v := src.(type) {
	case nil:
	case string:
		stored = v
	case []byte:
		stored = string(v)
	default:
		return fmt.Errorf("secret: can't scan %T", src)
	}
	plain, err := Decrypt(stored)
	*s = String(plain)
	return err
}
