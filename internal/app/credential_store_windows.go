//go:build windows

package app

import (
	"syscall"
	"unsafe"
)

// This file provides the Windows DPAPI primitive backing the persisted 88FRP
// login. CryptProtectData/CryptUnprotectData with the default CurrentUser
// scope tie the ciphertext to the current Windows user account, and the
// ciphertext lives in a dedicated file separate from settings.json. There is
// no plaintext fallback on any platform (see credential_store_other.go).

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtect   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32.NewProc("CryptUnprotectData")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree      = kernel32.NewProc("LocalFree")
)

// credentialStoreSupported reports whether the platform store is available.
// Windows DPAPI is the only supported backend.
func credentialStoreSupported() bool {
	return true
}

// dataBlob mirrors the Windows DATA_BLOB structure.
type dataBlob struct {
	size uint32
	data *byte
}

// localFree releases a buffer allocated by the system (CryptUnprotectData
// output). The input buffers are Go-allocated and managed by the GC.
func localFree(buffer *byte) {
	if buffer == nil {
		return
	}
	procLocalFree.Call(uintptr(unsafe.Pointer(buffer)))
}

// encryptCredentials protects the plaintext with CurrentUser-scope DPAPI so
// only the same Windows user account can decrypt it.
func encryptCredentials(plaintext []byte) ([]byte, error) {
	var input dataBlob
	if len(plaintext) > 0 {
		input.size = uint32(len(plaintext))
		input.data = &plaintext[0]
	}
	var output dataBlob
	// CryptProtectData(pDataIn, szDataDescr, pOptionalEntropy, pvReserved,
	// pPromptStruct, dwFlags, pDataOut). dwFlags = 0 selects the CurrentUser
	// scope (CRYPTPROTECT_LOCAL_MACHINE is never used) and no UI is ever shown
	// for the local service.
	ok, _, callErr := procCryptProtect.Call(
		uintptr(unsafe.Pointer(&input)),
		0, // szDataDescr
		0, // optional entropy
		0, // reserved
		0, // prompt struct
		0, // flags
		uintptr(unsafe.Pointer(&output)),
	)
	if ok == 0 {
		if callErr != nil {
			return nil, callErr
		}
		return nil, syscall.EINVAL
	}
	defer localFree(output.data)
	if output.size == 0 {
		return nil, nil
	}
	ciphertext := make([]byte, output.size)
	copy(ciphertext, unsafe.Slice(output.data, output.size))
	return ciphertext, nil
}

// decryptCredentials reverses encryptCredentials with the same CurrentUser
// scope. A decryption failure (e.g. the file was copied from another user or
// another machine) surfaces as an error and never falls back to plaintext.
func decryptCredentials(ciphertext []byte) ([]byte, error) {
	var input dataBlob
	if len(ciphertext) > 0 {
		input.size = uint32(len(ciphertext))
		input.data = &ciphertext[0]
	}
	var output dataBlob
	// CryptUnprotectData(pDataIn, ppszDataDescr, pOptionalEntropy, pvReserved,
	// pPromptStruct, dwFlags, pDataOut). The output description pointer is
	// never requested.
	ok, _, callErr := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&input)),
		0, // ppszDataDescr
		0, // optional entropy
		0, // reserved
		0, // prompt struct
		0, // flags
		uintptr(unsafe.Pointer(&output)),
	)
	if ok == 0 {
		if callErr != nil {
			return nil, callErr
		}
		return nil, syscall.EINVAL
	}
	defer localFree(output.data)
	if output.size == 0 {
		return nil, nil
	}
	plaintext := make([]byte, output.size)
	copy(plaintext, unsafe.Slice(output.data, output.size))
	return plaintext, nil
}
