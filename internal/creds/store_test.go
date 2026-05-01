package creds

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvStoreResolve(t *testing.T) {
	t.Setenv("SCOUTTRACE_TEST_TOKEN", "hunter2")
	m := NewMultiStore()
	got, err := m.Resolve("env://SCOUTTRACE_TEST_TOKEN")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("got %q, want hunter2", got)
	}
}

func TestEnvStoreMissing(t *testing.T) {
	os.Unsetenv("SCOUTTRACE_NOPE")
	m := NewMultiStore()
	if _, err := m.Resolve("env://SCOUTTRACE_NOPE"); err == nil {
		t.Fatalf("expected error for missing var")
	}
}

func TestEncFileStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.enc")
	s := NewEncFileStore(path, []byte("correct-horse-battery-staple"))
	if err := s.Put("default", "whs_live_abc123"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := s.Get("default"); err != nil || got != "whs_live_abc123" {
		t.Fatalf("Get: got=%q err=%v", got, err)
	}
	// Wrong passphrase fails to decrypt.
	bad := NewEncFileStore(path, []byte("wrong"))
	if _, err := bad.Get("default"); err == nil {
		t.Fatalf("expected decrypt failure with wrong passphrase")
	}
	// Persistence: a new store with the right passphrase succeeds.
	again := NewEncFileStore(path, []byte("correct-horse-battery-staple"))
	if got, err := again.Get("default"); err != nil || got != "whs_live_abc123" {
		t.Fatalf("Get after reopen: got=%q err=%v", got, err)
	}
}

func TestRefSchemeDispatch(t *testing.T) {
	if _, _, err := splitRef("env://X"); err != nil {
		t.Errorf("env://X failed: %v", err)
	}
	if _, _, err := splitRef("noscheme"); err == nil {
		t.Errorf("noscheme should fail")
	}
	if _, _, err := splitRef("keychain://scouttrace/x"); err != nil {
		t.Errorf("keychain://... failed: %v", err)
	}
}

func TestKeychainStubReturnsUnavailable(t *testing.T) {
	m := NewMultiStore()
	if _, err := m.Resolve("keychain://scouttrace/x"); err == nil {
		t.Fatalf("keychain stub should return ErrUnavailable")
	}
}
