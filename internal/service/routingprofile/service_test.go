package routingprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/deepseek"
)

// --- fake ManagementAPI ---

type fakeAPI struct {
	mu        sync.Mutex
	revision  int
	resources []configgraph.Resource
}

func newFakeAPI(resources ...configgraph.Resource) *fakeAPI {
	return &fakeAPI{resources: resources}
}

func (f *fakeAPI) rev() string { return fmt.Sprintf("rev-%d", f.revision) }

func (f *fakeAPI) Graph(context.Context) (configgraph.Graph, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.graphLocked(), nil
}

func (f *fakeAPI) graphLocked() configgraph.Graph {
	return configgraph.Graph{
		Revision:     f.rev(),
		Resources:    cloneResources(f.resources),
		Validation:   configgraph.ValidationState{Valid: true},
		Runtime:      configgraph.RuntimeState{Status: "ok"},
		Capabilities: configgraph.Capabilities{Autosave: true, Logs: true},
	}
}

func (f *fakeAPI) Patch(_ context.Context, req configgraph.PatchRequest) (configgraph.PatchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.BaseRevision != f.rev() {
		return f.conflictLocked(), nil
	}
	for _, op := range req.Changes {
		for i := range f.resources {
			if f.resources[i].Kind == op.Kind && f.resources[i].ID == op.ID {
				setNestedField(f.resources[i].Value, op.Field, op.Value)
			}
		}
	}
	f.revision++
	g := f.graphLocked()
	return configgraph.PatchResponse{Result: configgraph.ResultCommitted, Revision: f.rev(), Graph: &g}, nil
}

// setNestedField writes v at the dotted path field (e.g. "config.active_profile"),
// mirroring the real config graph patch semantics used by reconcileActivateProfile.
func setNestedField(value map[string]any, field string, v any) {
	parts := strings.Split(field, ".")
	cur := value
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = v
}

func (f *fakeAPI) CreateResource(_ context.Context, baseRevision string, kind configgraph.ResourceKind, id string, value map[string]any) (configgraph.PatchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if baseRevision != f.rev() {
		return f.conflictLocked(), nil
	}
	f.resources = append(f.resources, configgraph.Resource{Kind: kind, ID: id, Value: value})
	f.revision++
	g := f.graphLocked()
	return configgraph.PatchResponse{Result: configgraph.ResultCommitted, Revision: f.rev(), Graph: &g}, nil
}

func (f *fakeAPI) conflictLocked() configgraph.PatchResponse {
	return configgraph.PatchResponse{
		Result:   configgraph.ResultRevisionConflict,
		Revision: f.rev(),
		Errors:   []configgraph.FieldError{{Code: "revisionConflict"}},
	}
}

func cloneResources(in []configgraph.Resource) []configgraph.Resource {
	data, _ := json.Marshal(in)
	var out []configgraph.Resource
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

// --- graph fixtures ---

func modelResource(id, reasoning string) configgraph.Resource {
	value := map[string]any{"default_reasoning_level": reasoning}
	if reasoning == "" {
		delete(value, "default_reasoning_level")
	}
	return configgraph.Resource{Kind: configgraph.ResourceModel, ID: id, Value: value}
}

func providerResource(id, apiKey string) configgraph.Resource {
	value := map[string]any{}
	if apiKey != "" {
		value["api_key"] = apiKey
	}
	return configgraph.Resource{Kind: configgraph.ResourceProvider, ID: id, Value: value}
}

func routeResource(model, provider string) configgraph.Resource {
	return configgraph.Resource{Kind: configgraph.ResourceRoute, ID: deepseek.RouteID, Value: map[string]any{"model": model, "provider": provider}}
}

func typicalGraph() []configgraph.Resource {
	return []configgraph.Resource{
		modelResource(deepseek.ModelFlash, deepseek.ReasoningHigh),
		modelResource(deepseek.ModelPro, deepseek.ReasoningHigh),
		providerResource(deepseek.ProviderID, "configured"),
		routeResource(deepseek.ModelFlash, deepseek.ProviderID),
	}
}

// --- snapshot helpers ---

func slotReasoning(slots []Slot, slotID string) *string {
	for _, s := range slots {
		if s.ID == slotID {
			return s.Reasoning
		}
	}
	return nil
}

// --- tests ---

func TestSnapshotFromGraphDefaultsWhenExtensionAbsent(t *testing.T) {
	graph := configgraph.Graph{Resources: typicalGraph()}
	snap := SnapshotFromGraph(graph, true)
	if len(snap.Profiles) != 1 {
		t.Fatalf("want 1 profile, got %d", len(snap.Profiles))
	}
	p := snap.Profiles[0]
	if p.ID != deepseek.ProviderID || !p.Active {
		t.Fatalf("want active deepseek profile, got id=%q active=%v", p.ID, p.Active)
	}
	if !p.Configured {
		t.Fatal("want deepseek profile configured")
	}
	if len(p.Slots) != 3 {
		t.Fatalf("want 3 slots, got %d", len(p.Slots))
	}
	want := map[string]*string{
		SlotSol:   strPtr(deepseek.ReasoningMax),
		SlotTerra: strPtr(deepseek.ReasoningHigh),
		SlotLuna:  nil,
	}
	for _, slot := range p.Slots {
		if slot.ProviderID != deepseek.ProviderID || slot.ProviderLabel != "DeepSeek" || slot.UpstreamModel != deepseek.ModelFlash {
			t.Errorf("slot %s: want deepseek/flash, got %s/%s", slot.ID, slot.ProviderID, slot.UpstreamModel)
		}
		got := slot.Reasoning
		if wantReasoning := want[slot.ID]; !reasoningEqual(got, wantReasoning) {
			t.Errorf("slot %s reasoning: want %v, got %v", slot.ID, strVal(wantReasoning), strVal(got))
		}
	}
	if snap.ActiveProfileID != deepseek.ProviderID {
		t.Errorf("activeProfileId: want %q, got %q", deepseek.ProviderID, snap.ActiveProfileID)
	}
}

func TestSnapshotFromGraphReadsPersistedTable(t *testing.T) {
	reasoning := deepseek.ReasoningMax
	graph := configgraph.Graph{Resources: append(typicalGraph(), configgraph.Resource{
		Kind: configgraph.ResourceExtension,
		ID:   ExtensionResourceID,
		Value: map[string]any{
			"enabled": true,
			"config": map[string]any{"profiles": map[string]any{
				"custom": map[string]any{
					"display_name": "Custom",
					"slots": map[string]any{
						"sol":   map[string]any{"provider": "deepseek", "upstream_model": "deepseek-v4-flash", "reasoning": "max"},
						"terra": map[string]any{"provider": "deepseek", "upstream_model": "deepseek-v4-flash", "reasoning": "high"},
						"luna":  map[string]any{"provider": "deepseek", "upstream_model": "deepseek-v4-flash", "reasoning": nil},
					},
				},
			}},
		},
	})}
	snap := SnapshotFromGraph(graph, true)
	if len(snap.Profiles) != 1 {
		t.Fatalf("want 1 profile, got %d", len(snap.Profiles))
	}
	p := snap.Profiles[0]
	if p.ID != "custom" || p.DisplayName != "Custom" {
		t.Fatalf("want custom profile, got id=%q displayName=%q", p.ID, p.DisplayName)
	}
	if reasoningEqual(slotReasoning(p.Slots, SlotSol), &reasoning) == false {
		t.Errorf("sol reasoning: want max, got %v", strVal(slotReasoning(p.Slots, SlotSol)))
	}
	if slotReasoning(p.Slots, SlotLuna) != nil {
		t.Errorf("luna reasoning: want nil, got %v", strVal(slotReasoning(p.Slots, SlotLuna)))
	}
}

func TestSnapshotFromGraphFailClosedWithoutActiveProfile(t *testing.T) {
	// Extension present without active_profile: fail-closed, no route fallback.
	graph := configgraph.Graph{Resources: append(typicalGraph(), configgraph.Resource{
		Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Value: map[string]any{
			"enabled": true,
			"config": map[string]any{"profiles": map[string]any{
				"deepseek": map[string]any{"display_name": "DeepSeek", "slots": map[string]any{}},
				"other":    map[string]any{"display_name": "Other", "slots": map[string]any{}},
			}},
		},
	})}
	snap := SnapshotFromGraph(graph, true)
	if snap.ActiveProfileID != "" {
		t.Errorf("activeProfileId: want empty (fail-closed), got %q", snap.ActiveProfileID)
	}
	for _, p := range snap.Profiles {
		if p.Active {
			t.Errorf("profile %q must not be active when extension has no active_profile", p.ID)
		}
	}
	// No extension and no route → also empty.
	graphNoRoute := configgraph.Graph{Resources: typicalGraph()[:2]}
	snap2 := SnapshotFromGraph(graphNoRoute, false)
	if snap2.ActiveProfileID != "" {
		t.Errorf("activeProfileId without route: want empty, got %q", snap2.ActiveProfileID)
	}
}

func TestSnapshotActiveProfileSourceOfTruth(t *testing.T) {
	extension := func(active string) configgraph.Resource {
		profiles := map[string]any{
			"openrouter": map[string]any{"display_name": "OpenRouter", "slots": map[string]any{}},
			"deepseek":   map[string]any{"display_name": "DeepSeek", "slots": map[string]any{}},
		}
		cfg := map[string]any{"profiles": profiles}
		if active != "" {
			cfg["active_profile"] = active
		}
		return configgraph.Resource{Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Value: map[string]any{"enabled": true, "config": cfg}}
	}
	route := func(provider string) configgraph.Resource {
		return configgraph.Resource{Kind: configgraph.ResourceRoute, ID: deepseek.RouteID, Value: map[string]any{"model": deepseek.ModelFlash, "provider": provider}}
	}

	t.Run("A extension active_profile wins over route provider", func(t *testing.T) {
		graph := configgraph.Graph{Resources: []configgraph.Resource{extension("openrouter"), route(deepseek.ProviderID)}}
		snap := SnapshotFromGraph(graph, true)
		if snap.ActiveProfileID != "openrouter" {
			t.Errorf("activeProfileId: want openrouter, got %q", snap.ActiveProfileID)
		}
		var active []string
		for _, p := range snap.Profiles {
			if p.Active {
				active = append(active, p.ID)
			}
		}
		if len(active) != 1 || active[0] != "openrouter" {
			t.Errorf("active profiles = %v, want [openrouter]", active)
		}
	})

	t.Run("B extension active_profile deepseek ignores other route provider", func(t *testing.T) {
		graph := configgraph.Graph{Resources: []configgraph.Resource{extension("deepseek"), route("other")}}
		snap := SnapshotFromGraph(graph, true)
		if snap.ActiveProfileID != "deepseek" {
			t.Errorf("activeProfileId: want deepseek, got %q", snap.ActiveProfileID)
		}
	})

	t.Run("C extension present without active_profile does not fall back to route", func(t *testing.T) {
		graph := configgraph.Graph{Resources: []configgraph.Resource{extension(""), route(deepseek.ProviderID)}}
		snap := SnapshotFromGraph(graph, true)
		if snap.ActiveProfileID != "" {
			t.Errorf("activeProfileId: want empty (fail-closed), got %q", snap.ActiveProfileID)
		}
		for _, p := range snap.Profiles {
			if p.Active {
				t.Errorf("profile %q must not be active", p.ID)
			}
		}
	})

	t.Run("D extension absent derives from route provider", func(t *testing.T) {
		graph := configgraph.Graph{Resources: []configgraph.Resource{route(deepseek.ProviderID)}}
		snap := SnapshotFromGraph(graph, true)
		if snap.ActiveProfileID != deepseek.ProviderID {
			t.Errorf("activeProfileId: want deepseek (bootstrap), got %q", snap.ActiveProfileID)
		}
	})
}

func TestSnapshotFromGraphConfiguredFalseWhenProviderMissingKey(t *testing.T) {
	graph := configgraph.Graph{Resources: []configgraph.Resource{
		modelResource(deepseek.ModelFlash, deepseek.ReasoningHigh),
		modelResource(deepseek.ModelPro, deepseek.ReasoningHigh),
		providerResource(deepseek.ProviderID, ""),
		routeResource(deepseek.ModelFlash, deepseek.ProviderID),
	}}
	snap := SnapshotFromGraph(graph, true)
	if snap.Profiles[0].Configured {
		t.Error("want unconfigured profile when provider has no api_key")
	}
}

func TestInputValidate(t *testing.T) {
	valid := Input{Profile: ProfileInput{ID: "deepseek", DisplayName: "DeepSeek", Slots: map[string]SlotInput{
		SlotSol:   {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Reasoning: strPtr("max")},
		SlotTerra: {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Reasoning: strPtr("high")},
		SlotLuna:  {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Reasoning: nil},
	}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}

	missing := valid
	missing.Profile.Slots = map[string]SlotInput{SlotSol: valid.Profile.Slots[SlotSol]}
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("missing slot: want required error, got %v", err)
	}

	badReasoning := valid
	badReasoning.Profile.Slots[SlotSol] = SlotInput{Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Reasoning: strPtr("off")}
	if err := badReasoning.Validate(); err == nil || !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("bad reasoning: want reasoning error, got %v", err)
	}

	noProvider := valid
	noProvider.Profile.Slots[SlotSol] = SlotInput{UpstreamModel: "deepseek-v4-flash", Reasoning: strPtr("max")}
	if err := noProvider.Validate(); err == nil || !strings.Contains(err.Error(), "provider") {
		t.Errorf("empty provider: want provider error, got %v", err)
	}
}

func TestActivateSlotProjectsReasoning(t *testing.T) {
	svc := NewService(newFakeAPI(typicalGraph()...))
	snap, err := svc.ActivateSlot(context.Background(), deepseek.ProviderID, SlotSol)
	if err != nil {
		t.Fatalf("activate sol: %v", err)
	}
	// ActivateSlot creates the extension without active_profile, so under the
	// source-of-truth contract the snapshot is fail-closed (no active profile).
	if snap.ActiveProfileID != "" {
		t.Errorf("activeProfileId: want empty (ActivateSlot does not set active_profile), got %q", snap.ActiveProfileID)
	}
	// The extension was persisted.
	graph, _ := svc.api.Graph(context.Background())
	if resource(graph, configgraph.ResourceExtension, ExtensionResourceID) == nil {
		t.Error("want routing_profiles extension persisted")
	}
	if got := strField(graph, configgraph.ResourceModel, deepseek.ModelFlash, "default_reasoning_level"); got != deepseek.ReasoningMax {
		t.Errorf("flash reasoning: want max, got %q", got)
	}
	if got := strField(graph, configgraph.ResourceRoute, deepseek.RouteID, "model"); got != deepseek.ModelFlash {
		t.Errorf("route model: want flash, got %q", got)
	}
}

func TestActivateSlotLunaLeavesReasoningUntouched(t *testing.T) {
	svc := NewService(newFakeAPI(typicalGraph()...))
	if _, err := svc.ActivateSlot(context.Background(), deepseek.ProviderID, SlotLuna); err != nil {
		t.Fatalf("activate luna: %v", err)
	}
	graph, _ := svc.api.Graph(context.Background())
	if got := strField(graph, configgraph.ResourceModel, deepseek.ModelFlash, "default_reasoning_level"); got != deepseek.ReasoningHigh {
		t.Errorf("flash reasoning: want untouched high, got %q", got)
	}
}

func TestActivateSlotErrors(t *testing.T) {
	svc := NewService(newFakeAPI(typicalGraph()...))

	if _, err := svc.ActivateSlot(context.Background(), deepseek.ProviderID, "bogus"); err == nil {
		t.Error("unknown slot: want error")
	} else if se, ok := err.(*ServiceError); !ok || se.Kind != KindInvalidInput {
		t.Errorf("unknown slot: want invalid_input, got %T %v", err, err)
	}

	if _, err := svc.ActivateSlot(context.Background(), "bogus", SlotSol); err == nil {
		t.Error("unknown profile: want error")
	} else if se, ok := err.(*ServiceError); !ok || se.Kind != KindInvalidInput {
		t.Errorf("unknown profile: want invalid_input, got %T %v", err, err)
	}

	// Missing model resource → fail-closed.
	noModel := newFakeAPI(providerResource(deepseek.ProviderID, "configured"), routeResource(deepseek.ModelFlash, deepseek.ProviderID))
	svcNoModel := NewService(noModel)
	if _, err := svcNoModel.ActivateSlot(context.Background(), deepseek.ProviderID, SlotSol); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("missing model: want not-configured error, got %v", err)
	}
}

func TestSavePersistsProfileEdit(t *testing.T) {
	svc := NewService(newFakeAPI(typicalGraph()...))
	input := Input{Profile: ProfileInput{ID: deepseek.ProviderID, DisplayName: "DeepSeek", Slots: map[string]SlotInput{
		SlotSol:   {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Reasoning: strPtr("low")},
		SlotTerra: {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Reasoning: strPtr("high")},
		SlotLuna:  {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Reasoning: nil},
	}}}
	snap, err := svc.Save(context.Background(), input)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	for _, p := range snap.Profiles {
		if p.ID != deepseek.ProviderID {
			continue
		}
		if got := slotReasoning(p.Slots, SlotSol); !reasoningEqual(got, strPtr(deepseek.ReasoningLow)) {
			t.Errorf("sol reasoning: want low, got %v", strVal(got))
		}
	}
}

func TestSavePreservesExistingActiveProfile(t *testing.T) {
	deepseekSlots := func(model, solReasoning string) map[string]*slotFile {
		return map[string]*slotFile{
			SlotSol:   {Provider: deepseek.ProviderID, UpstreamModel: model, Reasoning: strPtr(solReasoning)},
			SlotTerra: {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningHigh)},
			SlotLuna:  {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: nil},
		}
	}
	seed := func(profiles map[string]*profileFile, active string) *fakeAPI {
		table := tableFile{ActiveProfile: active, Profiles: profiles}
		graph := buildGraphFromTable(t, table, active)
		return newFakeAPI(graph.Resources...)
	}

	t.Run("active deepseek survives a save", func(t *testing.T) {
		api := seed(map[string]*profileFile{
			deepseek.ProviderID: {DisplayName: "DeepSeek", Slots: deepseekSlots(deepseek.ModelFlash, deepseek.ReasoningMax)},
		}, deepseek.ProviderID)
		svc := NewService(api)
		snap, err := svc.Save(context.Background(), editDeepseekInput())
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if snap.ActiveProfileID != deepseek.ProviderID {
			t.Errorf("activeProfileId: want %q preserved, got %q", deepseek.ProviderID, snap.ActiveProfileID)
		}
		graph, _ := api.Graph(context.Background())
		if got := activeProfileField(t, graph); got != deepseek.ProviderID {
			t.Errorf("persisted active_profile: want %q, got %q", deepseek.ProviderID, got)
		}
		// Snapshot and resolver read the same active profile after Save.
		if !resolverResolves(graph) {
			t.Error("resolver should resolve via the preserved active profile")
		}
	})

	t.Run("non-active save keeps openrouter active", func(t *testing.T) {
		api := seed(map[string]*profileFile{
			"openrouter":        {DisplayName: "OpenRouter", Slots: deepseekSlots(deepseek.ModelFlash, deepseek.ReasoningHigh)},
			deepseek.ProviderID: {DisplayName: "DeepSeek", Slots: deepseekSlots(deepseek.ModelFlash, deepseek.ReasoningMax)},
		}, "openrouter")
		svc := NewService(api)
		snap, err := svc.Save(context.Background(), editDeepseekInput())
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if snap.ActiveProfileID != "openrouter" {
			t.Errorf("activeProfileId: want openrouter, got %q", snap.ActiveProfileID)
		}
		graph, _ := api.Graph(context.Background())
		if got := activeProfileField(t, graph); got != "openrouter" {
			t.Errorf("persisted active_profile: want openrouter, got %q", got)
		}
	})

	t.Run("unknown active_profile is preserved verbatim and fail-closed", func(t *testing.T) {
		api := seed(map[string]*profileFile{
			deepseek.ProviderID: {DisplayName: "DeepSeek", Slots: deepseekSlots(deepseek.ModelFlash, deepseek.ReasoningMax)},
		}, "unknown-profile")
		svc := NewService(api)
		snap, err := svc.Save(context.Background(), editDeepseekInput())
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if snap.ActiveProfileID != "" {
			t.Errorf("activeProfileId: want empty (unknown active_profile fail-closed), got %q", snap.ActiveProfileID)
		}
		graph, _ := api.Graph(context.Background())
		if got := activeProfileField(t, graph); got != "unknown-profile" {
			t.Errorf("persisted active_profile: want %q preserved verbatim, got %q", "unknown-profile", got)
		}
		if resolverResolves(graph) {
			t.Error("resolver must not resolve when active_profile is unknown")
		}
	})

	t.Run("ActivateProfile then Save keeps the activated profile", func(t *testing.T) {
		api := seed(map[string]*profileFile{
			"custom":            {DisplayName: "Custom", Slots: deepseekSlots(deepseek.ModelFlash, deepseek.ReasoningHigh)},
			deepseek.ProviderID: {DisplayName: "DeepSeek", Slots: deepseekSlots(deepseek.ModelFlash, deepseek.ReasoningMax)},
		}, "")
		svc := NewService(api)
		activated, err := svc.ActivateProfile(context.Background(), "custom")
		if err != nil {
			t.Fatalf("activate: %v", err)
		}
		if activated.ActiveProfileID != "custom" {
			t.Fatalf("after activate: activeProfileId = %q, want custom", activated.ActiveProfileID)
		}
		snap, err := svc.Save(context.Background(), editDeepseekInput())
		if err != nil {
			t.Fatalf("save after activate: %v", err)
		}
		if snap.ActiveProfileID != "custom" {
			t.Errorf("activeProfileId after save: want custom, got %q", snap.ActiveProfileID)
		}
		graph, _ := api.Graph(context.Background())
		if got := activeProfileField(t, graph); got != "custom" {
			t.Errorf("persisted active_profile: want custom, got %q", got)
		}
	})
}

func TestSaveDoesNotCreateActiveProfileWhenAbsent(t *testing.T) {
	graph := configgraph.Graph{Resources: []configgraph.Resource{{
		Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Value: map[string]any{
			"enabled": true,
			"config": map[string]any{"profiles": map[string]any{
				"deepseek": map[string]any{"display_name": "DeepSeek", "slots": map[string]any{}},
			}},
		},
	}}}
	svc := NewService(newFakeAPI(graph.Resources...))
	snap, err := svc.Save(context.Background(), editDeepseekInput())
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if snap.ActiveProfileID != "" {
		t.Errorf("activeProfileId: want empty (Save must not invent an active profile), got %q", snap.ActiveProfileID)
	}
	final, _ := svc.api.Graph(context.Background())
	res := resource(final, configgraph.ResourceExtension, ExtensionResourceID)
	cfg, _ := res.Value["config"].(map[string]any)
	if _, ok := cfg["active_profile"]; ok {
		t.Error("Save created an active_profile that was not present")
	}
}

func TestSnapshotIsSecretFree(t *testing.T) {
	graph := configgraph.Graph{Resources: typicalGraph()}
	snap := SnapshotFromGraph(graph, true)
	raw, _ := json.Marshal(snap)
	text := string(raw)
	for _, needle := range []string{"sk-", "Authorization", "apiKey", "controlToken", "serverToken"} {
		if strings.Contains(text, needle) {
			t.Errorf("snapshot leaks secret-shaped field %q", needle)
		}
	}
}

func TestLoad(t *testing.T) {
	svc := NewService(newFakeAPI(typicalGraph()...))
	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !snap.GatewayRunning || len(snap.Profiles) != 1 {
		t.Fatalf("want running snapshot with 1 profile, got %+v", snap)
	}
}

func TestBaselineDefaultPresentInSnapshot(t *testing.T) {
	graph := configgraph.Graph{Resources: typicalGraph()}
	snap := SnapshotFromGraph(graph, true)
	if snap.Baseline == nil {
		t.Fatal("want a baseline slot in the snapshot")
	}
	if snap.Baseline.ID != SlotBaseline || snap.Baseline.DisplayName != "Baseline" {
		t.Errorf("baseline identity = %q/%q, want baseline/Baseline", snap.Baseline.ID, snap.Baseline.DisplayName)
	}
	if snap.Baseline.ProviderID != deepseek.ProviderID || snap.Baseline.ProviderLabel != "DeepSeek" || snap.Baseline.UpstreamModel != deepseek.ModelFlash || snap.Baseline.Mode != ModeNormal {
		t.Errorf("baseline = %+v, want deepseek/DeepSeek/deepseek-v4-flash/normal", snap.Baseline)
	}
	if snap.Baseline.Reasoning != nil {
		t.Errorf("baseline must not carry reasoning, got %v", *snap.Baseline.Reasoning)
	}
}

func TestBaselineSurvivesConfigRoundTrip(t *testing.T) {
	table := defaultTable()
	if table.Baseline == nil {
		t.Fatal("default table must carry a baseline")
	}
	graph := configgraph.Graph{Resources: []configgraph.Resource{{
		Kind:  configgraph.ResourceExtension,
		ID:    ExtensionResourceID,
		Value: extensionValue(table),
	}}}
	restored := tableFromGraph(graph)
	if restored.Baseline == nil || restored.Baseline.Provider != deepseek.ProviderID || restored.Baseline.UpstreamModel != deepseek.ModelFlash || restored.Baseline.Mode != ModeNormal {
		t.Fatalf("baseline not restored through config round-trip: %+v", restored.Baseline)
	}
}

// --- helpers ---

func strField(graph configgraph.Graph, kind configgraph.ResourceKind, id, field string) string {
	r := resource(graph, kind, id)
	if r == nil {
		return ""
	}
	return valueString(r.Value, field)
}

func strPtr(s string) *string { return &s }

func strVal(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func reasoningEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// editDeepseekInput returns a valid Save input that edits the deepseek profile
// (Sol → Pro + high reasoning), leaving any active_profile untouched.
func editDeepseekInput() Input {
	return Input{Profile: ProfileInput{ID: deepseek.ProviderID, DisplayName: "DeepSeek", Slots: map[string]SlotInput{
		SlotSol:   {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelPro, Reasoning: strPtr(deepseek.ReasoningHigh)},
		SlotTerra: {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningHigh)},
		SlotLuna:  {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: nil},
	}}}
}

func activeProfileField(t *testing.T, graph configgraph.Graph) string {
	t.Helper()
	res := resource(graph, configgraph.ResourceExtension, ExtensionResourceID)
	if res == nil {
		return ""
	}
	cfg, _ := res.Value["config"].(map[string]any)
	id, _ := cfg["active_profile"].(string)
	return id
}

func resolverResolves(graph configgraph.Graph) bool {
	_, ok := NewSlotResolver(graph).ResolveSlot("gpt-5.6-sol")
	return ok
}
