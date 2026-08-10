package secretstore

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

type fakeCodec struct{ supported bool }

func (f fakeCodec) Encrypt(plaintext string) (string, error) {
	if !f.supported {
		return "", ErrUnsupported
	}
	if plaintext == "" {
		return "", nil
	}
	return ciphertextPrefix + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (f fakeCodec) Decrypt(stored string) (string, error) {
	if !f.supported {
		return "", ErrUnsupported
	}
	if stored == "" {
		return "", nil
	}
	if !IsCiphertext(stored) {
		return "", errors.New("not ciphertext")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, ciphertextPrefix))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (f fakeCodec) Supported() bool { return f.supported }

func TestIsCiphertext(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"empty", "", false},
		{"plaintext", "sk-123", false},
		{"dpapi prefix", "dpapi:v1:AAAA", true},
		{"prefix only", "dpapi:v1:", true},
		{"similar prefix", "dpapi:v2:AAAA", false},
		{"wrong case", "DPAPI:v1:AAAA", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCiphertext(tc.value); got != tc.want {
				t.Fatalf("IsCiphertext(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestEncryptIfPlaintext(t *testing.T) {
	codec := fakeCodec{supported: true}
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"empty unchanged", "", ""},
		{"plaintext encrypted", "sk-123", ciphertextPrefix + "c2stMTIz"},
		{"ciphertext unchanged", ciphertextPrefix + "AAAA", ciphertextPrefix + "AAAA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncryptIfPlaintext(codec, tc.value)
			if err != nil {
				t.Fatalf("EncryptIfPlaintext(%q) error: %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("EncryptIfPlaintext(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestEncryptIfPlaintextNeverDoubleEncrypts(t *testing.T) {
	codec := fakeCodec{supported: true}
	once, err := EncryptIfPlaintext(codec, "sk-123")
	if err != nil {
		t.Fatal(err)
	}
	twice, err := EncryptIfPlaintext(codec, once)
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Fatalf("double-encrypted: got %q from %q", twice, once)
	}
	if !strings.HasPrefix(once, ciphertextPrefix) {
		t.Fatalf("encrypted value lacks prefix: %q", once)
	}
}

func TestFakeCodecRoundTrip(t *testing.T) {
	codec := fakeCodec{supported: true}
	enc, err := codec.Encrypt("sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	dec, err := codec.Decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec != "sk-secret" {
		t.Fatalf("round trip = %q, want %q", dec, "sk-secret")
	}
}

func TestEncryptEmpty(t *testing.T) {
	codec := fakeCodec{supported: true}
	got, err := codec.Encrypt("")
	if err != nil || got != "" {
		t.Fatalf("Encrypt(\"\") = %q, %v; want \"\", nil", got, err)
	}
	got, err = codec.Decrypt("")
	if err != nil || got != "" {
		t.Fatalf("Decrypt(\"\") = %q, %v; want \"\", nil", got, err)
	}
}

func TestUnsupportedCodec(t *testing.T) {
	codec := fakeCodec{supported: false}
	if codec.Supported() {
		t.Fatal("fakeCodec{supported:false}.Supported() = true")
	}
	if _, err := codec.Encrypt("sk"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Encrypt error = %v, want ErrUnsupported", err)
	}
	if _, err := codec.Decrypt(ciphertextPrefix + "AAAA"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Decrypt error = %v, want ErrUnsupported", err)
	}
	// EncryptIfPlaintext leaves plaintext unchanged on unsupported platforms:
	// normalization must not break non-Windows bootstrap, and the resolver
	// refuses to use unsupported stored values.
	got, err := EncryptIfPlaintext(codec, "sk-plain")
	if !errors.Is(err, ErrUnsupported) || got != "" {
		t.Fatalf("EncryptIfPlaintext(unsupported) = %q, %v; want empty/ErrUnsupported", got, err)
	}
}
