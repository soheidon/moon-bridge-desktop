package routingprofile

import (
	"testing"

	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/deepseek"
)

func TestSlotResolverExactMatchOnly(t *testing.T) {
	graph := buildGraphWithActiveProfile(t, deepseek.ProviderID)
	resolver := NewSlotResolver(graph)

	cases := []struct {
		model      string
		wantOK     bool
		wantSlot   string
		wantModel  string
		wantReason *string
	}{
		{model: "gpt-5.6-sol", wantOK: true, wantSlot: SlotSol, wantModel: deepseek.ModelFlash, wantReason: strPtr(deepseek.ReasoningMax)},
		{model: "gpt-5.6-terra", wantOK: true, wantSlot: SlotTerra, wantModel: deepseek.ModelFlash, wantReason: strPtr(deepseek.ReasoningHigh)},
		{model: "gpt-5.6-luna", wantOK: true, wantSlot: SlotLuna, wantModel: deepseek.ModelFlash, wantReason: nil},
		{model: "gpt-5.6-opus", wantOK: false},
		{model: "sol", wantOK: false},
		{model: "gpt-5.6-sol-extra", wantOK: false},
		{model: "GPT-5.6-SOL", wantOK: false},
		{model: "", wantOK: false},
		{model: "deepseek-v4-flash", wantOK: false},
		{model: "moonbridge", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			result, ok := resolver.ResolveSlot(tc.model)
			if ok != tc.wantOK {
				t.Fatalf("ResolveSlot(%q) ok = %v, want %v", tc.model, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if result.ProviderKey != deepseek.ProviderID {
				t.Fatalf("ProviderKey = %q, want %q", result.ProviderKey, deepseek.ProviderID)
			}
			if result.SlotID != tc.wantSlot {
				t.Fatalf("SlotID = %q, want %q", result.SlotID, tc.wantSlot)
			}
			if result.ActiveProfileID != deepseek.ProviderID {
				t.Fatalf("ActiveProfileID = %q, want %q", result.ActiveProfileID, deepseek.ProviderID)
			}
			if result.UpstreamModel != tc.wantModel {
				t.Fatalf("UpstreamModel = %q, want %q", result.UpstreamModel, tc.wantModel)
			}
			if !reasoningEqual(result.Reasoning, tc.wantReason) {
				t.Fatalf("Reasoning = %v, want %v", result.Reasoning, tc.wantReason)
			}
		})
	}
}

func TestSlotResolverNoActiveProfile(t *testing.T) {
	graph := buildGraphWithActiveProfile(t, "")
	resolver := NewSlotResolver(graph)
	_, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if ok {
		t.Fatal("ResolveSlot with empty active profile should return false")
	}
}

func TestSlotResolverUnknownProfile(t *testing.T) {
	graph := buildGraphWithActiveProfile(t, "nonexistent")
	resolver := NewSlotResolver(graph)
	_, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if ok {
		t.Fatal("ResolveSlot with unknown active profile should return false")
	}
}

func TestSlotResolverFromDefaults(t *testing.T) {
	resolver := NewSlotResolverFromDefaults(deepseek.ProviderID)
	result, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if !ok {
		t.Fatal("ResolveSlot should succeed with default table")
	}
	if result.UpstreamModel != deepseek.ModelFlash {
		t.Fatalf("UpstreamModel = %q, want %q", result.UpstreamModel, deepseek.ModelFlash)
	}
	if !reasoningEqual(result.Reasoning, strPtr(deepseek.ReasoningMax)) {
		t.Fatalf("Reasoning = %v, want max", result.Reasoning)
	}
}

func TestSlotResolverFromDefaultsEmptyProfile(t *testing.T) {
	resolver := NewSlotResolverFromDefaults("")
	_, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if ok {
		t.Fatal("ResolveSlot should fail with empty active profile")
	}
}

func TestSlotResolverCustomProfile(t *testing.T) {
	graph := buildGraphWithCustomProfile(t, "custom", map[string]*slotFile{
		SlotSol:   {Provider: "other", UpstreamModel: "custom-model", Reasoning: strPtr("high")},
		SlotTerra: {Provider: "other", UpstreamModel: "custom-model", Reasoning: nil},
		SlotLuna:  {Provider: "other", UpstreamModel: "custom-model", Reasoning: strPtr("low")},
	})
	resolver := NewSlotResolver(graph)

	result, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if !ok {
		t.Fatal("ResolveSlot should succeed with custom profile")
	}
	if result.ProviderKey != "other" {
		t.Fatalf("ProviderKey = %q, want %q", result.ProviderKey, "other")
	}
	if !reasoningEqual(result.Reasoning, strPtr("high")) {
		t.Fatalf("Reasoning = %v, want high", result.Reasoning)
	}

	result, ok = resolver.ResolveSlot("gpt-5.6-terra")
	if !ok {
		t.Fatal("ResolveSlot should succeed for terra")
	}
	if result.Reasoning != nil {
		t.Fatalf("Reasoning = %v, want nil", result.Reasoning)
	}

	result, ok = resolver.ResolveSlot("gpt-5.6-luna")
	if !ok {
		t.Fatal("ResolveSlot should succeed for luna")
	}
	if !reasoningEqual(result.Reasoning, strPtr("low")) {
		t.Fatalf("Reasoning = %v, want low", result.Reasoning)
	}
}

func TestSlotResolverExtensionActiveProfileOverridesRouteProvider(t *testing.T) {
	// Extension has active_profile = "custom", but route provider = "deepseek".
	// Extension side should win.
	table := tableFile{Profiles: map[string]*profileFile{
		deepseek.ProviderID: {
			DisplayName: providerLabel,
			Slots: map[string]*slotFile{
				SlotSol:   {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningMax)},
				SlotTerra: {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningHigh)},
				SlotLuna:  {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: nil},
			},
		},
		"custom": {
			DisplayName: "Custom",
			Slots: map[string]*slotFile{
				SlotSol:   {Provider: "other", UpstreamModel: "custom-model", Reasoning: strPtr("high")},
				SlotTerra: {Provider: "other", UpstreamModel: "custom-model", Reasoning: nil},
				SlotLuna:  {Provider: "other", UpstreamModel: "custom-model", Reasoning: strPtr("low")},
			},
		},
	}}
	cfgValue := extensionConfigValueWithActiveProfile(table, "custom")
	graph := configgraph.Graph{
		Resources: []configgraph.Resource{
			{
				Kind:  configgraph.ResourceExtension,
				ID:    ExtensionResourceID,
				Value: map[string]any{"enabled": true, "config": cfgValue},
			},
			{
				Kind:  configgraph.ResourceRoute,
				ID:    deepseek.RouteID,
				Value: map[string]any{"provider": deepseek.ProviderID, "model": deepseek.ModelFlash, "display_name": "Moon Bridge"},
			},
		},
	}
	resolver := NewSlotResolver(graph)
	if resolver.BootstrapEligible() {
		t.Fatal("resolver should not be bootstrap-eligible when extension has active_profile")
	}
	result, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if !ok {
		t.Fatal("ResolveSlot should succeed")
	}
	// Should use "custom" profile, not "deepseek"
	if result.ProviderKey != "other" {
		t.Fatalf("ProviderKey = %q, want %q (custom profile should win over route provider)", result.ProviderKey, "other")
	}
}

func TestSlotResolverBootstrapOnlyWhenExtensionAbsent(t *testing.T) {
	// No extension resource: bootstrap from route provider.
	graph := configgraph.Graph{
		Resources: []configgraph.Resource{
			{
				Kind:  configgraph.ResourceRoute,
				ID:    deepseek.RouteID,
				Value: map[string]any{"provider": deepseek.ProviderID, "model": deepseek.ModelFlash, "display_name": "Moon Bridge"},
			},
		},
	}
	resolver := NewSlotResolver(graph)
	// Without extension, activeRouteProvider derives from route provider,
	// but active_profile is not set via config, so Empty() should be true
	// (bootstrap path in app.go handles this).
	// Actually, NewSlotResolver sets activeProfileID from activeRouteProvider
	// when extension is absent, so it should work as bootstrap.
	result, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if !ok {
		t.Fatal("bootstrap from route provider should work when extension is absent")
	}
	if result.ProviderKey != deepseek.ProviderID {
		t.Fatalf("ProviderKey = %q, want %q", result.ProviderKey, deepseek.ProviderID)
	}
}

// --- Tests: BootstrapEligible (extension-presence semantics) ---

func TestBootstrapEligible_ExtensionAbsentWithKnownRoute(t *testing.T) {
	table := defaultTable()
	graph := buildGraphFromTable(t, table, deepseek.ProviderID)
	// Remove extension resource to simulate extension-absent.
	graph.Resources = graph.Resources[1:]
	resolver := NewSlotResolver(graph)
	if !resolver.BootstrapEligible() {
		t.Fatal("expected BootstrapEligible()=true when extension absent")
	}
}

func TestBootstrapEligible_ExtensionPresentWithValidActiveProfile(t *testing.T) {
	graph := buildGraphWithActiveProfile(t, deepseek.ProviderID)
	resolver := NewSlotResolver(graph)
	if resolver.BootstrapEligible() {
		t.Fatal("expected BootstrapEligible()=false when extension present with valid active_profile")
	}
}

func TestBootstrapEligible_ExtensionPresentWithMissingActiveProfile(t *testing.T) {
	table := defaultTable()
	graph := configgraph.Graph{
		Resources: []configgraph.Resource{
			{Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Value: map[string]any{
				"config": map[string]any{},
			}},
		},
	}
	graph.Resources = append(graph.Resources, tableResourcesFromTable(table)...)

	resolver := NewSlotResolver(graph)
	if resolver.BootstrapEligible() {
		t.Fatal("expected BootstrapEligible()=false when extension present but active_profile missing")
	}
}

func TestBootstrapEligible_ExtensionPresentWithEmptyActiveProfile(t *testing.T) {
	table := defaultTable()
	graph := configgraph.Graph{
		Resources: []configgraph.Resource{
			{Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Value: map[string]any{
				"config": map[string]any{"active_profile": ""},
			}},
		},
	}
	graph.Resources = append(graph.Resources, tableResourcesFromTable(table)...)

	resolver := NewSlotResolver(graph)
	if resolver.BootstrapEligible() {
		t.Fatal("expected BootstrapEligible()=false when extension present but active_profile=\"\"")
	}
}

func TestBootstrapEligible_ExtensionPresentWithUnknownActiveProfile(t *testing.T) {
	table := defaultTable()
	graph := configgraph.Graph{
		Resources: []configgraph.Resource{
			{Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Value: map[string]any{
				"config": map[string]any{"active_profile": "bogus"},
			}},
		},
	}
	graph.Resources = append(graph.Resources, tableResourcesFromTable(table)...)

	resolver := NewSlotResolver(graph)
	if resolver.BootstrapEligible() {
		t.Fatal("expected BootstrapEligible()=false when extension present but active_profile unknown")
	}
}

func TestBootstrapEligible_ExtensionPresentInvalidActiveProfileWithValidRoute(t *testing.T) {
	table := defaultTable()
	graph := configgraph.Graph{
		Resources: []configgraph.Resource{
			{Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Value: map[string]any{
				"config": map[string]any{"active_profile": "bogus"},
			}},
			{Kind: configgraph.ResourceRoute, ID: deepseek.RouteID, Value: map[string]any{"provider": deepseek.ProviderID}},
		},
	}
	graph.Resources = append(graph.Resources, tableResourcesFromTable(table)...)

	resolver := NewSlotResolver(graph)
	if resolver.BootstrapEligible() {
		t.Fatal("expected BootstrapEligible()=false when extension present even with invalid active_profile+valid route")
	}
	_, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if ok {
		t.Fatal("expected no resolution when extension present with unknown active_profile")
	}
}

// --- helpers ---

func tableResourcesFromTable(table tableFile) []configgraph.Resource {
	// In the real config graph, routing profiles are embedded inside the
	// routing_profiles extension resource's config value, not as standalone
	// resources. For tests that build graph resources manually (without
	// extension), we add the table data as route resources so the graph
	// has something to index. The extension resource itself carries the
	// table data in its config.
	//
	// For these tests, the extension resource already carries the table
	// data in its config, so we just return the route resource for the
	// first profile found.
	var resources []configgraph.Resource
	for id := range table.Profiles {
		resources = append(resources, configgraph.Resource{
			Kind:  configgraph.ResourceRoute,
			ID:    deepseek.RouteID,
			Value: map[string]any{"provider": id},
		})
		break // only need one route for bootstrap tests
	}
	return resources
}

func extensionConfigValueWithActiveProfile(table tableFile, activeProfile string) map[string]any {
	cfg := extensionConfigValue(table)
	cfg["active_profile"] = activeProfile
	return cfg
}

func buildGraphWithActiveProfile(t *testing.T, activeProvider string) configgraph.Graph {
	t.Helper()
	table := defaultTable()
	return buildGraphFromTable(t, table, activeProvider)
}

func buildGraphWithCustomProfile(t *testing.T, profileID string, slots map[string]*slotFile) configgraph.Graph {
	t.Helper()
	table := tableFile{Profiles: map[string]*profileFile{
		profileID: {
			DisplayName: "Custom",
			Slots:       slots,
		},
	}}
	return buildGraphFromTable(t, table, profileID)
}

func buildGraphFromTable(t *testing.T, table tableFile, activeProvider string) configgraph.Graph {
	t.Helper()
	cfgValue := extensionConfigValue(table)
	if activeProvider != "" {
		cfgValue["active_profile"] = activeProvider
	}
	graph := configgraph.Graph{
		Resources: []configgraph.Resource{
			{
				Kind:  configgraph.ResourceExtension,
				ID:    ExtensionResourceID,
				Value: map[string]any{"enabled": true, "config": cfgValue},
			},
		},
	}
	if activeProvider != "" {
		graph.Resources = append(graph.Resources, configgraph.Resource{
			Kind:  configgraph.ResourceRoute,
			ID:    deepseek.RouteID,
			Value: map[string]any{"provider": activeProvider, "model": deepseek.ModelFlash, "display_name": "Moon Bridge"},
		})
	}
	return graph
}
