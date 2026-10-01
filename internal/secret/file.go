package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Sealed files: large data (database snapshots) encrypted for the disk.
// Each file has a random key of its own, kept in the header encrypted with
// the master key, so rotating the master key rewrites only the header. The
// data follows in AES-256-GCM chunks (the STREAM construction): every
// chunk is authenticated, chunks can't be reordered, and a cut-off file is
// detected because only the final chunk is sealed as the last one.
//
//	magic | wrapped file key | nonce prefix (7) | chunk | chunk | ... | last chunk
//
// The wrapped key is Encrypt's output for the base64 file key, so its
// length never changes and the header can be rewritten in place.

const (
	fileMagic       = "hakobu-sealed-v1\n"
	fileChunk       = 64 << 10
	noncePrefixSize = 7
)

// wrappedKeySize is the length of Encrypt's output for a base64 32-byte
// key: prefix + base64(nonce 12 + 44 + tag 16).
var wrappedKeySize = len(prefix) + base64.StdEncoding.EncodedLen(12+44+16)

var errNotSealed = errors.New("secret: not a sealed file")

func fileAEAD(fileKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(prefix []byte, counter uint32, last bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[noncePrefixSize:], counter)
	if last {
		n[11] = 1
	}
	return n
}

type sealWriter struct {
	w       io.Writer
	aead    cipher.AEAD
	prefix  []byte
	counter uint32
	buf     []byte
	closed  bool
}

// NewFileWriter seals what's written to it into w; Close writes the last
// chunk and must be called, or the file reads as cut off.
func NewFileWriter(w io.Writer) (io.WriteCloser, error) {
	fileKey := make([]byte, 32)
	prefix := make([]byte, noncePrefixSize)
	if _, err := rand.Read(fileKey); err != nil {
		return nil, err
	}
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	wrapped, err := Encrypt(base64.StdEncoding.EncodeToString(fileKey))
	if err != nil {
		return nil, err
	}
	if len(wrapped) != wrappedKeySize {
		return nil, fmt.Errorf("secret: wrapped key is %d bytes, want %d", len(wrapped), wrappedKeySize)
	}
	aead, err := fileAEAD(fileKey)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, fileMagic+wrapped); err != nil {
		return nil, err
	}
	if _, err := w.Write(prefix); err != nil {
		return nil, err
	}
	return &sealWriter{w: w, aead: aead, prefix: prefix, buf: make([]byte, 0, fileChunk)}, nil
}

func (s *sealWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("secret: write to a closed sealed file")
	}
	n := 0
	for len(p) > 0 {
		// A full chunk goes out only once more data follows: the last
		// chunk is sealed differently.
		if len(s.buf) == fileChunk {
			if err := s.flush(false); err != nil {
				return n, err
			}
		}
		k := copy(s.buf[len(s.buf):fileChunk], p)
		s.buf = s.buf[:len(s.buf)+k]
		p = p[k:]
		n += k
	}
	return n, nil
}

func (s *sealWriter) flush(last bool) error {
	if s.counter == ^uint32(0) {
		return errors.New("secret: sealed file too large")
	}
	sealed := s.aead.Seal(nil, chunkNonce(s.prefix, s.counter, last), s.buf, nil)
	s.counter++
	s.buf = s.buf[:0]
	_, err := s.w.Write(sealed)
	return err
}

func (s *sealWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

type openReader struct {
	r       io.Reader
	aead    cipher.AEAD
	prefix  []byte
	counter uint32
	plain   []byte
	next    []byte // read ahead: one byte past a chunk tells whether it was the last
	done    bool
}

// NewFileReader opens a sealed file from r, with any loaded master key.
func NewFileReader(r io.Reader) (io.Reader, error) {
	head := make([]byte, len(fileMagic)+wrappedKeySize+noncePrefixSize)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, errNotSealed
	}
	if string(head[:len(fileMagic)]) != fileMagic {
		return nil, errNotSealed
	}
	fileKey, err := unwrapFileKey(string(head[len(fileMagic) : len(fileMagic)+wrappedKeySize]))
	if err != nil {
		return nil, err
	}
	aead, err := fileAEAD(fileKey)
	if err != nil {
		return nil, err
	}
	return &openReader{r: r, aead: aead, prefix: head[len(fileMagic)+wrappedKeySize:]}, nil
}

func unwrapFileKey(wrapped string) ([]byte, error) {
	b64, err := Decrypt(wrapped)
	if err != nil {
		return nil, err
	}
	fileKey, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(fileKey) != 32 {
		return nil, errors.New("secret: corrupt sealed file key")
	}
	return fileKey, nil
}

func (o *openReader) Read(p []byte) (int, error) {
	for len(o.plain) == 0 {
		if o.done {
			return 0, io.EOF
		}
		if err := o.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, o.plain)
	o.plain = o.plain[n:]
	return n, nil
}

func (o *openReader) readChunk() error {
	sealedSize := fileChunk + o.aead.Overhead()
	buf := make([]byte, sealedSize+1)
	n := copy(buf, o.next)
	m, err := io.ReadFull(o.r, buf[n:])
	n += m
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	last := n <= sealedSize
	chunk := buf[:min(n, sealedSize)]
	o.next = nil
	if !last {
		o.next = buf[sealedSize:n]
	}
	plain, err := o.aead.Open(nil, chunkNonce(o.prefix, o.counter, last), chunk, nil)
	if err != nil {
		return errors.New("secret: sealed file is corrupt or cut off")
	}
	o.counter++
	o.plain = plain
	o.done = last
	return nil
}

// RewrapFile re-encrypts the file key in path's header with the current
// master key, during a rotation; the data stays as it is.
func RewrapFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, len(fileMagic)+wrappedKeySize)
	if _, err := io.ReadFull(f, head); err != nil || !bytes.HasPrefix(head, []byte(fileMagic)) {
		return errNotSealed
	}
	fileKey, err := unwrapFileKey(string(head[len(fileMagic):]))
	if err != nil {
		return err
	}
	wrapped, err := Encrypt(base64.StdEncoding.EncodeToString(fileKey))
	if err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(wrapped), int64(len(fileMagic))); err != nil {
		return err
	}
	return f.Sync()
}
