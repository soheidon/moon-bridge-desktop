package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/runtime"
	"moonbridge/internal/secretstore"
)

type endpointTestCodec struct{ supported bool }

func (c endpointTestCodec) Encrypt(plaintext string) (string, error) {
	if !c.supported {
		return "", secretstore.ErrUnsupported
	}
	if plaintext == "" {
		return "", nil
	}
	return "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (c endpointTestCodec) Decrypt(stored string) (string, error) {
	if !c.supported {
		return "", secretstore.ErrUnsupported
	}
	if stored == "" {
		return "", nil
	}
	if !secretstore.IsCiphertext(stored) {
		return "", secretstore.ErrUnsupported
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "dpapi:v1:"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (c endpointTestCodec) Supported() bool { return c.supported }

func endpointEnvAbsent(string) (string, bool) { return "", false }

func TestGetCredentialStatusEndpoint(t *testing.T) {
	fx := newFixture(t)
	codec := endpointTestCodec{supported: true}
	enc, err := codec.Encrypt("sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	cfg := fx.rt.Current().Config
	def := cfg.ProviderDefs["anthropic"]
	def.APIKey = enc
	cfg.ProviderDefs["anthropic"] = def

	reg := provider.NewCredentialStatusRegistry()
	resolver := &provider.CredentialResolver{Codec: codec, LookupEnv: endpointEnvAbsent, Registry: reg}
	rt := runtime.NewRuntime(cfg, nil, nil, resolver)
	if err := rt.Reload(cfg); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	handler := NewRouter(fx.store, rt, nil, nil, &testServer{rt: rt})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/credentials/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Credentials []provider.CredentialInfo `json:"credentials"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Credentials) != 1 || resp.Credentials[0].ProviderID != "anthropic" {
		t.Fatalf("credentials = %+v, want single anthropic entry", resp.Credentials)
	}
	info := resp.Credentials[0]
	if info.Source != provider.SourceStored || info.State != provider.StateAvailable || info.ErrorCode != "" {
		t.Fatalf("credential = %+v, want stored/available", info)
	}
}

// The endpoint carries source/state/error code only: no key, ciphertext, or
// decrypted value may appear in the wire form.
func TestGetCredentialStatusNeverLeaksSecrets(t *testing.T) {
	fx := newFixture(t)
	codec := endpointTestCodec{supported: true}
	enc, err := codec.Encrypt("sk-super-secret-12345")
	if err != nil {
		t.Fatal(err)
	}
	cfg := fx.rt.Current().Config
	def := cfg.ProviderDefs["anthropic"]
	def.APIKey = enc
	cfg.ProviderDefs["anthropic"] = def

	reg := provider.NewCredentialStatusRegistry()
	resolver := &provider.CredentialResolver{Codec: codec, LookupEnv: endpointEnvAbsent, Registry: reg}
	rt := runtime.NewRuntime(cfg, nil, nil, resolver)
	if err := rt.Reload(cfg); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	handler := NewRouter(fx.store, rt, nil, nil, &testServer{rt: rt})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/credentials/status", nil))
	body := rr.Body.String()
	if strings.Contains(body, "sk-super-secret") || strings.Contains(body, "dpapi:v1:") {
		t.Fatalf("credential status response leaked a secret: %s", body)
	}
}

func TestGetCredentialStatusEmptyWithoutManager(t *testing.T) {
	fx := newFixture(t)
	handler := NewRouter(fx.store, fx.rt, nil, nil, &testServer{rt: fx.rt})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/credentials/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Credentials []provider.CredentialInfo `json:"credentials"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Credentials == nil || len(resp.Credentials) != 0 {
		t.Fatalf("credentials = %#v, want empty non-nil slice", resp.Credentials)
	}
}
