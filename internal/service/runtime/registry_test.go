package runtime_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/runtime"
	"moonbridge/internal/secretstore"
)

type registryTestCodec struct{}

func (registryTestCodec) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (registryTestCodec) Decrypt(stored string) (string, error) {
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

func (registryTestCodec) Supported() bool { return true }

func envAbsent(string) (string, bool) { return "", false }

// runtimeConfigWithKey builds a valid transform-mode config whose provider
// carries the given stored API key value.
func runtimeConfigWithKey(key string) config.Config {
	return config.Config{
		Mode:         config.ModeTransform,
		Addr:         "127.0.0.1:38440",
		DefaultModel: "test-model",
		Routes: map[string]config.RouteEntry{
			"test": {Provider: "default", Model: "claude-test"},
		},
		ProviderDefs: map[string]config.ProviderDef{
			"default": {
				BaseURL: "https://api.anthropic.test",
				APIKey:  key,
				Models:  map[string]config.ModelMeta{"claude-test": {ContextWindow: 100000}},
			},
		},
		Cache: config.CacheConfig{Mode: "off"},
	}
}

// A committed Reload rebuilds the provider manager with the runtime's injected
// resolver, so the credential status registry is re-recorded (decrypt → stored/
// available) rather than left stale.
func TestReloadReRecordsRegistryThroughResolver(t *testing.T) {
	codec := registryTestCodec{}
	enc, err := codec.Encrypt("sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	reg := provider.NewCredentialStatusRegistry()
	resolver := &provider.CredentialResolver{Codec: codec, LookupEnv: envAbsent, Registry: reg}

	cfg := runtimeConfigWithKey(enc)
	rt := runtime.NewRuntime(cfg, nil, nil, resolver)

	if err := rt.Reload(cfg); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	info, ok := reg.Get("default")
	if !ok {
		t.Fatal("registry has no entry after reload")
	}
	if info.Source != provider.SourceStored || info.State != provider.StateAvailable || info.ErrorCode != "" {
		t.Fatalf("registry info = %+v, want stored/available", info)
	}
}

// ValidateCandidate validates a draft without committing it. It must not churn
// the shared registry with draft-derived statuses.
func TestValidateCandidateDoesNotChurnRegistry(t *testing.T) {
	codec := registryTestCodec{}
	enc, err := codec.Encrypt("sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	reg := provider.NewCredentialStatusRegistry()
	resolver := &provider.CredentialResolver{Codec: codec, LookupEnv: envAbsent, Registry: reg}

	cfg := runtimeConfigWithKey(enc)
	rt := runtime.NewRuntime(cfg, nil, nil, resolver)
	if err := rt.Reload(cfg); err != nil {
		t.Fatalf("seed Reload() error = %v", err)
	}

	draft := runtimeConfigWithKey("sk-draft-plaintext")
	if err := rt.ValidateCandidate(draft); err != nil {
		t.Fatalf("ValidateCandidate() error = %v", err)
	}
	info, ok := reg.Get("default")
	if !ok {
		t.Fatal("registry entry lost after candidate validation")
	}
	if info.ErrorCode != "" {
		t.Fatalf("candidate validation churned registry: %+v", info)
	}
	if info.Source != provider.SourceStored || info.State != provider.StateAvailable {
		t.Fatalf("registry info = %+v, want untouched stored/available", info)
	}
}
