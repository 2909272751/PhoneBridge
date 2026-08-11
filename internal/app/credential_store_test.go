package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// requireCredentialStore skips a test on platforms without a supported
// credential store (non-Windows). On Windows the real DPAPI backend runs.
func requireCredentialStore(t *testing.T) {
	t.Helper()
	if !credentialStoreSupported() {
		t.Skip("credential store is unsupported on this platform")
	}
}

// TestCredentialStoreDPAPIRoundtrip verifies the platform encryption returns
// the original plaintext and that the ciphertext leaks none of it (SPEC
// point 10).
func TestCredentialStoreDPAPIRoundtrip(t *testing.T) {
	requireCredentialStore(t)
	plaintext := []byte(`{"serviceUrl":"http://127.0.0.1:8801","username":"alice","password":"correct-password"}`)
	ciphertext, err := encryptCredentials(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(ciphertext) == 0 {
		t.Fatal("encrypt returned empty ciphertext")
	}
	for _, secret := range []string{"alice", "correct-password", "8801"} {
		if bytes.Contains(ciphertext, []byte(secret)) {
			t.Fatalf("ciphertext contains plaintext %q", secret)
		}
	}
	decrypted, err := decryptCredentials(ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Fatalf("roundtrip mismatch: got %q want %q", decrypted, plaintext)
	}
}

func TestCredentialStoreSaveLoadRoundtrip(t *testing.T) {
	requireCredentialStore(t)
	store := newCredentialStore(filepath.Join(t.TempDir(), "settings.json"))
	want := storedCredentials{ServiceURL: "http://127.0.0.1:8801", Username: "alice", Password: "correct-password", Remember: true}
	if err := store.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Fatalf("loaded credentials mismatch: %#v vs %#v", got, want)
	}
	if !store.HasSaved() {
		t.Fatal("HasSaved must be true after a save")
	}
}

// TestCredentialStoreFileContainsNoPlaintext reads the on-disk credentials
// file and asserts it holds no plaintext credential material (SPEC point 10).
func TestCredentialStoreFileContainsNoPlaintext(t *testing.T) {
	requireCredentialStore(t)
	directory := t.TempDir()
	store := newCredentialStore(filepath.Join(directory, "settings.json"))
	credentials := storedCredentials{ServiceURL: "http://127.0.0.1:8801", Username: "alice-秘密", Password: "correct-password-秘密", Remember: true}
	if err := store.Save(credentials); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "frp-credentials.bin"))
	if err != nil {
		t.Fatalf("read credentials file: %v", err)
	}
	for _, secret := range []string{"alice", "correct-password", "秘密", "8801"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("credentials file leaks plaintext %q", secret)
		}
	}
}

func TestCredentialStoreDeleteClearsCredentials(t *testing.T) {
	requireCredentialStore(t)
	store := newCredentialStore(filepath.Join(t.TempDir(), "settings.json"))
	if err := store.Save(storedCredentials{ServiceURL: "http://127.0.0.1:8801", Username: "alice", Password: "pw"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if store.HasSaved() {
		t.Fatal("credentials must be gone after delete")
	}
	// Deleting again is a no-op, not an error.
	if err := store.Delete(); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestCredentialStoreAtomicReplace(t *testing.T) {
	requireCredentialStore(t)
	store := newCredentialStore(filepath.Join(t.TempDir(), "settings.json"))
	first := storedCredentials{ServiceURL: "http://127.0.0.1:8801", Username: "alice", Password: "pw-1"}
	if err := store.Save(first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	second := storedCredentials{ServiceURL: "http://127.0.0.1:8801", Username: "bob", Password: "pw-2"}
	if err := store.Save(second); err != nil {
		t.Fatalf("save second: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("load after replace: %v", err)
	}
	if got != second {
		t.Fatalf("replace failed: got %#v want %#v", got, second)
	}
}

func TestCredentialStoreUnsupportedWithoutPath(t *testing.T) {
	store := newCredentialStore("")
	if err := store.Save(storedCredentials{}); !errors.Is(err, ErrCredentialStoreUnsupported) {
		t.Fatalf("save without path: got %v, want ErrCredentialStoreUnsupported", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrCredentialStoreUnsupported) {
		t.Fatalf("load without path: got %v, want ErrCredentialStoreUnsupported", err)
	}
	if err := store.Delete(); !errors.Is(err, ErrCredentialStoreUnsupported) {
		t.Fatalf("delete without path: got %v, want ErrCredentialStoreUnsupported", err)
	}
}
