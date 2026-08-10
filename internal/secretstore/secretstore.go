// Package secretstore encrypts provider API keys before they are persisted
// and decrypts them only at the client-generation boundary.
package secretstore

import "errors"

// ErrUnsupported is returned when the platform cannot encrypt secrets
// (e.g. non-Windows builds). Detection of ciphertext still works there.
var ErrUnsupported = errors.New("secret encryption is not supported on this platform")

// SecretCodec encrypts and decrypts stored secrets. Implementations must be
// safe for concurrent use by multiple goroutines.
type SecretCodec interface {
	// Encrypt returns the stored representation of plaintext.
	Encrypt(plaintext string) (string, error)
	// Decrypt returns the plaintext of a stored representation.
	Decrypt(stored string) (string, error)
	// Supported reports whether Encrypt/Decrypt can run on this platform.
	Supported() bool
}

// New returns the platform codec. On non-Windows platforms it still satisfies
// SecretCodec but Encrypt/Decrypt return ErrUnsupported.
func New() SecretCodec {
	return platformCodec()
}

// IsCiphertext reports whether value is already an encrypted secret rather
// than a legacy plaintext API key.
func IsCiphertext(value string) bool {
	return hasCiphertextPrefix(value)
}

// EncryptIfPlaintext encrypts value only when it is non-empty and not already
// ciphertext, so stored secrets are never double-encrypted. Empty values and
// existing ciphertext pass through unchanged. Unsupported codecs fail closed
// instead of returning plaintext to a persistence boundary.
func EncryptIfPlaintext(codec SecretCodec, value string) (string, error) {
	if value == "" || IsCiphertext(value) {
		return value, nil
	}
	if codec == nil || !codec.Supported() {
		return "", ErrUnsupported
	}
	return codec.Encrypt(value)
}
