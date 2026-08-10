//go:build windows

package secretstore

import (
	"strings"
	"testing"
)

func TestDPAPIRoundTrip(t *testing.T) {
	codec := New()
	if !codec.Supported() {
		t.Fatal("DPAPI codec reports unsupported on windows")
	}
	const plaintext = "sk-real-secret-123"
	enc, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !IsCiphertext(enc) {
		t.Fatalf("encrypted value is not marked ciphertext: %q", enc)
	}
	if strings.Contains(enc, plaintext) {
		t.Fatal("ciphertext leaks plaintext")
	}
	dec, err := codec.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if dec != plaintext {
		t.Fatalf("round trip = %q, want %q", dec, plaintext)
	}
}

func TestDPAPIEncryptEmpty(t *testing.T) {
	codec := New()
	got, err := codec.Encrypt("")
	if err != nil || got != "" {
		t.Fatalf("Encrypt(\"\") = %q, %v; want \"\", nil", got, err)
	}
	got, err = codec.Decrypt("")
	if err != nil || got != "" {
		t.Fatalf("Decrypt(\"\") = %q, %v; want \"\", nil", got, err)
	}
}
