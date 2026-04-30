package codex_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"moonbridge/internal/extension/codex"
	"moonbridge/internal/foundation/config"
)

func TestBuildModelInfoFromRouteEnablesApplyPatchFreeform(t *testing.T) {
	info := codex.BuildModelInfoFromRoute("gpt-test", "default", config.RouteEntry{
		ContextWindow: 100000,
	})
	if info.ApplyPatchToolType == nil || *info.ApplyPatchToolType != "freeform" {
		t.Fatalf("apply_patch_tool_type = %v", info.ApplyPatchToolType)
	}
	if info.TruncationPolicy.Mode != "tokens" || info.TruncationPolicy.Limit != codex.DefaultCatalogTruncationLimit {
		t.Fatalf("truncation_policy = %+v", info.TruncationPolicy)
	}
}

func TestBuildModelInfoFromRouteUsesTokenTruncationPolicy(t *testing.T) {
	info := codex.BuildModelInfoFromRoute("gpt-5.2", "default", config.RouteEntry{
		ContextWindow: 200000,
	})
	if info.TruncationPolicy.Mode != "tokens" || info.TruncationPolicy.Limit != codex.DefaultCatalogTruncationLimit {
		t.Fatalf("truncation_policy = %+v", info.TruncationPolicy)
	}
}

func TestBuildModelInfosFromConfigIncludesProviderModelsBeforeRouteFallback(t *testing.T) {
	models := codex.BuildModelInfosFromConfig(config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"openai": {
				BaseURL: "https://api.openai.com",
				APIKey:  "sk-test",
				Models: map[string]config.ModelMeta{
					"gpt-4o": {},
				},
				Protocol: "openai-response",
			},
		},
	})
	if len(models) != 1 {
		t.Fatalf("expected 1 model (model(provider) only), got %d", len(models))
	}
	if models[0].Slug != "gpt-4o(openai)" {
		t.Fatalf("slug[0] = %q, want gpt-4o(openai)", models[0].Slug)
	}
}

func TestBuildModelInfoPreservesReasoningLevels(t *testing.T) {
	info := codex.BuildModelInfoFromProviderModel("deepseek-v4-pro(deepseek)", "deepseek", config.ModelMeta{
		ContextWindow:         200000,
		DefaultReasoningLevel: "high",
		SupportedReasoningLevels: []config.ReasoningLevelPreset{
			{Effort: "high", Description: "High reasoning effort"},
			{Effort: "xhigh", Description: "Extra high reasoning effort"},
		},
	})
	if info.DefaultReasoningLevel != "high" {
		t.Fatalf("DefaultReasoningLevel = %q, want high", info.DefaultReasoningLevel)
	}
	if len(info.SupportedReasoningLevels) != 2 {
		t.Fatalf("SupportedReasoningLevels = %+v, want two levels", info.SupportedReasoningLevels)
	}
	if info.SupportedReasoningLevels[0].Effort != "high" || info.SupportedReasoningLevels[1].Effort != "xhigh" {
		t.Fatalf("SupportedReasoningLevels = %+v", info.SupportedReasoningLevels)
	}
}

func TestGenerateConfigTomlDoesNotSetServiceTier(t *testing.T) {
	var output bytes.Buffer
	err := codex.GenerateConfigToml(&output, "moonbridge", "http://127.0.0.1:38440/v1", "", config.Config{
		Routes: map[string]config.RouteEntry{
			"moonbridge": {
				Provider:      "openai",
				Model:         "gpt-5.4",
				ContextWindow: 200000,
			},
		},
	})
	if err != nil {
		t.Fatalf("GenerateConfigToml() error = %v", err)
	}
	generated := output.String()

	for _, notWant := range []string{"service_tier", "flex"} {
		if strings.Contains(generated, notWant) {
			t.Fatalf("generated config should not contain %q:\n%s", notWant, generated)
		}
	}
	for _, want := range []string{
		`model = "moonbridge"`,
		`model_provider = "moonbridge"`,
		`model_context_window = 200000`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated config missing %q:\n%s", want, generated)
		}
	}
}

func TestGenerateConfigTomlIncludesDeepWikiMCPServer(t *testing.T) {
	var output bytes.Buffer
	err := codex.GenerateConfigToml(&output, "test", "http://127.0.0.1:38440/v1", "", config.Config{
		Routes: map[string]config.RouteEntry{
			"test": {Provider: "openai", Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("GenerateConfigToml() error = %v", err)
	}
	generated := output.String()
	if !strings.Contains(generated, "[mcp_servers.deepwiki]") {
		t.Fatalf("missing deepwiki MCP server in generated config:\n%s", generated)
	}
}


func TestGenerateConfigTomlNormalizesDirectProviderModelRef(t *testing.T) {
	var output bytes.Buffer
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"deepseek": {
				Models: map[string]config.ModelMeta{
					"deepseek-v4-pro": {
						ContextWindow: 200000,
					},
				},
			},
		},
	}
	// No route for "deepseek/deepseek-v4-pro" — it is a direct provider/model reference.
	err := codex.GenerateConfigToml(&output, "deepseek/deepseek-v4-pro", "http://127.0.0.1:38440/v1", "", cfg)
	if err != nil {
		t.Fatalf("GenerateConfigToml() error = %v", err)
	}
	generated := output.String()
	// Should normalize to model(provider) format so Codex can match catalog entries.
	if !strings.Contains(generated, `model = "deepseek-v4-pro(deepseek)"`) {
		t.Fatalf("expected normalized model slug, got:\n%s", generated)
	}
	// Context window should still be resolved from the provider catalog.
	if !strings.Contains(generated, "model_context_window = 200000") {
		t.Fatalf("expected context_window from provider meta, got:\n%s", generated)
	}
}

func TestGenerateConfigTomlModelProviderFormatInputStable(t *testing.T) {
	var output bytes.Buffer
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"deepseek": {
				Models: map[string]config.ModelMeta{
					"deepseek-v4-pro": {
						ContextWindow: 200000,
					},
				},
			},
		},
	}
	// Input already in model(provider) format — should remain unchanged.
	err := codex.GenerateConfigToml(&output, "deepseek-v4-pro(deepseek)", "http://127.0.0.1:38440/v1", "", cfg)
	if err != nil {
		t.Fatalf("GenerateConfigToml() error = %v", err)
	}
	generated := output.String()
	if !strings.Contains(generated, `model = "deepseek-v4-pro(deepseek)"`) {
		t.Fatalf("expected model(provider) slug unchanged, got:\n%s", generated)
	}
}

func TestGenerateConfigTomlRouteAliasWithSlashNotNormalized(t *testing.T) {
	var output bytes.Buffer
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"p1": {
				Models: map[string]config.ModelMeta{
					"model-a": {
						ContextWindow: 1000,
					},
				},
			},
		},
		Routes: map[string]config.RouteEntry{
			"p1/model-a": {Provider: "p1", Model: "model-a", ContextWindow: 1000},
		},
	}
	// p1/model-a is an explicit route alias — should NOT be normalized.
	err := codex.GenerateConfigToml(&output, "p1/model-a", "http://127.0.0.1:38440/v1", "", cfg)
	if err != nil {
		t.Fatalf("GenerateConfigToml() error = %v", err)
	}
	generated := output.String()
	if !strings.Contains(generated, `model = "p1/model-a"`) {
		t.Fatalf("expected route alias preserved as-is, got:\n%s", generated)
	}
}

func TestGenerateConfigTomlCodexHomeContainsNormalizedSlug(t *testing.T) {
	codexHome := t.TempDir()
	var output bytes.Buffer
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"deepseek": {
				Models: map[string]config.ModelMeta{
					"deepseek-v4-pro": {
						ContextWindow: 200000,
					},
				},
			},
		},
	}
	err := codex.GenerateConfigToml(&output, "deepseek/deepseek-v4-pro", "http://127.0.0.1:38440/v1", codexHome, cfg)
	if err != nil {
		t.Fatalf("GenerateConfigToml() error = %v", err)
	}
	// Read the generated catalog and verify the normalized slug is present.
	raw, err := os.ReadFile(codexHome + "/models_catalog.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(raw), `"slug": "deepseek-v4-pro(deepseek)"`) {
		t.Fatalf("catalog missing normalized slug:\n%s", string(raw))
	}
	// The config should reference the normalized slug.
	generated := output.String()
	if !strings.Contains(generated, `model = "deepseek-v4-pro(deepseek)"`) {
		t.Fatalf("config missing normalized model slug:\n%s", generated)
	}
}


func TestWriteModelsCatalogProducesValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/models_catalog.json"
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"openai": {
				BaseURL: "https://api.openai.com",
				APIKey:  "sk-test",
				Models: map[string]config.ModelMeta{
					"gpt-4o": {},
				},
				Protocol: "openai-response",
			},
		},
	}
	if err := codex.WriteModelsCatalog(path, cfg); err != nil {
		t.Fatalf("WriteModelsCatalog() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var result struct {
		Models []codex.ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("JSON unmarshal error = %v", err)
	}
	if len(result.Models) == 0 {
		t.Fatal("expected at least 1 model in catalog")
	}
}

func TestBuildModelInfoFromProviderModelIncludesInputModalities(t *testing.T) {
	info := codex.BuildModelInfoFromProviderModel("gpt-4o(openai)", "openai", config.ModelMeta{
		ContextWindow:   128000,
		InputModalities: []string{"text", "image"},
	})
	if len(info.InputModalities) != 2 {
		t.Fatalf("InputModalities = %v, want [text image]", info.InputModalities)
	}
	if info.InputModalities[0] != "text" || info.InputModalities[1] != "image" {
		t.Fatalf("InputModalities = %v, want [text image]", info.InputModalities)
	}
}

func TestBuildModelInfoDefaultsInputModalitiesToText(t *testing.T) {
	info := codex.BuildModelInfoFromProviderModel("gpt-4o(openai)", "openai", config.ModelMeta{
		ContextWindow: 128000,
		// InputModalities not set; should default to ["text"]
	})
	if len(info.InputModalities) != 1 || info.InputModalities[0] != "text" {
		t.Fatalf("InputModalities = %v, want [text]", info.InputModalities)
	}
	if info.SupportsImageDetailOriginal {
		t.Fatal("SupportsImageDetailOriginal should be false by default")
	}
}

func TestBuildModelInfoFromRoutePropagatesInputModalities(t *testing.T) {
	info := codex.BuildModelInfoFromRoute("gpt-image", "openai", config.RouteEntry{
		Model:                       "gpt-4o",
		InputModalities:             []string{"text", "image"},
		SupportsImageDetailOriginal: true,
	})
	if len(info.InputModalities) != 2 || info.InputModalities[1] != "image" {
		t.Fatalf("InputModalities = %v, want [text image]", info.InputModalities)
	}
	if !info.SupportsImageDetailOriginal {
		t.Fatal("SupportsImageDetailOriginal should be true")
	}
}
func TestDisplayNameFromSlug(t *testing.T) {
	tests := []struct {
		slug string
		want string
	}{
		{"gpt-5.5", "GPT 5.5"},
		{"gpt-5.5-codex", "GPT 5.5 Codex"},
		{"gpt-4o", "GPT 4o"},
		{"deepseek-v4-pro", "Deepseek V4 Pro"},
		{"gp", "Gp"},
		{"g", "G"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			got := codex.DisplayNameFromSlug(tt.slug)
			if got != tt.want {
				t.Fatalf("displayNameFromSlug(%q) = %q, want %q", tt.slug, got, tt.want)
			}
		})
	}
}

func TestBuildModelInfoFromRouteExplicitDisplayName(t *testing.T) {
	info := codex.BuildModelInfoFromRoute("gpt-5.5", "openai", config.RouteEntry{
		DisplayName: "My Custom Name",
	})
	if info.DisplayName != "My Custom Name" {
		t.Fatalf("DisplayName = %q, want My Custom Name", info.DisplayName)
	}
	if info.Slug != "gpt-5.5" {
		t.Fatalf("Slug = %q, want gpt-5.5", info.Slug)
	}
}

func TestBuildModelInfoFromRouteAutoDisplayName(t *testing.T) {
	info := codex.BuildModelInfoFromRoute("gpt-5.5-codex", "openai", config.RouteEntry{})
	if info.DisplayName != "GPT 5.5 Codex" {
		t.Fatalf("DisplayName = %q, want GPT 5.5 Codex", info.DisplayName)
	}
}
