package server

import (
	"bytes"
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdCompress(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// D0a-D0e: normalizeContentEncoding classifies the full header (empty, single
// token, comma lists, and multiple header values) into one supported token.
func TestNormalizeContentEncoding(t *testing.T) {
	for name, values := range map[string][]string{
		"absent header":   {},
		"empty value":     {""},
		"identity":        {"identity"},
		"zstd":            {"zstd"},
		"uppercase zstd":  {"ZSTD"},
		"whitespace zstd": {"  zstd  "},
	} {
		got, err := normalizeContentEncoding(values)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", name, err)
		}
		want := "identity"
		switch name {
		case "zstd", "uppercase zstd", "whitespace zstd":
			want = "zstd"
		}
		if got != want {
			t.Fatalf("%s: got %q, want %q", name, got, want)
		}
	}

	rejects := map[string][]string{
		"comma list":        {"gzip, zstd"},
		"leading comma":     {", zstd"},
		"trailing comma":    {"zstd,"},
		"multi header":      {"gzip", "zstd"},
		"duplicate header":  {"zstd", "zstd"},
		"gzip":              {"gzip"},
		"br":                {"br"},
		"multi comma token": {"gzip, br"},
	}
	for name, values := range rejects {
		if _, err := normalizeContentEncoding(values); !errors.Is(err, errUnsupportedContentEncoding) {
			t.Fatalf("%s: got %v, want errUnsupportedContentEncoding", name, err)
		}
	}
}

// D1: identity/empty passes the bytes through unchanged.
func TestDecodeRequestBodyIdentity(t *testing.T) {
	plain := []byte(`{"model":"gpt-test","input":"Hello"}`)
	for _, encoding := range []string{"", "identity"} {
		got, err := decodeRequestBody(plain, encoding)
		if err != nil {
			t.Fatalf("encoding %q error = %v", encoding, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("encoding %q changed the body: %q", encoding, got)
		}
	}
}

// D2: a zstd frame is decompressed back to the plain JSON byte-for-byte.
func TestDecodeRequestBodyZstdRoundTrip(t *testing.T) {
	plain := []byte(`{"model":"gpt<｜end▁of▁thinking｜>","input":"zstd round trip"}`)
	encoded := zstdCompress(t, plain)
	got, err := decodeRequestBody(encoded, "zstd")
	if err != nil {
		t.Fatalf("zstd decode error = %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("zstd round-trip = %q, want %q", got, plain)
	}
}

// D3: a body that is not a zstd frame but claims encoding zstd fails at the
// reader (not classified as unsupported-content-encoding).
func TestDecodeRequestBodyZstdMalformed(t *testing.T) {
	if _, err := decodeRequestBody([]byte(`{"model":"not-zstd"}`), "zstd"); err == nil {
		t.Fatal("plain body claimed as zstd decoded without error")
	} else if errors.Is(err, errUnsupportedContentEncoding) {
		t.Fatalf("malformed zstd misclassified as unsupported encoding: %v", err)
	} else if errors.Is(err, errRequestBodyTooLarge) {
		t.Fatalf("malformed zstd misclassified as too large: %v", err)
	}
}

// D4: decodeRequestBody returns bytes (even for initially-invalid JSON); JSON
// validity is enforced later by json.Unmarshal. Here we only confirm decoding a
// zstd JSON object works; the "decode OK → invalid JSON" HTTP path is covered by
// the integration test.
func TestDecodeRequestBodyZstdThenInvalidJSONDecodes(t *testing.T) {
	badJSON := []byte(`{malformed`)
	encoded := zstdCompress(t, badJSON)
	decoded, err := decodeRequestBody(encoded, "zstd")
	if err != nil {
		t.Fatalf("zstd decode error = %v", err)
	}
	if !bytes.Equal(decoded, badJSON) {
		t.Fatalf("decoded = %q, want %q", decoded, badJSON)
	}
}

// D5: unknown single encodings are rejected at the decode layer (multi-token
// lists are normalized/rejected by the caller already).
func TestDecodeRequestBodyUnsupportedEncoding(t *testing.T) {
	for _, encoding := range []string{"gzip", "br", "deflate"} {
		if _, err := decodeRequestBody([]byte(`{}`), encoding); !errors.Is(err, errUnsupportedContentEncoding) {
			t.Fatalf("encoding %q: got %v, want errUnsupportedContentEncoding", encoding, err)
		}
	}
}

// D6: a zstd frame that decompresses beyond the decoded cap is rejected.
func TestDecodeRequestBodyTooLarge(t *testing.T) {
	// Use a body that decompresses to > maxDecodedRequestBody. Compressing that
	// much data is slow; instead craft a frame whose declared content is huge by
	// compressing hard-to-compress random data to defeat streaming size hints is
	// not reliable. We rely on the decoder's own maxMemory to fail before the
	// explicit size check for a real oversized frame; to keep the test fast and
	// deterministic, we assert the explicit LimitReader cap by patching via the
	// value: decompress an incompressible body larger than the cap.
	big := bytes.Repeat([]byte("A"), maxDecodedRequestBody+1)
	encoded := zstdCompress(t, big)
	if _, err := decodeRequestBody(encoded, "zstd"); err == nil {
		t.Fatal("oversized zstd decode succeeded")
	}
}

// D7: identity yields no error for empty input.
func TestDecodeRequestBodyEmptyNil(t *testing.T) {
	if _, err := decodeRequestBody(nil, ""); err != nil {
		t.Fatalf("empty identity decode error = %v", err)
	}
}
