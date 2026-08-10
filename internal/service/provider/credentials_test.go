package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"moonbridge/internal/secretstore"
)

type fakeCodec struct{ supported bool }

func (f fakeCodec) Encrypt(plaintext string) (string, error) {
	if !f.supported {
		return "", secretstore.ErrUnsupported
	}
	if plaintext == "" {
		return "", nil
	}
	return "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (f fakeCodec) Decrypt(stored string) (string, error) {
	if !f.supported {
		return "", secretstore.ErrUnsupported
	}
	if stored == "" {
		return "", nil
	}
	if !secretstore.IsCiphertext(stored) {
		return "", errors.New("not ciphertext")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "dpapi:v1:"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (f fakeCodec) Supported() bool { return f.supported }

func envMap(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := m[name]
		return value, ok
	}
}

func newTestResolver(codec secretstore.SecretCodec, env func(string) (string, bool)) (*CredentialResolver, *CredentialStatusRegistry) {
	reg := NewCredentialStatusRegistry()
	return &CredentialResolver{Codec: codec, LookupEnv: env, Registry: reg}, reg
}

func TestResolverStoredCiphertextDecrypts(t *testing.T) {
	codec := fakeCodec{supported: true}
	enc, err := codec.Encrypt("sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	resolver, reg := newTestResolver(codec, envMap(nil))
	got := resolver.Resolve("deepseek", enc, "DEEPSEEK_API_KEY")
	if got != "sk-secret" {
		t.Fatalf("Resolve(stored ciphertext) = %q, want %q", got, "sk-secret")
	}
	info, ok := reg.Get("deepseek")
	if !ok {
		t.Fatal("registry has no entry for deepseek")
	}
	if info.Source != SourceStored || info.State != StateAvailable || info.ErrorCode != "" {
		t.Fatalf("info = %+v, want stored/available", info)
	}
}

func TestResolverStoredDecryptFailure(t *testing.T) {
	codec := fakeCodec{supported: true}
	resolver, reg := newTestResolver(codec, envMap(nil))
	got := resolver.Resolve("deepseek", "dpapi:v1:!!!not-base64!!!", "DEEPSEEK_API_KEY")
	if got != "" {
		t.Fatalf("Resolve(undecryptable) = %q, want empty", got)
	}
	info, _ := reg.Get("deepseek")
	if info.Source != SourceStored || info.State != StateUnavailable || info.ErrorCode != ErrCodeDecryptFailed {
		t.Fatalf("info = %+v, want stored/unavailable/decrypt_failed", info)
	}
}

func TestResolverLegacyPlaintextRefused(t *testing.T) {
	codec := fakeCodec{supported: true}
	resolver, reg := newTestResolver(codec, envMap(map[string]string{"DEEPSEEK_API_KEY": "env-key"}))
	got := resolver.Resolve("deepseek", "sk-legacy-plaintext", "DEEPSEEK_API_KEY")
	if got != "" {
		t.Fatalf("Resolve(legacy plaintext) = %q, want empty (fail closed)", got)
	}
	info, _ := reg.Get("deepseek")
	if info.Source != SourceStored || info.State != StateUnavailable || info.ErrorCode != ErrCodeMigrationFailed {
		t.Fatalf("info = %+v, want stored/unavailable/migration_failed", info)
	}
}

func TestResolverEnvironmentFallback(t *testing.T) {
	codec := fakeCodec{supported: true}
	resolver, reg := newTestResolver(codec, envMap(map[string]string{"DEEPSEEK_API_KEY": "env-key"}))
	got := resolver.Resolve("deepseek", "", "DEEPSEEK_API_KEY")
	if got != "env-key" {
		t.Fatalf("Resolve(env) = %q, want %q", got, "env-key")
	}
	info, _ := reg.Get("deepseek")
	if info.Source != SourceEnvironment || info.State != StateAvailable {
		t.Fatalf("info = %+v, want environment/available", info)
	}
}

func TestResolverMissing(t *testing.T) {
	codec := fakeCodec{supported: true}
	resolver, reg := newTestResolver(codec, envMap(nil))
	got := resolver.Resolve("deepseek", "", "DEEPSEEK_API_KEY")
	if got != "" {
		t.Fatalf("Resolve(missing) = %q, want empty", got)
	}
	info, _ := reg.Get("deepseek")
	if info.Source != SourceNone || info.State != StateMissing {
		t.Fatalf("info = %+v, want none/missing", info)
	}
}

func TestResolverUnsupportedPlatformStoredRefusedEnvOnly(t *testing.T) {
	codec := fakeCodec{supported: false}
	resolver, reg := newTestResolver(codec, envMap(map[string]string{"DEEPSEEK_API_KEY": "env-key"}))
	if got := resolver.Resolve("deepseek", "dpapi:v1:AAAA", "DEEPSEEK_API_KEY"); got != "" {
		t.Fatalf("Resolve(stored, unsupported) = %q, want empty", got)
	}
	info, _ := reg.Get("deepseek")
	if info.Source != SourceStored || info.State != StateUnavailable || info.ErrorCode != ErrCodeUnsupportedPlatform {
		t.Fatalf("info = %+v, want stored/unavailable/unsupported_platform", info)
	}
	// Environment is still usable on unsupported platforms.
	if got := resolver.Resolve("deepseek", "", "DEEPSEEK_API_KEY"); got != "env-key" {
		t.Fatalf("Resolve(env, unsupported) = %q, want %q", got, "env-key")
	}
}

func TestResolverNilFailsClosed(t *testing.T) {
	var resolver *CredentialResolver
	if got := resolver.Resolve("deepseek", "sk-plain", ""); got != "" {
		t.Fatalf("nil resolver stored = %q, want empty", got)
	}
	if got := resolver.Resolve("deepseek", "dpapi:v1:AAAA", ""); got != "" {
		t.Fatalf("nil resolver ciphertext = %q, want empty", got)
	}
	if got := resolver.Resolve("deepseek", "", "UNSET_VAR_XYZ"); got != "" {
		t.Fatalf("nil resolver env = %q, want empty", got)
	}
}

func TestManagerMigrationIssueBlocksEnvironmentFallback(t *testing.T) {
	calls := 0
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected transport call")
	})
	resolver, _ := newTestResolver(fakeCodec{supported: true}, envMap(map[string]string{
		"DEEPSEEK_API_KEY": "env-key",
	}))
	manager, err := NewSecureProviderManagerWithIssues(
		map[string]ProviderConfig{
			"deepseek": {
				BaseURL:        "https://provider.example.test",
				APIKeyEnv:      "DEEPSEEK_API_KEY",
				ClientOverride: &http.Client{Transport: transport},
			},
		},
		map[string]ModelRoute{
			"model": {Provider: "deepseek", Name: "upstream-model"},
		},
		resolver,
		[]CredentialInfo{{
			ProviderID: "deepseek",
			Source:     SourceStored,
			State:      StateUnavailable,
			ErrorCode:  ErrCodeMigrationFailed,
		}},
	)
	if err != nil {
		t.Fatalf("NewSecureProviderManagerWithIssues() error = %v", err)
	}
	route, err := manager.ResolveModel("model")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}
	if _, err := route.Candidates[0].Client.CreateMessage(context.Background(), nil); err == nil {
		t.Fatal("CreateMessage() error = nil, want migration failure")
	}
	if calls != 0 {
		t.Fatalf("transport calls = %d, want 0", calls)
	}
}

func TestManagerCredentialStatusAccessor(t *testing.T) {
	codec := fakeCodec{supported: true}
	enc, err := codec.Encrypt("sk-ok")
	if err != nil {
		t.Fatal(err)
	}
	resolver, _ := newTestResolver(codec, envMap(nil))
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"default": {BaseURL: "https://example.test", APIKey: enc},
	}, nil, resolver)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	status := pm.CredentialStatus()
	if len(status) != 1 || status[0].ProviderID != "default" || status[0].State != StateAvailable {
		t.Fatalf("CredentialStatus() = %+v, want single available default", status)
	}
}

func TestManagerCredentialStatusNilWithoutResolver(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"default": {BaseURL: "https://example.test", APIKey: "sk-plain"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	if status := pm.CredentialStatus(); status != nil {
		t.Fatalf("CredentialStatus() = %+v, want nil without resolver", status)
	}
}

func TestManagerBuildRecordsStatusesAndClearsStale(t *testing.T) {
	codec := fakeCodec{supported: true}
	enc, err := codec.Encrypt("sk-ok")
	if err != nil {
		t.Fatal(err)
	}
	resolver, reg := newTestResolver(codec, envMap(nil))
	_, err = NewProviderManager(map[string]ProviderConfig{
		"default": {BaseURL: "https://example.test", APIKey: enc},
		"bad":     {BaseURL: "https://example.test", APIKey: "sk-plain-legacy"},
	}, nil, resolver)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	byID := map[string]CredentialInfo{}
	for _, info := range reg.All() {
		byID[info.ProviderID] = info
	}
	if byID["default"].State != StateAvailable {
		t.Fatalf("default = %+v, want available", byID["default"])
	}
	if byID["bad"].ErrorCode != ErrCodeMigrationFailed {
		t.Fatalf("bad = %+v, want migration_failed", byID["bad"])
	}
	// A rebuild with fewer providers drops the removed one's status.
	_, err = NewProviderManager(map[string]ProviderConfig{
		"default": {BaseURL: "https://example.test", APIKey: enc},
	}, nil, resolver)
	if err != nil {
		t.Fatalf("second NewProviderManager() error = %v", err)
	}
	if _, ok := reg.Get("bad"); ok {
		t.Fatal("registry retained status for removed provider")
	}
}
