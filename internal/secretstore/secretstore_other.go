//go:build !windows

package secretstore

import "strings"

const ciphertextPrefix = "dpapi:v1:"

// unavailable is a SecretCodec for platforms without OS-backed secret storage.
// Ciphertext detection still works so fail-closed logic can refuse to run a
// provider whose stored key cannot be decrypted here.
type unavailable struct{}

func platformCodec() SecretCodec { return unavailable{} }

func hasCiphertextPrefix(value string) bool {
	return strings.HasPrefix(value, ciphertextPrefix)
}

// Supported reports whether the platform can encrypt secrets.
func (unavailable) Supported() bool { return false }

func (unavailable) Encrypt(string) (string, error) { return "", ErrUnsupported }

func (unavailable) Decrypt(string) (string, error) { return "", ErrUnsupported }
