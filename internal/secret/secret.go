// Package secret encrypts secrets at rest in hakobu's database with
// AES-256-GCM. The key lives in data/master.key (or HAKOBU_MASTER_KEY), so a
// copy of the database alone — a backup, a stray download — reveals nothing.
// It doesn't help against someone who can read the whole data directory.
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

var (
	mu   sync.RWMutex
	aead cipher.AEAD
	key  []byte
)

// LoadKey reads the master key from HAKOBU_MASTER_KEY (base64) or path,
// creating path with a new random key if neither exists. A process uses one
// key: loading a different one later is an error.
func LoadKey(path string) error {
	k, err := readKey(path)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if key != nil {
		if string(key) != string(k) {
			return fmt.Errorf("%s holds a different master key than the one already in use", path)
		}
		return nil
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return err
	}
	if aead, err = cipher.NewGCM(block); err != nil {
		return err
	}
	key = k
	return nil
}

func readKey(path string) ([]byte, error) {
	if v := os.Getenv("HAKOBU_MASTER_KEY"); v != "" {
		return decodeKey(v, "HAKOBU_MASTER_KEY")
	}
	b, err := os.ReadFile(path)
	if err == nil {
		return decodeKey(strings.TrimSpace(string(b)), path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	mu.RLock()
	k := key
	mu.RUnlock()
	if k == nil {
		k = make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}
	}
	// O_EXCL: never overwrite a key another process just wrote.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	return k, f.Close()
}

func decodeKey(s, source string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("%s must be 32 bytes, base64-encoded", source)
	}
	return k, nil
}

func cipherOrErr() (cipher.AEAD, error) {
	mu.RLock()
	defer mu.RUnlock()
	if aead == nil {
		return nil, errors.New("secret: master key not loaded")
	}
	return aead, nil
}

// IsEncrypted reports whether a stored value is already encrypted.
func IsEncrypted(stored string) bool { return strings.HasPrefix(stored, prefix) }

// Encrypt seals plain; an empty string stays empty so "not set" checks keep
// working on the raw column.
func Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	a, err := cipherOrErr()
	if err != nil {
		return "", err
	}
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
	a, err := cipherOrErr()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil || len(raw) < a.NonceSize() {
		return "", errors.New("secret: corrupt value")
	}
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], nil)
	if err != nil {
		return "", errors.New("secret: can't decrypt, wrong master key?")
	}
	return string(plain), nil
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
