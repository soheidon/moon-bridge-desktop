package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClientTestProvider(t *testing.T) {
	var method, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ConnectionTestResult{
			Success:  true,
			Code:     "ok",
			Message:  "connection succeeded",
			Model:    "claude-sonnet-20241022",
			Duration: "120ms",
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "run-token")
	result, err := c.TestProvider(context.Background(), ProviderID)
	if err != nil {
		t.Fatalf("TestProvider error = %v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
	if path != "/api/v1/providers/"+ProviderID+"/test" {
		t.Fatalf("path = %q, want /api/v1/providers/%s/test", path, ProviderID)
	}
	if auth != "Bearer run-token" {
		t.Fatalf("authorization = %q, want Bearer run-token", auth)
	}
	if !result.Success || result.Code != "ok" || result.Model != "claude-sonnet-20241022" {
		t.Fatalf("result = %+v, want ok/claude-sonnet-20241022", result)
	}
	if result.Message == "" || result.Duration == "" {
		t.Fatalf("result = %+v, want message/duration populated", result)
	}
}

func TestHTTPClientTestProviderErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	_, err := c.TestProvider(context.Background(), ProviderID)
	if err == nil {
		t.Fatal("TestProvider error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %q, want status 500 mentioned", err)
	}
	if strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %q, leaked upstream body", err)
	}
}

func TestHTTPClientTestProviderMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	if _, err := c.TestProvider(context.Background(), ProviderID); err == nil {
		t.Fatal("TestProvider error = nil, want decode error")
	}
}
