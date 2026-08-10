package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"

	"moonbridge/internal/protocol/openai"
)

// These integration tests exercise the inbound request decoder. They all fail
// during decode (before model routing), so no Provider/ProviderMgr is needed.

func zstdEncode(t *testing.T, plain []byte) []byte {
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

func doResponses(t *testing.T, path string, body []byte, encoding string) (int, string) {
	t.Helper()
	handler := New(Config{})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder.Code, recorder.Body.String()
}

func assertBodyErrorCode(t *testing.T, status int, body string, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d, body = %s", status, wantStatus, body)
	}
	var resp openai.ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if resp.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q (body %q)", resp.Error.Code, wantCode, body)
	}
}

// I1: malformed zstd body over /responses is rejected as invalid_request_body.
func TestResponsesZstdMalformed(t *testing.T) {
	status, body := doResponses(t, "/responses", []byte(`{"model":"not-zstd"}`), "zstd")
	assertBodyErrorCode(t, status, body, http.StatusBadRequest, "invalid_request_body")
}

// I2: same malformed zstd over /v1/responses — both paths share one decoder.
func TestV1ResponsesZstdMalformed(t *testing.T) {
	status, body := doResponses(t, "/v1/responses", []byte(`{"model":"not-zstd"}`), "zstd")
	assertBodyErrorCode(t, status, body, http.StatusBadRequest, "invalid_request_body")
}

// I3: an unsupported single encoding is explicitly rejected.
func TestResponsesUnsupportedEncoding(t *testing.T) {
	for _, enc := range []string{"gzip", "br", "deflate"} {
		status, body := doResponses(t, "/responses", []byte(`{"model":"gpt-test"}`), enc)
		assertBodyErrorCode(t, status, body, http.StatusBadRequest, "unsupported_content_encoding")
	}
}

// I4: a zstd body that decompresses beyond the decoded cap is rejected as
// request_too_large.
func TestResponsesZstdDecodedTooLarge(t *testing.T) {
	// Highly compressible data that decompresses past the decoded cap while its
	// encoded form stays under the encoded cap.
	big := bytes.Repeat([]byte("A"), maxDecodedRequestBody+1)
	status, body := doResponses(t, "/responses", zstdEncode(t, big), "zstd")
	assertBodyErrorCode(t, status, body, http.StatusBadRequest, "request_too_large")
}

// I5: identity + invalid JSON still returns invalid_json (existing contract kept).
func TestResponsesIdentityInvalidJSON(t *testing.T) {
	status, body := doResponses(t, "/responses", []byte(`{malformed`), "identity")
	assertBodyErrorCode(t, status, body, http.StatusBadRequest, "invalid_json")
}

// I6: zstd that decodes then fails JSON validation yields invalid_json (decode
// succeeded, decode-to-JSON boundary).
func TestResponsesZstdInvalidJSON(t *testing.T) {
	status, body := doResponses(t, "/responses", zstdEncode(t, []byte(`{malformed`)), "zstd")
	assertBodyErrorCode(t, status, body, http.StatusBadRequest, "invalid_json")
}

// I7: an encoded body larger than the encoded cap is rejected as too large even
// when it is not a valid zstd frame (the bound is enforced at read time).
func TestResponsesZstdEncodedTooLarge(t *testing.T) {
	// Non-zstd payload bigger than the encoded cap; the bounded read must detect
	// the overage before attempting/trusting a zstd decode.
	big := bytes.Repeat([]byte("x"), maxEncodedZstdRequestBody+1)
	status, body := doResponses(t, "/responses", big, "zstd")
	assertBodyErrorCode(t, status, body, http.StatusBadRequest, "request_too_large")
}

// I8: a multi-value Content-Encoding header is rejected as unsupported.
func TestResponsesMultiContentEncoding(t *testing.T) {
	handler := New(Config{})
	req := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(zstdEncode(t, []byte(`{"model":"gpt-test"}`))))
	req.Header.Add("Content-Encoding", "gzip")
	req.Header.Add("Content-Encoding", "zstd")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assertBodyErrorCode(t, recorder.Code, recorder.Body.String(), http.StatusBadRequest, "unsupported_content_encoding")

	req = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(zstdEncode(t, []byte(`{"model":"gpt-test"}`))))
	req.Header.Set("Content-Encoding", "gzip, zstd")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assertBodyErrorCode(t, recorder.Code, recorder.Body.String(), http.StatusBadRequest, "unsupported_content_encoding")
}
