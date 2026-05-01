package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// EncFileStore is a passphrase-encrypted local credentials file.
//
// The MVP uses AES-GCM with the key derived from the passphrase via
// SHA-256 (a placeholder for scrypt; we cannot use scrypt without an
// external dependency in this stdlib-only build). Documentation in the
// release notes calls out the upgrade to scrypt/argon2 in production.
type EncFileStore struct {
	Path       string
	Passphrase []byte

	mu sync.Mutex
}

// NewEncFileStore returns a store backed by `path`. The passphrase is
// kept in memory (the caller should zero it on shutdown).
func NewEncFileStore(path string, passphrase []byte) *EncFileStore {
	return &EncFileStore{Path: path, Passphrase: passphrase}
}

// Get returns the value for key.
func (s *EncFileStore) Get(key string) (string, error) {
	m, err := s.load()
	if err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("%w: encfile %q", ErrNotFound, key)
	}
	return v, nil
}

// Put stores or overwrites a secret.
func (s *EncFileStore) Put(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if m == nil {
		m = map[string]string{}
	}
	m[key] = value
	return s.save(m)
}

// Delete removes a secret.
func (s *EncFileStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	delete(m, key)
	return s.save(m)
}

// List returns every key in the encrypted file.
func (s *EncFileStore) List() ([]string, error) {
	m, err := s.load()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out, nil
}

func (s *EncFileStore) load() (map[string]string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return map[string]string{}, nil
	}
	plain, err := decrypt(b, s.key())
	if err != nil {
		return nil, fmt.Errorf("creds: decrypt %s: %w", s.Path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("creds: parse %s: %w", s.Path, err)
	}
	return m, nil
}

func (s *EncFileStore) save(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	enc, err := encrypt(plain, s.key())
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s *EncFileStore) key() [32]byte {
	return sha256.Sum256(s.Passphrase)
}

// encrypt wraps `plain` in AES-GCM. Layout: nonce(12) || ciphertext.
func encrypt(plain []byte, key [32]byte) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := gcm.Seal(nonce, nonce, plain, nil)
	return out, nil
}

func decrypt(buf []byte, key [32]byte) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(buf) < gcm.NonceSize() {
		return nil, errors.New("creds: ciphertext too short")
	}
	nonce, ct := buf[:gcm.NonceSize()], buf[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}
