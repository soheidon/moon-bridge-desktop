package routingprofile

import (
	"moonbridge/internal/service/configgraph"
)

// SlotResult is the output of a slot resolution. Reasoning is nil when the
// slot carries no reasoning override (Luna).
type SlotResult struct {
	SlotID          string
	ActiveProfileID string
	ProviderKey     string
	UpstreamModel   string
	Mode            string
	Reasoning       *string
}

// modelToSlot is the fixed mapping from Codex request model identifiers to
// routing slot IDs. Only exact literal matches are accepted.
var modelToSlot = map[string]string{
	"gpt-5.6-sol":   SlotSol,
	"gpt-5.6-terra": SlotTerra,
	"gpt-5.6-luna":  SlotLuna,
}

// SlotResolver resolves Codex request model identifiers to slot configurations
// from the active routing profile. It is read-only: it never patches the graph.
type SlotResolver struct {
	table               tableFile
	activeProfileID     string
	hasProfileExtension bool
}

// NewSlotResolver builds a SlotResolver from a config graph snapshot.
func NewSlotResolver(graph configgraph.Graph) *SlotResolver {
	table := tableFromGraph(graph)
	hasExtension := resource(graph, configgraph.ResourceExtension, ExtensionResourceID) != nil
	return &SlotResolver{
		table:               table,
		activeProfileID:     activeProfileIDFromGraph(graph, table),
		hasProfileExtension: hasExtension,
	}
}

// activeProfileIDFromGraph resolves the active profile id under the confirmed
// contract: when the routing_profiles extension exists, config.active_profile is
// the sole source of truth (missing/empty/unknown → ""); route/provider
// derivation is allowed only for bootstrap when the extension is absent.
func activeProfileIDFromGraph(graph configgraph.Graph, table tableFile) string {
	res := resource(graph, configgraph.ResourceExtension, ExtensionResourceID)
	if res != nil {
		cfg, ok := res.Value["config"].(map[string]any)
		if !ok {
			return ""
		}
		id, ok := cfg["active_profile"].(string)
		if !ok || id == "" {
			return ""
		}
		if _, ok := table.Profiles[id]; ok {
			return id
		}
		return ""
	}
	id := activeRouteProvider(graph)
	if id != "" {
		if _, ok := table.Profiles[id]; ok {
			return id
		}
	}
	return ""
}

// NewSlotResolverFromDefaults builds a SlotResolver from the default profile
// table and an explicit active profile ID. Use this when no config graph is
// available (e.g. during Gateway startup before the management API is ready).
// BootstrapEligible() returns true for resolvers built this way.
func NewSlotResolverFromDefaults(activeProfileID string) *SlotResolver {
	return &SlotResolver{
		table:               defaultTable(),
		activeProfileID:     activeProfileID,
		hasProfileExtension: false,
	}
}

// BootstrapEligible reports whether the resolver was built without a
// routing_profiles extension resource. When true, the caller may fall back
// to route/provider derivation for legacy bootstrap. When false, the
// extension is present (even if active_profile is invalid) and no
// bootstrap fallback should occur.
func (r *SlotResolver) BootstrapEligible() bool {
	return !r.hasProfileExtension
}

// ResolveSlot returns the slot configuration for a Codex request model.
// Returns ok=false when no active profile or no exact match.
func (r *SlotResolver) ResolveSlot(requestModel string) (SlotResult, bool) {
	slotID, ok := modelToSlot[requestModel]
	if !ok {
		return SlotResult{}, false
	}
	if r.activeProfileID == "" {
		return SlotResult{}, false
	}
	profile, ok := r.table.Profiles[r.activeProfileID]
	if !ok || profile == nil {
		return SlotResult{}, false
	}
	slot, ok := profile.Slots[slotID]
	if !ok || slot == nil {
		return SlotResult{}, false
	}
	mode, err := normalizeSlotMode(slot.Mode, slot.Reasoning)
	if err != nil {
		return SlotResult{}, false
	}
	var reasoning *string
	if mode == ModeThinking {
		reasoning = cloneReasoning(slot.Reasoning)
	}
	return SlotResult{
		SlotID:          slotID,
		ActiveProfileID: r.activeProfileID,
		ProviderKey:     slot.Provider,
		UpstreamModel:   slot.UpstreamModel,
		Mode:            mode,
		Reasoning:       reasoning,
	}, true
}
