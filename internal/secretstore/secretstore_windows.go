//go:build windows

package secretstore

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const ciphertextPrefix = "dpapi:v1:"

// DPAPI is a SecretCodec backed by Windows Data Protection API bound to the
// current user account.
type DPAPI struct{}

func platformCodec() SecretCodec { return DPAPI{} }

func hasCiphertextPrefix(value string) bool {
	return strings.HasPrefix(value, ciphertextPrefix)
}

// Supported reports whether Windows DPAPI is available.
func (DPAPI) Supported() bool { return true }

// Encrypt protects plaintext with the current user's credentials. The result
// is portable only to the same Windows account on the same machine.
func (DPAPI) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	in, err := newDataBlob(plaintext)
	if err != nil {
		return "", err
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", fmt.Errorf("cryptprotectdata: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return ciphertextPrefix + base64.StdEncoding.EncodeToString(blobBytes(&out)), nil
}

// Decrypt returns the plaintext of a stored representation produced by Encrypt.
func (DPAPI) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !IsCiphertext(stored) {
		return "", fmt.Errorf("secretstore: not ciphertext")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, ciphertextPrefix))
	if err != nil {
		return "", fmt.Errorf("secretstore: decode: %w", err)
	}
	in := &windows.DataBlob{Size: uint32(len(raw))}
	if len(raw) > 0 {
		in.Data = &raw[0]
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", fmt.Errorf("cryptunprotectdata: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(blobBytes(&out)), nil
}

func newDataBlob(s string) (*windows.DataBlob, error) {
	b := []byte(s)
	if len(b) > int(^uint32(0)) {
		return nil, fmt.Errorf("secretstore: value too large")
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}, nil
}

func blobBytes(b *windows.DataBlob) []byte {
	if b.Data == nil {
		return nil
	}
	// #nosec G103 -- DataBlob is a raw C-style allocation owned by the caller.
	return unsafe.Slice(b.Data, int(b.Size))
}
