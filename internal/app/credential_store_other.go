//go:build !windows

package app

// This file is the non-Windows credential store: saving the 88FRP login is
// explicitly unsupported and the store never degrades to plaintext
// persistence (SPEC point 4).

// credentialStoreSupported reports whether the platform store is available.
// Only Windows DPAPI is supported; every other platform is unsupported.
func credentialStoreSupported() bool {
	return false
}

// encryptCredentials is unsupported off Windows.
func encryptCredentials([]byte) ([]byte, error) {
	return nil, ErrCredentialStoreUnsupported
}

// decryptCredentials is unsupported off Windows.
func decryptCredentials([]byte) ([]byte, error) {
	return nil, ErrCredentialStoreUnsupported
}
