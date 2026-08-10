package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/routingprofile"
)

// fakeCodec is a deterministic SecretCodec that round-trips a plaintext through
// the dpapi:v1: envelope so tests can prove the probe used the shared resolver's
// decrypted value.
type fakeCodec struct{}

func (fakeCodec) Encrypt(plaintext string) (string, error) {
	return "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (fakeCodec) Decrypt(stored string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "dpapi:v1:"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (fakeCodec) Supported() bool { return true }

func probeTestResolver() *provider.CredentialResolver {
	return &provider.CredentialResolver{
		Codec:    fakeCodec{},
		Registry: provider.NewCredentialStatusRegistry(),
	}
}

func probeStoredKey() string {
	return "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte("sk-ant-test-key-12345678"))
}

func TestHandleTestProviderSuccess(t *testing.T) {
	var mu sync.Mutex
	var gotKey, gotVersion, gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: probeTestResolver(),
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.APIKey = probeStoredKey()
			def.BaseURL = upstream.URL
			cfg.ProviderDefs["anthropic"] = def
		},
	})

	resp := f.request("POST", "/providers/anthropic/test", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var pr probeResult
	f.decode(resp, &pr)
	if !pr.Success || pr.Code != "ok" {
		t.Fatalf("result = %+v, want success/ok", pr)
	}
	if pr.Message == "" || pr.Duration == "" {
		t.Fatalf("result = %+v, want duration/message populated", pr)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotKey != "sk-ant-test-key-12345678" {
		t.Fatalf("x-api-key = %q, want the configured key", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
	if gotModel != "claude-sonnet-20241022" {
		t.Fatalf("probe model = %q, want default route model claude-sonnet-20241022", gotModel)
	}
	if pr.Model != gotModel {
		t.Fatalf("result model = %q, want the probed model %q", pr.Model, gotModel)
	}
}

func TestHandleTestProviderAuthFailed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"invalid api key xyz"}}`)
	}))
	defer upstream.Close()

	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: probeTestResolver(),
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.APIKey = probeStoredKey()
			def.BaseURL = upstream.URL
			cfg.ProviderDefs["anthropic"] = def
		},
	})

	resp := f.request("POST", "/providers/anthropic/test", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (structured result, not upstream status): %s", resp.Code, resp.Body.String())
	}
	var pr probeResult
	f.decode(resp, &pr)
	if pr.Success {
		t.Fatalf("result = %+v, want success=false", pr)
	}
	if pr.Code != "auth_failed" {
		t.Fatalf("code = %q, want auth_failed", pr.Code)
	}
	// The upstream error body must never be echoed back verbatim.
	if strings.Contains(pr.Message, "invalid api key") || strings.Contains(pr.Message, "xyz") {
		t.Fatalf("message leaked upstream body: %q", pr.Message)
	}
	if strings.Contains(resp.Body.String(), "invalid api key") {
		t.Fatalf("response leaked upstream body: %s", resp.Body.String())
	}
}

func TestHandleTestProviderTimeout(t *testing.T) {
	old := probeTimeout
	probeTimeout = 20 * time.Millisecond
	defer func() { probeTimeout = old }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: probeTestResolver(),
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.APIKey = probeStoredKey()
			def.BaseURL = upstream.URL
			cfg.ProviderDefs["anthropic"] = def
		},
	})

	start := time.Now()
	resp := f.request("POST", "/providers/anthropic/test", nil)
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("probe took too long, timeout not honored: %s", time.Since(start))
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var pr probeResult
	f.decode(resp, &pr)
	if pr.Success || pr.Code != "timeout" {
		t.Fatalf("result = %+v, want success=false/timeout", pr)
	}
}

func TestHandleTestProviderCredentialUnavailable(t *testing.T) {
	t.Setenv("MOONBRIDGE_PROBE_UNSET_KEY", "")
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: probeTestResolver(),
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.APIKey = probeStoredKey()
			def.BaseURL = upstream.URL
			def.APIKey = ""
			def.APIKeyEnv = "MOONBRIDGE_PROBE_UNSET_KEY"
			cfg.ProviderDefs["anthropic"] = def
		},
	})

	resp := f.request("POST", "/providers/anthropic/test", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var pr probeResult
	f.decode(resp, &pr)
	if pr.Success || pr.Code != "credential_unavailable" {
		t.Fatalf("result = %+v, want success=false/credential_unavailable", pr)
	}
	if pr.Message == "" {
		t.Fatal("message empty, want actionable wording")
	}
	if hit {
		t.Fatal("upstream hit even though no credential is available")
	}
}

func TestHandleTestProviderModelUnavailable(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	// Keep the config valid with models/routes on a second provider; the probed
	// anthropic provider then has no default route model, no offer, and no model.
	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: probeTestResolver(),
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.APIKey = probeStoredKey()
			def.BaseURL = upstream.URL
			def.Offers = nil
			def.Models = nil
			cfg.ProviderDefs["anthropic"] = def
			cfg.Models["other-model"] = config.ModelDef{ContextWindow: 1000}
			cfg.ProviderDefs["other"] = config.ProviderDef{
				BaseURL:  "https://other.invalid",
				APIKey:   "sk-other-key-12345678",
				Protocol: "anthropic",
				Models:   map[string]config.ModelMeta{"other-model": {ContextWindow: 1000}},
			}
			cfg.Routes = map[string]config.RouteEntry{
				"other": {Provider: "other", Model: "other-model"},
			}
			cfg.Defaults.Model = "other"
		},
	})

	resp := f.request("POST", "/providers/anthropic/test", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var pr probeResult
	f.decode(resp, &pr)
	if pr.Success || pr.Code != "model_unavailable" {
		t.Fatalf("result = %+v, want success=false/model_unavailable", pr)
	}
	if hit {
		t.Fatal("upstream hit even though no probe model is available")
	}
}

// TestHandleTestProviderNoSecretLeak asserts the structured result and error path
// never carry the API key, the Authorization scheme, or the raw upstream body.
func TestHandleTestProviderNoSecretLeak(t *testing.T) {
	const upstreamSecretBody = `{"error":{"type":"authentication_error","message":"sk-ant-secret-123456"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, upstreamSecretBody)
	}))
	defer upstream.Close()

	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: probeTestResolver(),
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.APIKey = probeStoredKey()
			def.BaseURL = upstream.URL
			def.APIKey = probeStoredKey()
			cfg.ProviderDefs["anthropic"] = def
		},
	})

	resp := f.request("POST", "/providers/anthropic/test", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	body := resp.Body.String()
	for _, forbidden := range []string{"sk-ant-test-key-12345678", "sk-ant-secret-123456", "x-api-key", "Authorization", "authentication_error"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
	var pr probeResult
	f.decode(resp, &pr)
	if pr.Code != "auth_failed" {
		t.Fatalf("code = %q, want auth_failed", pr.Code)
	}
	if strings.Contains(pr.Message, "sk-ant-secret") {
		t.Fatalf("message leaked upstream key: %q", pr.Message)
	}
}

// TestHandleTestProviderSharedResolver proves the connection probe resolves the
// credential through the same CredentialResolver the provider manager uses: a
// stored ciphertext is decrypted exactly once and the outcome is recorded in the
// shared registry.
func TestHandleTestProviderMigrationIssueBlocksEnvironmentFallback(t *testing.T) {
	hit := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	resolver := probeTestResolver()
	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: resolver,
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.BaseURL = upstream.URL
			def.APIKey = ""
			def.APIKeyEnv = "DEEPSEEK_API_KEY"
			cfg.ProviderDefs["anthropic"] = def
		},
	})
	resolver.Registry.Set(provider.CredentialInfo{
		ProviderID: "anthropic",
		Source:     provider.SourceStored,
		State:      provider.StateUnavailable,
		ErrorCode:  provider.ErrCodeMigrationFailed,
	})

	resp := f.request("POST", "/providers/anthropic/test", nil)
	var result probeResult
	f.decode(resp, &result)
	if result.Success || result.Code != "credential_unavailable" {
		t.Fatalf("result = %+v, want credential_unavailable", result)
	}
	if hit != 0 {
		t.Fatalf("upstream calls = %d, want 0", hit)
	}
}

func TestHandleTestProviderSharedResolver(t *testing.T) {
	plaintext := "sk-plaintext-resolved-12345678"
	stored := "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext))
	registry := provider.NewCredentialStatusRegistry()
	resolver := &provider.CredentialResolver{
		Codec:     fakeCodec{},
		LookupEnv: func(string) (string, bool) { return "", false },
		Registry:  registry,
	}
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	f := newFixtureWithOptions(t, fixtureOptions{
		resolver: resolver,
		mutateConfig: func(cfg *config.Config) {
			def := cfg.ProviderDefs["anthropic"]
			def.BaseURL = upstream.URL
			def.APIKey = stored
			def.APIKeyEnv = ""
			cfg.ProviderDefs["anthropic"] = def
		},
	})

	resp := f.request("POST", "/providers/anthropic/test", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	if gotKey != plaintext {
		t.Fatalf("x-api-key = %q, want decrypted plaintext %q", gotKey, plaintext)
	}
	info, ok := registry.Get("anthropic")
	if !ok {
		t.Fatal("registry missing anthropic entry")
	}
	if info.Source != provider.SourceStored || info.State != provider.StateAvailable || info.ErrorCode != "" {
		t.Fatalf("registry = %+v, want stored/available", info)
	}
	if strings.Contains(resp.Body.String(), stored) || strings.Contains(resp.Body.String(), plaintext) {
		t.Fatalf("response leaked ciphertext or plaintext: %s", resp.Body.String())
	}
}

func TestPickProbeModelOrdering(t *testing.T) {
	def := config.ProviderDef{
		Models: map[string]config.ModelMeta{
			"zeta": {}, "alpha": {},
		},
		Offers: []config.OfferEntry{
			{Model: "beta"}, {Model: "gamma"}, {Model: "alpha"},
		},
	}
	cfg := config.Config{
		Defaults: config.Defaults{Model: "route"},
		Routes: map[string]config.RouteEntry{
			"route": {Provider: "anthropic", Model: "route-model"},
		},
	}

	t.Run("default route model wins over active profile and offers", func(t *testing.T) {
		model, ok := pickProbeModel(cfg, "anthropic", def, "active-slot-model")
		if !ok || model != "route-model" {
			t.Fatalf("pickProbeModel = %q/%v, want route-model/true", model, ok)
		}
	})

	t.Run("active profile model used when no default route model", func(t *testing.T) {
		otherCfg := cfg
		otherCfg.Defaults.Model = "other-route"
		model, ok := pickProbeModel(otherCfg, "anthropic", def, "active-slot-model")
		if !ok || model != "active-slot-model" {
			t.Fatalf("pickProbeModel = %q/%v, want active-slot-model/true", model, ok)
		}
	})

	t.Run("first sorted offer model when nothing else", func(t *testing.T) {
		model, ok := pickProbeModel(config.Config{}, "anthropic", def, "")
		if !ok || model != "alpha" {
			t.Fatalf("pickProbeModel = %q/%v, want alpha/true (sorted offers)", model, ok)
		}
	})

	t.Run("no model available", func(t *testing.T) {
		empty := config.ProviderDef{}
		model, ok := pickProbeModel(config.Config{}, "anthropic", empty, "")
		if ok || model != "" {
			t.Fatalf("pickProbeModel = %q/%v, want empty/false", model, ok)
		}
	})
}

func TestFirstSortedOfferModelFallsBackToCatalog(t *testing.T) {
	def := config.ProviderDef{
		Models: map[string]config.ModelMeta{"omega": {}, "kappa": {}, "alpha": {}},
	}
	if got := firstSortedOfferModel(def); got != "alpha" {
		t.Fatalf("firstSortedOfferModel = %q, want alpha (sorted catalog keys)", got)
	}
}

func TestDefaultRouteModelForProvider(t *testing.T) {
	cfg := config.Config{
		Defaults: config.Defaults{Model: "route"},
		Routes: map[string]config.RouteEntry{
			"route": {Provider: "anthropic", Model: "route-model"},
			"other": {Provider: "other", Model: "other-model"},
		},
	}
	if got := defaultRouteModelForProvider(cfg, "anthropic"); got != "route-model" {
		t.Fatalf("default route model = %q, want route-model", got)
	}
	// The default route belongs to another provider, so this provider has no
	// default-route model to probe with.
	if got := defaultRouteModelForProvider(cfg, "other"); got != "" {
		t.Fatalf("default route model for other = %q, want empty", got)
	}
	// A default route with an empty model falls back to the route alias.
	aliasCfg := config.Config{
		Defaults: config.Defaults{Model: "fallback"},
		Routes: map[string]config.RouteEntry{
			"fallback": {Provider: "anthropic", Model: ""},
		},
	}
	if got := defaultRouteModelForProvider(aliasCfg, "anthropic"); got != "fallback" {
		t.Fatalf("default route model with empty model = %q, want the alias fallback", got)
	}
}

func TestActiveProfileSlotModel(t *testing.T) {
	graph := configgraph.Graph{Resources: []configgraph.Resource{{
		Kind: configgraph.ResourceExtension,
		ID:   routingprofile.ExtensionResourceID,
		Value: map[string]any{
			"enabled": true,
			"config": map[string]any{
				"active_profile": "custom",
				"profiles": map[string]any{
					"custom": map[string]any{
						"display_name": "Custom",
						"slots": map[string]any{
							"sol":   map[string]any{"provider": "deepseek", "upstream_model": "deepseek-v4-flash"},
							"terra": map[string]any{"provider": "other", "upstream_model": "some-model"},
						},
					},
					"idle": map[string]any{
						"display_name": "Idle",
						"slots": map[string]any{
							"sol": map[string]any{"provider": "deepseek", "upstream_model": "ignored-model"},
						},
					},
				},
			},
		},
	}}}

	if got := activeProfileSlotModel(graph, "deepseek"); got != "deepseek-v4-flash" {
		t.Fatalf("active profile slot model = %q, want deepseek-v4-flash", got)
	}
	// A second slot of the same active profile is also probed for its provider.
	if got := activeProfileSlotModel(graph, "other"); got != "some-model" {
		t.Fatalf("other slot model = %q, want some-model", got)
	}
	// A provider absent from the active profile, or an empty graph, yields "".
	if got := activeProfileSlotModel(graph, "anthropic"); got != "" {
		t.Fatalf("absent provider slot model = %q, want empty", got)
	}
	if got := activeProfileSlotModel(configgraph.Graph{}, "deepseek"); got != "" {
		t.Fatalf("empty graph slot model = %q, want empty", got)
	}
}

func TestClassifyProbeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"success", nil, "ok"},
		{"unauthorized", &anthropic.ProviderError{StatusCode: 401}, "auth_failed"},
		{"forbidden", &anthropic.ProviderError{StatusCode: 403}, "auth_failed"},
		{"rate limited", &anthropic.ProviderError{StatusCode: 429}, "rate_limited"},
		{"model not found", &anthropic.ProviderError{StatusCode: 404}, "model_unavailable"},
		{"request timeout", &anthropic.ProviderError{StatusCode: 408}, "timeout"},
		{"gateway timeout", &anthropic.ProviderError{StatusCode: 504}, "timeout"},
		{"server error", &anthropic.ProviderError{StatusCode: 500}, "general"},
		{"bad request", &anthropic.ProviderError{StatusCode: 400}, "general"},
		{"wrapped provider error", fmt.Errorf("wrapped: %w", &anthropic.ProviderError{StatusCode: 429}), "rate_limited"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, _ := classifyProbeError(tt.err)
			if code != tt.code {
				t.Fatalf("classifyProbeError = %q, want %q", code, tt.code)
			}
		})
	}
}
