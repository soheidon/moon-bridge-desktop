// Package routingprofile manages the Codex routing profile table in the Moon
// Bridge gateway config. A routing profile groups the three Codex routing slots
// (Sol/Terra/Luna) into one upstream assignment: each slot maps to a provider,
// an upstream model, and an optional reasoning override. The table lives in the
// config graph's routing_profiles extension resource; when the resource is
// absent the confirmed default profile (Sol→flash/max, Terra→flash/high,
// Luna→flash/no override) is served in memory without writing to the graph.
//
// The active profile is stored as routing_profiles.config.active_profile; when
// the extension exists it is the sole source of truth for the active profile id.
// Route/provider derivation is a bootstrap compatibility fallback used only when
// the extension is absent. Slots carry no persistent active state (the Codex
// client's current selection is not routing state). Activating a slot projects
// its reasoning onto the upstream model resource's default_reasoning_level
// (Luna's nil override leaves reasoning untouched) and its upstream model onto
// the route.
package routingprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/deepseek"
)

const (
	// ExtensionResourceID is the graph extension resource that persists the
	// profile table. It is registered in the builtin extension catalog.
	ExtensionResourceID = "routing_profiles"

	SlotSol   = "sol"
	SlotTerra = "terra"
	SlotLuna  = "luna"

	providerLabel = "DeepSeek"

	maxReconcileAttempts = 3
)

var allSlots = []string{SlotSol, SlotTerra, SlotLuna}

var slotDisplayNames = map[string]string{
	SlotSol:   "Sol",
	SlotTerra: "Terra",
	SlotLuna:  "Luna",
}

// --- persisted table shape (config.profiles) ---

const (
	ModeNormal   = "normal"
	ModeThinking = "thinking"
)

type slotFile struct {
	Provider      string  `json:"provider"`
	UpstreamModel string  `json:"upstream_model"`
	Mode          string  `json:"mode,omitempty"`
	Reasoning     *string `json:"reasoning"`
}

type profileFile struct {
	DisplayName string               `json:"display_name"`
	Slots       map[string]*slotFile `json:"slots"`
}

type tableFile struct {
	ActiveProfile string                  `json:"active_profile,omitempty"`
	Profiles      map[string]*profileFile `json:"profiles"`
}

// --- snapshot (secret-free DTO) ---

// Slot is one Codex routing slot of a profile. Reasoning is nil when the slot
// carries no reasoning override (Luna).
type Slot struct {
	ID            string  `json:"id"`
	DisplayName   string  `json:"displayName"`
	ProviderID    string  `json:"providerId"`
	ProviderLabel string  `json:"providerLabel"`
	UpstreamModel string  `json:"upstreamModel"`
	Mode          string  `json:"mode"`
	Reasoning     *string `json:"reasoning"`
}

// Profile is one routing profile card. Active reflects the graph's active
// profile: routing_profiles.config.active_profile when the extension exists,
// the moonbridge route provider only as a bootstrap fallback when it does not.
type Profile struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Active      bool   `json:"active"`
	Configured  bool   `json:"configured"`
	Slots       []Slot `json:"slots"`
}

// Snapshot is the JSON shape surfaced by the routing profile bindings.
type Snapshot struct {
	GatewayRunning  bool      `json:"gatewayRunning"`
	ActiveProfileID string    `json:"activeProfileId"`
	Profiles        []Profile `json:"profiles"`
}

// SnapshotFromGraph derives a snapshot from a config graph. The active profile
// follows the same source-of-truth contract as SlotResolver: when the
// routing_profiles extension exists, config.active_profile is authoritative
// (missing/empty/unknown → no active profile); route/provider derivation is used
// only for bootstrap when the extension is absent. gatewayRunning is set by the
// caller (true when invoked through a live gateway session).
func SnapshotFromGraph(graph configgraph.Graph, gatewayRunning bool) Snapshot {
	table := tableFromGraph(graph)
	activeProvider := activeProfileIDFromGraph(graph, table)
	profiles := profilesFromTable(graph, table, activeProvider)
	activeProfileID := ""
	for _, p := range profiles {
		if p.Active {
			activeProfileID = p.ID
			break
		}
	}
	return Snapshot{
		GatewayRunning:  gatewayRunning,
		ActiveProfileID: activeProfileID,
		Profiles:        profiles,
	}
}

// --- input ---

// SlotInput is one slot's edit payload for a profile save.
type SlotInput struct {
	Provider      string  `json:"provider"`
	UpstreamModel string  `json:"upstreamModel"`
	Mode          string  `json:"mode"`
	Reasoning     *string `json:"reasoning"`
}

// ProfileInput is the edit payload for a single profile.
type ProfileInput struct {
	ID          string               `json:"id"`
	DisplayName string               `json:"displayName"`
	Slots       map[string]SlotInput `json:"slots"`
}

// Input is the save payload. It edits one profile in the table, leaving other
// profiles untouched.
type Input struct {
	Profile ProfileInput `json:"profile"`
}

// Validate checks the input shape. Reasoning is normalized before the
// allowed-membership check so xhigh is accepted as max.
func (in Input) Validate() error {
	p := in.Profile
	if strings.TrimSpace(p.ID) == "" {
		return invalidInput("profile.id", "profile id must not be empty")
	}
	for _, slotID := range allSlots {
		slot, ok := p.Slots[slotID]
		if !ok {
			return invalidInput("profile.slots."+slotID, "slot "+slotID+" is required")
		}
		if strings.TrimSpace(slot.Provider) == "" {
			return invalidInput("profile.slots."+slotID+".provider", "slot provider must not be empty")
		}
		if strings.TrimSpace(slot.UpstreamModel) == "" {
			return invalidInput("profile.slots."+slotID+".upstreamModel", "slot upstreamModel must not be empty")
		}
		mode, err := normalizeSlotMode(slot.Mode, slot.Reasoning)
		if err != nil {
			return invalidInput("profile.slots."+slotID+".mode", err.Error())
		}
		if mode == ModeThinking && slot.Reasoning != nil && !reasoningAllowed(slot.UpstreamModel, *slot.Reasoning) {
			return invalidInput("profile.slots."+slotID+".reasoning", "slot reasoning must be one of low, high, max")
		}
	}
	return nil
}

// --- service errors ---

type ServiceErrorKind string

const (
	KindInvalidInput             ServiceErrorKind = "invalid_input"
	KindGatewayAPIFailed         ServiceErrorKind = "gateway_api_failed"
	KindSaveRejected             ServiceErrorKind = "save_rejected"
	KindRevisionConflictExceeded ServiceErrorKind = "revision_conflict_exceeded"
	KindVerifyFailed             ServiceErrorKind = "verify_failed"
)

// ServiceError is a structured, non-secret error. Message and Details never
// contain API keys, control tokens, or raw resource values.
type ServiceError struct {
	Kind            ServiceErrorKind
	Message         string
	Field           *string
	Details         map[string]any
	MutationStarted bool
	Retryable       bool
}

func (e *ServiceError) Error() string {
	if e.Message == "" {
		return string(e.Kind)
	}
	return e.Message
}

func invalidInput(field, message string) error {
	f := field
	return &ServiceError{Kind: KindInvalidInput, Message: message, Field: &f}
}

// ManagementAPI is the subset of the gateway config management API the routing
// profile service drives.
type ManagementAPI interface {
	Graph(ctx context.Context) (configgraph.Graph, error)
	Patch(ctx context.Context, req configgraph.PatchRequest) (configgraph.PatchResponse, error)
	CreateResource(ctx context.Context, baseRevision string, kind configgraph.ResourceKind, id string, value map[string]any) (configgraph.PatchResponse, error)
}

// Service manages routing profiles through a ManagementAPI client.
type Service struct {
	api ManagementAPI
}

func NewService(api ManagementAPI) *Service {
	return &Service{api: api}
}

// Load returns the current routing profile snapshot without mutating state.
func (s *Service) Load(ctx context.Context) (*Snapshot, error) {
	graph, err := s.api.Graph(ctx)
	if err != nil {
		return nil, &ServiceError{
			Kind:      KindGatewayAPIFailed,
			Message:   "unable to load routing profiles",
			Retryable: true,
		}
	}
	snap := SnapshotFromGraph(graph, true)
	return &snap, nil
}

// Validate checks a save input without requiring a gateway session.
func (s *Service) Validate(input Input) error {
	return input.Validate()
}

// Save persists one profile edit to the routing_profiles extension resource.
// Other profiles are preserved. Revision conflicts are retried against a fresh
// graph up to maxReconcileAttempts; after exhaustion a residual (non-secret)
// state is reported in Details.
func (s *Service) Save(ctx context.Context, input Input) (*Snapshot, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		graph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, gatewayFailed(err, "unable to load current configuration", false)
		}
		table := tableFromGraph(graph)
		if table.Profiles == nil {
			table.Profiles = map[string]*profileFile{}
		}
		table.Profiles[input.Profile.ID] = inputToProfileFile(input.Profile)
		_, outcome := s.reconcileSave(ctx, graph, table)
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.conflict {
			continue
		}
		finalGraph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, &ServiceError{
				Kind:            KindGatewayAPIFailed,
				Message:         "profile saved but could not be re-read for verification",
				MutationStarted: true,
				Retryable:       true,
			}
		}
		if detail, ok := verifySavedTable(finalGraph, table); !ok {
			return nil, &ServiceError{
				Kind:            KindVerifyFailed,
				Message:         "saved routing profile does not match the requested state",
				MutationStarted: true,
				Details:         map[string]any{"final_state_mismatch": detail},
			}
		}
		snap := SnapshotFromGraph(finalGraph, true)
		return &snap, nil
	}
	finalGraph, err := s.api.Graph(ctx)
	if err != nil {
		return nil, &ServiceError{
			Kind:            KindGatewayAPIFailed,
			Message:         "configuration changed repeatedly",
			MutationStarted: true,
			Retryable:       true,
		}
	}
	return nil, &ServiceError{
		Kind:            KindRevisionConflictExceeded,
		Message:         "configuration changed repeatedly; retry saving the routing profile",
		MutationStarted: true,
		Retryable:       true,
		Details:         residualState(finalGraph),
	}
}

// ActivateSlot activates a slot of a profile: it persists the profile table
// (creating the extension when absent), points the moonbridge route at the
// slot's provider and upstream model, and projects the slot's reasoning onto
// the upstream model resource's default_reasoning_level. A nil reasoning
// (Luna) leaves the model's reasoning untouched.
//
// Deprecated: Use ActivateProfile instead. Slot-level activation patches routes
// and reasoning on every call, which is no longer required. ActivateProfile
// changes only active_profile and lets the resolver handle slot lookup at
// request time. This method is retained for backward compatibility and will be
// removed after frontend migration completes.
func (s *Service) ActivateSlot(ctx context.Context, profileID, slotID string) (*Snapshot, error) {
	profileID = strings.TrimSpace(profileID)
	slotID = strings.TrimSpace(slotID)
	if profileID == "" {
		return nil, invalidInput("profile", "profile id must not be empty")
	}
	if !contains(allSlots, slotID) {
		return nil, invalidInput("slot", "slot must be one of sol, terra, luna")
	}
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		graph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, gatewayFailed(err, "unable to load current configuration", false)
		}
		_, outcome := s.reconcileActivate(ctx, graph, profileID, slotID)
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.conflict {
			continue
		}
		finalGraph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, &ServiceError{
				Kind:            KindGatewayAPIFailed,
				Message:         "profile activated but could not be re-read for verification",
				MutationStarted: true,
				Retryable:       true,
			}
		}
		if detail, ok := verifyActivation(finalGraph, profileID, slotID); !ok {
			return nil, &ServiceError{
				Kind:            KindVerifyFailed,
				Message:         "activated profile does not match the requested slot",
				MutationStarted: true,
				Details:         map[string]any{"final_state_mismatch": detail},
			}
		}
		snap := SnapshotFromGraph(finalGraph, true)
		return &snap, nil
	}
	finalGraph, err := s.api.Graph(ctx)
	if err != nil {
		return nil, &ServiceError{
			Kind:            KindGatewayAPIFailed,
			Message:         "configuration changed repeatedly",
			MutationStarted: true,
			Retryable:       true,
		}
	}
	return nil, &ServiceError{
		Kind:            KindRevisionConflictExceeded,
		Message:         "configuration changed repeatedly; retry activating the routing profile",
		MutationStarted: true,
		Retryable:       true,
		Details:         residualState(finalGraph),
	}
}

// ActivateProfile changes only routing_profiles.config.active_profile.
// It does not modify slot definitions, route resources, or model reasoning.
// This is the canonical activation primitive; slot-level activation (which
// also patched routes and reasoning) is preserved for backward compatibility
// but new callers should use ActivateProfile.
func (s *Service) ActivateProfile(ctx context.Context, profileID string) (*Snapshot, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return nil, invalidInput("profile", "profile id must not be empty")
	}
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		graph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, gatewayFailed(err, "unable to load current configuration", false)
		}
		table := tableFromGraph(graph)
		if _, ok := table.Profiles[profileID]; !ok {
			return nil, &ServiceError{
				Kind:    KindInvalidInput,
				Message: fmt.Sprintf("unknown routing profile %q", profileID),
			}
		}
		_, outcome := s.reconcileActivateProfile(ctx, graph, profileID)
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.conflict {
			continue
		}
		finalGraph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, &ServiceError{
				Kind:            KindGatewayAPIFailed,
				Message:         "profile activated but could not be re-read for verification",
				MutationStarted: true,
				Retryable:       true,
			}
		}
		if detail, ok := verifyActiveProfile(finalGraph, profileID); !ok {
			return nil, &ServiceError{
				Kind:            KindVerifyFailed,
				Message:         "activated profile does not match the requested profile",
				MutationStarted: true,
				Details:         map[string]any{"final_state_mismatch": detail},
			}
		}
		snap := SnapshotFromGraph(finalGraph, true)
		return &snap, nil
	}
	finalGraph, err := s.api.Graph(ctx)
	if err != nil {
		return nil, &ServiceError{
			Kind:            KindGatewayAPIFailed,
			Message:         "configuration changed repeatedly",
			MutationStarted: true,
			Retryable:       true,
		}
	}
	return nil, &ServiceError{
		Kind:            KindRevisionConflictExceeded,
		Message:         "configuration changed repeatedly; retry activating the routing profile",
		MutationStarted: true,
		Retryable:       true,
		Details:         residualState(finalGraph),
	}
}

type reconcileOutcome struct {
	conflict bool
	mutated  bool
	err      error
}

func (s *Service) reconcileActivate(ctx context.Context, graph configgraph.Graph, profileID, slotID string) (configgraph.Graph, reconcileOutcome) {
	out := reconcileOutcome{}

	table := tableFromGraph(graph)
	profile, ok := table.Profiles[profileID]
	if !ok {
		return graph, reconcileOutcome{err: &ServiceError{
			Kind:    KindInvalidInput,
			Message: fmt.Sprintf("unknown routing profile %q", profileID),
		}}
	}
	slot, ok := profile.Slots[slotID]
	if !ok || slot == nil {
		return graph, reconcileOutcome{err: &ServiceError{
			Kind:    KindInvalidInput,
			Message: fmt.Sprintf("profile %q has no slot %q", profileID, slotID),
		}}
	}
	if strings.TrimSpace(slot.Provider) == "" || strings.TrimSpace(slot.UpstreamModel) == "" {
		return graph, reconcileOutcome{err: &ServiceError{
			Kind:    KindInvalidInput,
			Message: fmt.Sprintf("slot %s of profile %q is not configured", slotID, profileID),
		}}
	}

	// 1. Persist the profile table on first activation so future edits have a
	//    stable base.
	if resource(graph, configgraph.ResourceExtension, ExtensionResourceID) == nil {
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.CreateResource(ctx, base, configgraph.ResourceExtension, ExtensionResourceID, extensionValue(table))
		})
		if err != nil {
			return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
		}
		if conflict {
			return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
		}
		graph = g
		out.mutated = true
	}

	// 2. Route: create if missing, else patch model/provider differences.
	if resource(graph, configgraph.ResourceRoute, deepseek.RouteID) == nil {
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.CreateResource(ctx, base, configgraph.ResourceRoute, deepseek.RouteID, routeValue(slot.Provider, slot.UpstreamModel))
		})
		if err != nil {
			return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
		}
		if conflict {
			return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
		}
		graph = g
		out.mutated = true
	} else {
		current := resource(graph, configgraph.ResourceRoute, deepseek.RouteID)
		var changes []configgraph.PatchOp
		if valueString(current.Value, "model") != slot.UpstreamModel {
			changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceRoute, ID: deepseek.RouteID, Field: "model", Value: slot.UpstreamModel})
		}
		if valueString(current.Value, "provider") != slot.Provider {
			changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceRoute, ID: deepseek.RouteID, Field: "provider", Value: slot.Provider})
		}
		if len(changes) > 0 {
			g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
				return s.api.Patch(ctx, configgraph.PatchRequest{BaseRevision: base, Changes: changes})
			})
			if err != nil {
				return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
			}
			if conflict {
				return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
			}
			graph = g
			out.mutated = true
		}
	}

	// 3. Model reasoning: only when the slot carries a reasoning override.
	//    Luna's nil override leaves the model's reasoning untouched.
	if slot.Reasoning != nil {
		model := resource(graph, configgraph.ResourceModel, slot.UpstreamModel)
		if model == nil {
			return graph, reconcileOutcome{err: &ServiceError{
				Kind:    KindInvalidInput,
				Message: fmt.Sprintf("model %q is not configured; run DeepSeek setup first", slot.UpstreamModel),
			}}
		}
		if valueString(model.Value, "default_reasoning_level") != *slot.Reasoning {
			g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
				return s.api.Patch(ctx, configgraph.PatchRequest{BaseRevision: base, Changes: []configgraph.PatchOp{{
					Kind: configgraph.ResourceModel, ID: slot.UpstreamModel, Field: "default_reasoning_level", Value: *slot.Reasoning,
				}}})
			})
			if err != nil {
				return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
			}
			if conflict {
				return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
			}
			graph = g
			out.mutated = true
		}
	}

	return graph, out
}

// reconcileActivateProfile changes only routing_profiles.config.active_profile.
// It does not touch slot definitions, route resources, or model reasoning.
func (s *Service) reconcileActivateProfile(ctx context.Context, graph configgraph.Graph, profileID string) (configgraph.Graph, reconcileOutcome) {
	out := reconcileOutcome{}

	// Ensure the extension resource exists.
	if resource(graph, configgraph.ResourceExtension, ExtensionResourceID) == nil {
		table := tableFromGraph(graph)
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.CreateResource(ctx, base, configgraph.ResourceExtension, ExtensionResourceID, extensionValue(table))
		})
		if err != nil {
			return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
		}
		if conflict {
			return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
		}
		graph = g
		out.mutated = true
	}

	// Patch only active_profile.
	g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
		return s.api.Patch(ctx, configgraph.PatchRequest{
			BaseRevision: base,
			Changes: []configgraph.PatchOp{{
				Kind:  configgraph.ResourceExtension,
				ID:    ExtensionResourceID,
				Field: "config.active_profile",
				Value: profileID,
			}},
		})
	})
	if err != nil {
		return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
	}
	if conflict {
		return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
	}
	graph = g
	out.mutated = true

	return graph, out
}

// verifyActiveProfile checks that the graph's active_profile matches profileID.
func verifyActiveProfile(graph configgraph.Graph, profileID string) (string, bool) {
	res := resource(graph, configgraph.ResourceExtension, ExtensionResourceID)
	if res == nil {
		return "extension resource missing", false
	}
	cfg, ok := res.Value["config"].(map[string]any)
	if !ok {
		return "config key missing", false
	}
	got, ok := cfg["active_profile"].(string)
	if !ok || got != profileID {
		return fmt.Sprintf("active_profile=%q, want %q", got, profileID), false
	}
	return "", true
}

func (s *Service) reconcileSave(ctx context.Context, graph configgraph.Graph, table tableFile) (configgraph.Graph, reconcileOutcome) {
	out := reconcileOutcome{}
	configValue := extensionConfigValue(table)
	if resource(graph, configgraph.ResourceExtension, ExtensionResourceID) == nil {
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.CreateResource(ctx, base, configgraph.ResourceExtension, ExtensionResourceID, extensionValue(table))
		})
		if err != nil {
			return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
		}
		if conflict {
			return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
		}
		graph = g
		out.mutated = true
		return graph, out
	}
	current := resource(graph, configgraph.ResourceExtension, ExtensionResourceID)
	var changes []configgraph.PatchOp
	if !valueEqual(current.Value["enabled"], true) {
		changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Field: "enabled", Value: true})
	}
	if !valueEqual(current.Value["config"], configValue) {
		changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceExtension, ID: ExtensionResourceID, Field: "config", Value: configValue})
	}
	if len(changes) > 0 {
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.Patch(ctx, configgraph.PatchRequest{BaseRevision: base, Changes: changes})
		})
		if err != nil {
			return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
		}
		if conflict {
			return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
		}
		graph = g
		out.mutated = true
	}
	return graph, out
}

// apply runs one mutation and returns the updated graph. A revision conflict
// is signaled via the conflict flag; the caller refetches and recomputes. The
// response's graph is preferred so the next mutation carries the new revision.
func (s *Service) apply(ctx context.Context, graph configgraph.Graph, mutate func(context.Context, string) (configgraph.PatchResponse, error)) (configgraph.Graph, bool, error) {
	resp, err := mutate(ctx, graph.Revision)
	if err != nil {
		return graph, false, err
	}
	switch resp.Result {
	case configgraph.ResultRevisionConflict:
		return graph, true, nil
	case configgraph.ResultCommitted, configgraph.ResultRestartRequired:
		if resp.Graph != nil {
			return *resp.Graph, false, nil
		}
		refreshed, err := s.api.Graph(ctx)
		if err != nil {
			return graph, false, err
		}
		return refreshed, false, nil
	default:
		return graph, false, &ServiceError{
			Kind:    KindSaveRejected,
			Message: fmt.Sprintf("configuration change was rejected: %s", resp.Result),
			Details: fieldErrorDetails(resp.Errors),
		}
	}
}

// mutationError converts a raw mutation error into a ServiceError, marking the
// mutation as started when any prior mutation committed.
func (s *Service) mutationError(err error, mutated bool) error {
	var se *ServiceError
	if errors.As(err, &se) {
		if !se.MutationStarted {
			se.MutationStarted = mutated
		}
		return se
	}
	return &ServiceError{
		Kind:            KindGatewayAPIFailed,
		Message:         fmt.Sprintf("management API request failed: %v", err),
		MutationStarted: mutated,
		Retryable:       true,
	}
}

func gatewayFailed(err error, message string, mutated bool) error {
	return &ServiceError{
		Kind:            KindGatewayAPIFailed,
		Message:         message,
		MutationStarted: mutated,
		Retryable:       true,
	}
}

// --- graph reading ---

func NormalizeSlotMode(mode string, reasoning *string) (string, error) {
	return normalizeSlotMode(mode, reasoning)
}

func normalizeSlotMode(mode string, reasoning *string) (string, error) {
	if mode == "" {
		if reasoning == nil {
			return ModeNormal, nil
		}
		return ModeThinking, nil
	}
	if mode != ModeNormal && mode != ModeThinking {
		return "", fmt.Errorf("mode must be normal or thinking")
	}
	// Thinking + nil reasoning（Default）を許可
	// Normal + reasoningも許可（ユーザー選択を上書きしない）
	return mode, nil
}

func tableFromGraph(graph configgraph.Graph) tableFile {
	res := resource(graph, configgraph.ResourceExtension, ExtensionResourceID)
	if res == nil {
		return defaultTable()
	}
	cfg, ok := res.Value["config"].(map[string]any)
	if !ok {
		return defaultTable()
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return defaultTable()
	}
	var table tableFile
	if err := json.Unmarshal(data, &table); err != nil {
		return defaultTable()
	}
	if len(table.Profiles) == 0 {
		return defaultTable()
	}
	for _, profile := range table.Profiles {
		for _, slot := range profile.Slots {
			if slot == nil || slot.Mode != "" {
				continue
			}
			slot.Mode, _ = normalizeSlotMode("", slot.Reasoning)
		}
	}
	return table
}

func defaultTable() tableFile {
	strPtr := func(s string) *string { return &s }
	return tableFile{Profiles: map[string]*profileFile{
		deepseek.ProviderID: {
			DisplayName: providerLabel,
			Slots: map[string]*slotFile{
				SlotSol:   {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningMax)},
				SlotTerra: {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningHigh)},
				SlotLuna:  {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: nil},
			},
		},
	}}
}

func profilesFromTable(graph configgraph.Graph, table tableFile, activeProvider string) []Profile {
	ids := sortedKeys(table.Profiles)
	out := make([]Profile, 0, len(ids))
	for _, id := range ids {
		p := table.Profiles[id]
		if p == nil {
			continue
		}
		slots := make([]Slot, 0, len(allSlots))
		configured := true
		for _, slotID := range allSlots {
			sf := p.Slots[slotID]
			if sf == nil {
				slots = append(slots, Slot{ID: slotID, DisplayName: slotDisplayNames[slotID]})
				configured = false
				continue
			}
			slots = append(slots, Slot{
				ID:            slotID,
				DisplayName:   slotDisplayNames[slotID],
				ProviderID:    sf.Provider,
				ProviderLabel: providerDisplayLabel(sf.Provider),
				UpstreamModel: sf.UpstreamModel,
				Mode:          sf.Mode,
				Reasoning:     cloneReasoning(sf.Reasoning),
			})
			if !providerConfigured(graph, sf.Provider) {
				configured = false
			}
		}
		out = append(out, Profile{
			ID:          id,
			DisplayName: p.DisplayName,
			Active:      id == activeProvider,
			Configured:  configured,
			Slots:       slots,
		})
	}
	return out
}

func activeRouteProvider(graph configgraph.Graph) string {
	res := resource(graph, configgraph.ResourceRoute, deepseek.RouteID)
	if res == nil {
		return ""
	}
	return valueString(res.Value, "provider")
}

func providerConfigured(graph configgraph.Graph, providerID string) bool {
	if providerID == "" {
		return false
	}
	res := resource(graph, configgraph.ResourceProvider, providerID)
	if res == nil {
		return false
	}
	return valueString(res.Value, "api_key") != ""
}

func providerDisplayLabel(providerID string) string {
	if providerID == deepseek.ProviderID {
		return providerLabel
	}
	return providerID
}

func reasoningAllowed(model, effort string) bool {
	effort = deepseek.NormalizeReasoningEffort(effort)
	// すべてのモデルでlow/high/maxを許可（スロット・モデルによる制約なし）
	allowed := []string{deepseek.ReasoningLow, deepseek.ReasoningHigh, deepseek.ReasoningMax}
	return contains(allowed, effort)
}

// --- verification ---

func verifyActivation(graph configgraph.Graph, profileID, slotID string) (string, bool) {
	table := tableFromGraph(graph)
	profile, ok := table.Profiles[profileID]
	if !ok {
		return "profile " + profileID, false
	}
	slot, ok := profile.Slots[slotID]
	if !ok || slot == nil {
		return "profile slot " + slotID, false
	}
	route := resource(graph, configgraph.ResourceRoute, deepseek.RouteID)
	if route == nil {
		return "moonbridge route", false
	}
	if valueString(route.Value, "provider") != slot.Provider {
		return "moonbridge route provider", false
	}
	if valueString(route.Value, "model") != slot.UpstreamModel {
		return "moonbridge route model", false
	}
	if slot.Reasoning != nil {
		model := resource(graph, configgraph.ResourceModel, slot.UpstreamModel)
		if model == nil {
			return "model " + slot.UpstreamModel, false
		}
		if valueString(model.Value, "default_reasoning_level") != *slot.Reasoning {
			return "model " + slot.UpstreamModel + " reasoning", false
		}
	}
	return "", true
}

func verifySavedTable(graph configgraph.Graph, table tableFile) (string, bool) {
	res := resource(graph, configgraph.ResourceExtension, ExtensionResourceID)
	if res == nil {
		return "routing_profiles extension", false
	}
	enabled, _ := res.Value["enabled"].(bool)
	if !enabled {
		return "routing_profiles enabled", false
	}
	cfg, ok := res.Value["config"].(map[string]any)
	if !ok {
		return "routing_profiles config", false
	}
	if !valueEqual(cfg, extensionConfigValue(table)) {
		return "routing_profiles config value", false
	}
	return "", true
}

// residualState reports which routing-profile resources are present after retry
// exhaustion. It never contains secret material.
func residualState(graph configgraph.Graph) map[string]any {
	state := func(kind configgraph.ResourceKind, id string) string {
		if resource(graph, kind, id) != nil {
			return "present"
		}
		return "missing"
	}
	return map[string]any{
		"extension":   state(configgraph.ResourceExtension, ExtensionResourceID),
		"route":       state(configgraph.ResourceRoute, deepseek.RouteID),
		"model_flash": state(configgraph.ResourceModel, deepseek.ModelFlash),
		"model_pro":   state(configgraph.ResourceModel, deepseek.ModelPro),
	}
}

// --- value builders ---

func inputToProfileFile(in ProfileInput) *profileFile {
	p := &profileFile{DisplayName: in.DisplayName, Slots: map[string]*slotFile{}}
	for _, slotID := range allSlots {
		slot := in.Slots[slotID]
		mode, _ := normalizeSlotMode(slot.Mode, slot.Reasoning)
		p.Slots[slotID] = &slotFile{
			Provider:      strings.TrimSpace(slot.Provider),
			UpstreamModel: strings.TrimSpace(slot.UpstreamModel),
			Mode:          mode,
			Reasoning:     normalizedReasoning(slot.Reasoning),
		}
	}
	return p
}

func extensionValue(table tableFile) map[string]any {
	return map[string]any{
		"enabled": true,
		"config":  extensionConfigValue(table),
	}
}

func extensionConfigValue(table tableFile) map[string]any {
	data, err := json.Marshal(table)
	if err != nil {
		panic(fmt.Sprintf("marshal routing profile table: %v", err))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("unmarshal routing profile table: %v", err))
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}

func routeValue(provider, upstreamModel string) map[string]any {
	return map[string]any{
		"model":        upstreamModel,
		"provider":     provider,
		"display_name": "Moon Bridge",
	}
}

// --- helpers ---

func resource(graph configgraph.Graph, kind configgraph.ResourceKind, id string) *configgraph.Resource {
	for i := range graph.Resources {
		if graph.Resources[i].Kind == kind && graph.Resources[i].ID == id {
			return &graph.Resources[i]
		}
	}
	return nil
}

func valueString(value map[string]any, key string) string {
	if v, ok := value[key].(string); ok {
		return v
	}
	return ""
}

func normalizedReasoning(r *string) *string {
	if r == nil {
		return nil
	}
	v := deepseek.NormalizeReasoningEffort(*r)
	return &v
}

func cloneReasoning(r *string) *string {
	if r == nil {
		return nil
	}
	v := *r
	return &v
}

func sortedKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func valueEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, avv := range av {
			bvv, ok := bv[k]
			if !ok || !valueEqual(avv, bvv) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valueEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	default:
		return false
	}
}

func fieldErrorDetails(errors []configgraph.FieldError) map[string]any {
	if len(errors) == 0 {
		return nil
	}
	items := make([]any, 0, len(errors))
	for _, fe := range errors {
		items = append(items, map[string]any{
			"resourceKind": fe.ResourceKind,
			"resourceId":   fe.ResourceID,
			"field":        fe.Field,
			"message":      fe.Message,
		})
	}
	return map[string]any{"errors": items}
}
