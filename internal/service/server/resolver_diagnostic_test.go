package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/routingprofile"
	"moonbridge/internal/service/server"
)

type diagnosticResolver struct{}

func (diagnosticResolver) ResolveSlot(string) (server.RoutingProfileSlotResult, bool) {
	return server.RoutingProfileSlotResult{}, false
}

func (diagnosticResolver) SafeState() routingprofile.SafeResolverState {
	return routingprofile.SafeResolverState{
		ExtensionState:     "valid",
		ActiveProfileState: "present_valid",
		SlotCount:          3,
		SolState:           "ready",
		TerraState:         "ready",
		LunaState:          "ready",
	}
}

func TestRoutingResolverStatusIsSecretSafeAndTracksServerGeneration(t *testing.T) {
	s := server.New(server.Config{
		RoutingProfileResolver: diagnosticResolver{},
		RoutingConfigSource:    "persisted_store",
	})
	first := s.RoutingResolverStatus()
	if !strings.HasPrefix(first.ServerInstance, "server#") || first.Generation != 1 || first.InstallSource != "startup" || !first.ResolverPresent {
		t.Fatalf("startup status = %#v", first)
	}
	if first.RoutingProfileState.SlotCount != 3 {
		t.Fatalf("startup safe state = %#v", first.RoutingProfileState)
	}
	b, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	serialized := string(b)
	for _, forbidden := range []string{"profile-secret-sentinel", "provider-secret-sentinel", "model-secret-sentinel", "https://secret.invalid", "C:\\secret"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("diagnostic JSON contains forbidden sentinel %q: %s", forbidden, serialized)
		}
	}

	s.SwapRoutingProfileResolver(diagnosticResolver{})
	second := s.RoutingResolverStatus()
	if second.ServerInstance != first.ServerInstance || second.Generation != 2 || second.InstallSource != "profile_refresh" {
		t.Fatalf("refresh status = %#v, first = %#v", second, first)
	}
}

type runtimeSnapshotResolver struct {
	state routingprofile.SafeResolverState
	slots map[string]server.RoutingProfileSlotResult
}

func (r runtimeSnapshotResolver) ResolveSlot(model string) (server.RoutingProfileSlotResult, bool) {
	slot, ok := r.slots[model]
	return slot, ok
}

func (r runtimeSnapshotResolver) SafeState() routingprofile.SafeResolverState { return r.state }

func TestRuntimeConfigurationStatusIsBoundedAndChecksProviderReferences(t *testing.T) {
	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
		"deepseek": {BaseURL: "https://secret.invalid", ModelNames: []string{"deepseek-v4-flash"}},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	resolver := runtimeSnapshotResolver{
		state: routingprofile.SafeResolverState{
			ExtensionState: "valid", ActiveProfileState: "present_valid", SlotCount: 3,
			SolState: "ready", TerraState: "ready", LunaState: "ready",
		},
		slots: map[string]server.RoutingProfileSlotResult{
			"gpt-5.6-sol":   {SlotID: "sol", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: routingprofile.ModeThinking, Reasoning: strPtr("max")},
			"gpt-5.6-terra": {SlotID: "terra", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: routingprofile.ModeThinking, Reasoning: strPtr("high")},
			"gpt-5.6-luna":  {SlotID: "luna", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: routingprofile.ModeNormal},
		},
	}
	s := server.New(server.Config{ProviderMgr: pm, RoutingProfileResolver: resolver, RoutingProfileState: resolver.state, RoutingConfigSource: "persisted_store"})
	status := s.RuntimeConfigurationStatus()
	if status.State != "ready" || status.ReadySlotCount != 3 || status.Slots.Sol.State != "ready" || status.Slots.Luna.Mode != "normal" {
		t.Fatalf("runtime status = %#v", status)
	}

	broken := resolver
	broken.slots["gpt-5.6-terra"] = server.RoutingProfileSlotResult{ProviderKey: "missing", UpstreamModel: "secret-model"}
	s.SwapRoutingProfileResolver(broken)
	status = s.RuntimeConfigurationStatus()
	if status.State != "degraded" || status.ReadySlotCount != 2 || status.Slots.Terra.State != "reference_unresolved" {
		t.Fatalf("unresolved runtime status = %#v", status)
	}

	b, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	serialized := string(b)
	for _, forbidden := range []string{"secret.invalid", "secret-model", "deepseek/deepseek-v4-flash", "apiKey", "Authorization"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("runtime JSON contains forbidden sentinel %q: %s", forbidden, serialized)
		}
	}
}

func strPtr(value string) *string { return &value }
