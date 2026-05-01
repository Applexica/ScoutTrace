// Package creds resolves credential refs into secret material with a
// well-defined precedence order: env > keychain > encfile.
//
// The MVP build ships:
//   - EnvStore  (always available)
//   - EncFileStore (NaCl-style not in stdlib, so the MVP uses a
//     scrypt+AES-GCM scheme keyed by a passphrase)
//   - KeychainStore (stub that returns ErrUnavailable on this platform)
//
// Production adds an OS-specific keychain implementation that wraps the
// platform API. The interface is set up to make that drop-in.
package creds

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Errors.
var (
	ErrUnavailable = errors.New("creds: backend unavailable on this platform")
	ErrNotFound    = errors.New("creds: ref not found")
	ErrBadRef      = errors.New("creds: invalid ref")
)

// Store is the storage interface implemented by each backend.
type Store interface {
	// Get resolves ref into a secret.
	Get(ref string) (string, error)
	// Put stores a secret under ref.
	Put(ref string, value string) error
	// Delete removes a secret under ref.
	Delete(ref string) error
	// List returns all stored refs (best-effort).
	List() ([]string, error)
}

// MultiStore tries backends in the order:
//  1. env      (env://VAR)
//  2. keychain (keychain://...)
//  3. encfile  (encfile://name)
//
// Get picks the backend by ref scheme. It does NOT fall through to other
// backends — the ref's scheme dictates which store handles the lookup.
type MultiStore struct {
	Env      *EnvStore
	Keychain Store
	EncFile  Store
}

// NewMultiStore returns a MultiStore with default Env / placeholder
// keychain / nil encfile.
func NewMultiStore() *MultiStore {
	return &MultiStore{
		Env:      &EnvStore{},
		Keychain: &keychainStub{},
	}
}

// Resolve parses ref and returns the secret. Implements destinations.Resolver.
func (m *MultiStore) Resolve(ref string) (string, error) {
	scheme, key, err := splitRef(ref)
	if err != nil {
		return "", err
	}
	switch scheme {
	case "env":
		return m.Env.Get(key)
	case "keychain":
		if m.Keychain == nil {
			return "", ErrUnavailable
		}
		return m.Keychain.Get(key)
	case "encfile":
		if m.EncFile == nil {
			return "", ErrUnavailable
		}
		return m.EncFile.Get(key)
	}
	return "", ErrBadRef
}

// Get is an alias for Resolve.
func (m *MultiStore) Get(ref string) (string, error) { return m.Resolve(ref) }

// Put stores a secret under the given ref.
func (m *MultiStore) Put(ref, value string) error {
	scheme, key, err := splitRef(ref)
	if err != nil {
		return err
	}
	switch scheme {
	case "env":
		return errors.New("creds: env store is read-only; set the env var directly")
	case "keychain":
		if m.Keychain == nil {
			return ErrUnavailable
		}
		return m.Keychain.Put(key, value)
	case "encfile":
		if m.EncFile == nil {
			return ErrUnavailable
		}
		return m.EncFile.Put(key, value)
	}
	return ErrBadRef
}

// Delete removes a secret.
func (m *MultiStore) Delete(ref string) error {
	scheme, key, err := splitRef(ref)
	if err != nil {
		return err
	}
	switch scheme {
	case "keychain":
		if m.Keychain == nil {
			return ErrUnavailable
		}
		return m.Keychain.Delete(key)
	case "encfile":
		if m.EncFile == nil {
			return ErrUnavailable
		}
		return m.EncFile.Delete(key)
	}
	return ErrBadRef
}

func splitRef(ref string) (scheme, key string, err error) {
	idx := strings.Index(ref, "://")
	if idx <= 0 {
		return "", "", fmt.Errorf("%w: missing scheme in %q", ErrBadRef, ref)
	}
	return ref[:idx], ref[idx+3:], nil
}

// EnvStore is a read-only Store backed by os environment variables.
type EnvStore struct{}

// Get returns the env variable value or ErrNotFound.
func (e *EnvStore) Get(key string) (string, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", fmt.Errorf("%w: $%s", ErrNotFound, key)
	}
	return v, nil
}

// Put always errors — env vars are external state.
func (e *EnvStore) Put(_, _ string) error { return errors.New("creds: env store is read-only") }

// Delete always errors.
func (e *EnvStore) Delete(_ string) error { return errors.New("creds: env store is read-only") }

// List returns env keys with the SCOUTTRACE_ prefix as a hint.
func (e *EnvStore) List() ([]string, error) {
	var out []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := kv[:eq]
		if strings.HasPrefix(k, "SCOUTTRACE_") {
			out = append(out, k)
		}
	}
	return out, nil
}

// keychainStub returns ErrUnavailable for all operations. Production
// builds replace this with an OS-specific implementation.
type keychainStub struct{}

func (*keychainStub) Get(string) (string, error) { return "", ErrUnavailable }
func (*keychainStub) Put(string, string) error   { return ErrUnavailable }
func (*keychainStub) Delete(string) error        { return ErrUnavailable }
func (*keychainStub) List() ([]string, error)    { return nil, nil }
