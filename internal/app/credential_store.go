package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrCredentialStoreUnsupported is returned when the current platform cannot
// persist the 88FRP login (non-Windows). The store never degrades to
// plaintext persistence on any platform.
var ErrCredentialStoreUnsupported = errors.New("当前平台不支持保存 88FRP 登录凭据")

// errNoSavedCredentials is the sentinel returned by Load when no credentials
// have been saved yet (or they were cleared). It is an expected state, not a
// corruption error.
var errNoSavedCredentials = errors.New("尚未保存 88FRP 登录凭据")

// storedCredentials is the plaintext form of the persisted 88FRP login. It is
// only ever encrypted on disk; the raw JSON never touches the file system.
type storedCredentials struct {
	ServiceURL string `json:"serviceUrl"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Remember   bool   `json:"remember"`
}

// credentialStore persists the 88FRP login for automatic sign-in after a
// restart. The plaintext is encrypted with the platform store (Windows DPAPI
// CurrentUser scope) and written atomically to a dedicated file next to
// settings.json — never into settings.json, the logs or Web Storage.
//
// The store is deliberately secret-free in its public surface: status
// reporting only asks HasSaved, and Load's result is consumed in memory by
// the automatic login path.
type credentialStore struct {
	mu   sync.Mutex
	path string
}

// newCredentialStore derives the dedicated credentials file path from the
// settings file location so the store survives across restarts. A zero
// settings path yields an unusable store (every operation is unsupported).
func newCredentialStore(settingsPath string) *credentialStore {
	if settingsPath == "" {
		return &credentialStore{}
	}
	return &credentialStore{path: filepath.Join(filepath.Dir(settingsPath), "frp-credentials.bin")}
}

// Save encrypts the credentials with the platform store and atomically
// replaces the credentials file. The on-disk bytes are ciphertext only.
func (store *credentialStore) Save(credentials storedCredentials) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.path == "" || !credentialStoreSupported() {
		return ErrCredentialStoreUnsupported
	}
	if strings.TrimSpace(credentials.Username) == "" {
		return errors.New("缺少 88FRP 用户名")
	}
	plaintext, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	ciphertext, err := encryptCredentials(plaintext)
	if err != nil {
		return fmt.Errorf("加密 88FRP 登录凭据失败：%w", err)
	}
	return store.writeLocked(ciphertext)
}

// Load decrypts and returns the saved credentials. errNoSavedCredentials is
// returned when nothing has been saved; any decryption or parse failure
// surfaces as an error and never falls back to plaintext.
func (store *credentialStore) Load() (storedCredentials, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.path == "" || !credentialStoreSupported() {
		return storedCredentials{}, ErrCredentialStoreUnsupported
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return storedCredentials{}, errNoSavedCredentials
		}
		return storedCredentials{}, err
	}
	plaintext, err := decryptCredentials(data)
	if err != nil {
		return storedCredentials{}, fmt.Errorf("解密 88FRP 登录凭据失败：%w", err)
	}
	var credentials storedCredentials
	if err := json.Unmarshal(plaintext, &credentials); err != nil {
		return storedCredentials{}, fmt.Errorf("88FRP 凭据文件格式无效：%w", err)
	}
	if strings.TrimSpace(credentials.Username) == "" {
		return storedCredentials{}, errNoSavedCredentials
	}
	return credentials, nil
}

// HasSaved reports whether a usable credentials file exists. It never reveals
// which user or service the credentials belong to.
func (store *credentialStore) HasSaved() bool {
	_, err := store.Load()
	return err == nil
}

// Delete removes the credentials file so a later restart does not sign in
// again. Removing a missing file is a no-op.
func (store *credentialStore) Delete() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.path == "" || !credentialStoreSupported() {
		return ErrCredentialStoreUnsupported
	}
	err := os.Remove(store.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// writeLocked atomically replaces the credentials file: the ciphertext is
// written to a temporary file in the same directory, synced, then renamed
// over the target so a crash never leaves a half-written credentials file.
// Callers must hold store.mu.
func (store *credentialStore) writeLocked(ciphertext []byte) error {
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".frp-credentials-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		if temporaryName != "" {
			_ = os.Remove(temporaryName)
		}
	}()
	if _, err := temporary.Write(ciphertext); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryName, 0600); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, store.path); err != nil {
		return err
	}
	temporaryName = ""
	return nil
}
