// Package deepseek manages the DeepSeek provider settings in the Moon Bridge
// gateway config through the in-process management API. It ports the reconcile
// behavior of the old Tauri deepseek.rs (create missing resources, patch field
// differences, idempotent) and exposes a status snapshot.
package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/provider"
)

const (
	BaseURL    = "https://api.deepseek.com/anthropic"
	Protocol   = "anthropic"
	Version    = "2023-06-01"
	UserAgent  = "moonbridge-desktop/0.1"
	ModelPro   = "deepseek-v4-pro"
	ModelFlash = "deepseek-v4-flash"
	RouteID    = "moonbridge"
	ProviderID = "deepseek"
)

const (
	ReasoningLow  = "low"
	ReasoningHigh = "high"
	ReasoningMax  = "max"

	legacyXHighReasoning = "xhigh"

	// apiKeyMasked is the irreversible mask form the management API returns for
	// a configured api_key. SnapshotFromGraph preserves it and re-masks any
	// plaintext defensively.
	apiKeyMasked = "******"

	maxReconcileAttempts = 3
)

type offerPrices struct {
	input     float64
	output    float64
	cacheRead float64
}

var (
	offerProPrices   = offerPrices{input: 2.0, output: 8.0, cacheRead: 0.2}
	offerFlashPrices = offerPrices{input: 1.0, output: 2.0, cacheRead: 0.02}
)

// AllowedReasoningEfforts returns the reasoning efforts a model accepts.
func AllowedReasoningEfforts(model string) []string {
	switch model {
	case ModelPro:
		return []string{ReasoningHigh, ReasoningMax}
	case ModelFlash:
		return []string{ReasoningLow, ReasoningHigh, ReasoningMax}
	default:
		return nil
	}
}

// NormalizeReasoningEffort maps the legacy xhigh level to max.
func NormalizeReasoningEffort(effort string) string {
	if effort == legacyXHighReasoning {
		return ReasoningMax
	}
	return effort
}

// NormalizeModelReasoningEffort applies model-specific legacy migration rules.
// Anthro Bridge historically accepted low/medium for Pro, then migrated those
// values to high because DeepSeek Pro exposes only high and max.
func NormalizeModelReasoningEffort(model, effort string) string {
	effort = NormalizeReasoningEffort(effort)
	if model == ModelPro && (effort == ReasoningLow || effort == "medium") {
		return ReasoningHigh
	}
	return effort
}

// Input is the save payload. APIKey empty keeps the existing key.
type Input struct {
	APIKey         string  `json:"apiKey,omitempty"`
	APIKeyEnv      *string `json:"apiKeyEnv,omitempty"`
	DefaultModel   string  `json:"defaultModel"` // pro|flash
	ProReasoning   string  `json:"proReasoning"`
	FlashReasoning string  `json:"flashReasoning"`
}

func (in Input) APIKeyEnvValue() string {
	if in.APIKeyEnv == nil {
		return "DEEPSEEK_API_KEY"
	}
	return *in.APIKeyEnv
}

// SelectedModel maps DefaultModel to the route model id.
func (in Input) SelectedModel() string {
	if in.DefaultModel == "flash" {
		return ModelFlash
	}
	return ModelPro
}

// Normalized returns a copy with reasoning levels canonicalized and the API
// key trimmed. Call after Validate.
func (in Input) Normalized() Input {
	in.ProReasoning = NormalizeModelReasoningEffort(ModelPro, in.ProReasoning)
	in.FlashReasoning = NormalizeModelReasoningEffort(ModelFlash, in.FlashReasoning)
	in.APIKey = strings.TrimSpace(in.APIKey)
	if in.APIKeyEnv != nil {
		value := strings.TrimSpace(*in.APIKeyEnv)
		in.APIKeyEnv = &value
	}
	return in
}

// Validate checks the input shape. Model-specific legacy values are normalized
// before the allowed-membership check so Pro low/medium and xhigh are migrated.
func (in Input) Validate() error {
	if in.DefaultModel != "pro" && in.DefaultModel != "flash" {
		return invalidInput("defaultModel", "defaultModel must be \"pro\" or \"flash\"")
	}
	if !contains(AllowedReasoningEfforts(ModelPro), NormalizeModelReasoningEffort(ModelPro, in.ProReasoning)) {
		return invalidInput("proReasoning", "proReasoning must be one of high, max")
	}
	if !contains(AllowedReasoningEfforts(ModelFlash), NormalizeModelReasoningEffort(ModelFlash, in.FlashReasoning)) {
		return invalidInput("flashReasoning", "flashReasoning must be one of low, high, max")
	}
	if key := strings.TrimSpace(in.APIKey); key != "" && (!strings.HasPrefix(key, "sk-") || len(key) < 8) {
		return invalidInput("apiKey", "apiKey must be an sk- prefixed key of at least 8 characters")
	}
	if in.APIKeyEnv != nil {
		value := strings.TrimSpace(*in.APIKeyEnv)
		if value == "" {
			return invalidInput("apiKeyEnv", "apiKeyEnv must not be empty")
		}
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(value) {
			return invalidInput("apiKeyEnv", "apiKeyEnv must be a valid environment variable name")
		}
	}
	return nil
}

// ServiceErrorKind classifies errors returned by the service so the binding
// layer can map them to CommandError codes.
type ServiceErrorKind string

const (
	ServiceErrorKindInvalidInput             ServiceErrorKind = "invalid_input"
	ServiceErrorKindAPIKeyRequired           ServiceErrorKind = "api_key_required"
	ServiceErrorKindGatewayAPIFailed         ServiceErrorKind = "gateway_api_failed"
	ServiceErrorKindSaveRejected             ServiceErrorKind = "save_rejected"
	ServiceErrorKindRevisionConflictExceeded ServiceErrorKind = "revision_conflict_exceeded"
	ServiceErrorKindVerifyFailed             ServiceErrorKind = "verify_failed"
)

// ServiceError is a structured, non-secret error. Message and Details never
// contain the api key, control token, or server token.
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
	return &ServiceError{Kind: ServiceErrorKindInvalidInput, Message: message, Field: &f}
}

// CredentialSource identifies where the provider's effective API key comes
// from: a stored value in the config, an environment variable, or nowhere.
type CredentialSource string

const (
	CredentialSourceStored      CredentialSource = "stored"
	CredentialSourceEnvironment CredentialSource = "environment"
	CredentialSourceNone        CredentialSource = "none"
)

// CredentialState is the runtime status of the provider credential. It is
// derived, never persisted to the config graph: while stopped a stored key is
// not decrypted (no probe), so it reports unverified until the gateway runs or
// a connection test resolves it.
type CredentialState string

const (
	CredentialStateAvailable   CredentialState = "available"
	CredentialStateMissing     CredentialState = "missing"
	CredentialStateUnavailable CredentialState = "unavailable"
	CredentialStateUnverified  CredentialState = "unverified"
)

// CredentialErrorCode classifies why a stored credential is unavailable. It is
// non-secret; it never carries the key or ciphertext.
type CredentialErrorCode string

const (
	CredentialErrorCodeDecryptFailed       CredentialErrorCode = "decrypt_failed"
	CredentialErrorCodeMigrationFailed     CredentialErrorCode = "migration_failed"
	CredentialErrorCodeUnsupportedPlatform CredentialErrorCode = "unsupported_platform"
)

// DeriveCredential computes the credential source and state for a provider. It
// is the single formal derivation shared by SnapshotFromGraph and the desktop
// input preview so a stored key is never inferred from a naive keySet=true.
//
// gatewayRunning controls the stored state: while stopped the stored value is
// not decrypted (no probe) and stays unverified; while running it is available.
// Environment lookups need no decryption and resolve immediately either way.
func DeriveCredential(apiKeyRaw, apiKeyEnv string, gatewayRunning bool, lookupEnv func(string) (string, bool)) (CredentialSource, CredentialState) {
	if apiKeyRaw != "" {
		if gatewayRunning {
			return CredentialSourceStored, CredentialStateAvailable
		}
		return CredentialSourceStored, CredentialStateUnverified
	}
	if apiKeyEnv != "" {
		if value, ok := lookupEnv(apiKeyEnv); ok && value != "" {
			return CredentialSourceEnvironment, CredentialStateAvailable
		}
	}
	return CredentialSourceNone, CredentialStateMissing
}

// Snapshot is the JSON shape surfaced by the DeepSeek bindings. It is
// compatible with the old DeepSeekStatus fields and adds per-model config.
type Snapshot struct {
	GatewayRunning                bool        `json:"gatewayRunning"`
	ProviderExists                bool        `json:"providerExists"`
	APIKeySet                     bool        `json:"apiKeySet"`
	APIKeyMasked                  string      `json:"apiKeyMasked,omitempty"`
	APIKeyEnv                     string      `json:"apiKeyEnv"`
	CredentialSource              string      `json:"credentialSource"`
	CredentialState               string      `json:"credentialState"`
	CredentialErrorCode           string      `json:"credentialErrorCode,omitempty"`
	Configured                    bool        `json:"configured"`
	Active                        bool        `json:"active"`
	SelectedModel                 string      `json:"selectedModel,omitempty"`
	DefaultModel                  string      `json:"defaultModel"`
	ReasoningEffort               string      `json:"reasoningEffort"`
	ReasoningExplicitlyConfigured bool        `json:"reasoningExplicitlyConfigured"`
	AllowedReasoningEfforts       []string    `json:"allowedReasoningEfforts"`
	RouteAlias                    string      `json:"routeAlias"`
	Pro                           ModelConfig `json:"pro"`
	Flash                         ModelConfig `json:"flash"`
}

// ModelConfig describes one DeepSeek model's current reasoning configuration.
type ModelConfig struct {
	ModelID   string   `json:"modelId"`
	Reasoning string   `json:"reasoning"`
	Supported []string `json:"supported"`
}

// SnapshotFromGraph derives a snapshot from a config graph. gatewayRunning is
// set by the caller (true when invoked through a live gateway session).
func SnapshotFromGraph(graph configgraph.Graph, gatewayRunning bool) Snapshot {
	provider := resource(graph, configgraph.ResourceProvider, ProviderID)
	route := resource(graph, configgraph.ResourceRoute, RouteID)
	selected := ""
	if route != nil {
		selected = valueString(route.Value, "model")
	}

	providerExists := provider != nil
	apiKeyRaw := ""
	apiKeyEnv := ""
	if provider != nil {
		apiKeyRaw = valueString(provider.Value, "api_key")
		apiKeyEnv = valueString(provider.Value, "api_key_env")
	}
	if apiKeyEnv == "" {
		apiKeyEnv = "DEEPSEEK_API_KEY"
	}
	apiKeySet := apiKeyRaw != ""
	if !apiKeySet && apiKeyEnv != "" {
		value, ok := os.LookupEnv(apiKeyEnv)
		apiKeySet = ok && value != ""
	}
	credSource, credState := DeriveCredential(apiKeyRaw, apiKeyEnv, gatewayRunning, os.LookupEnv)

	modelsReady := resource(graph, configgraph.ResourceModel, ModelPro) != nil &&
		resource(graph, configgraph.ResourceModel, ModelFlash) != nil
	offersReady := offerMatches(graph, ModelPro) && offerMatches(graph, ModelFlash)
	active := route != nil &&
		valueString(route.Value, "provider") == ProviderID &&
		(selected == ModelPro || selected == ModelFlash)

	reasoning, explicit := modelReasoning(graph, selected)
	if reasoning == "" {
		reasoning = ReasoningHigh
	}
	allowed := AllowedReasoningEfforts(selected)
	if allowed == nil {
		allowed = []string{}
	}

	return Snapshot{
		GatewayRunning:                gatewayRunning,
		ProviderExists:                providerExists,
		APIKeySet:                     apiKeySet,
		APIKeyMasked:                  maskAPIKey(apiKeyRaw),
		APIKeyEnv:                     apiKeyEnv,
		CredentialSource:              string(credSource),
		CredentialState:               string(credState),
		Configured:                    providerExists && apiKeySet && modelsReady && offersReady && active,
		Active:                        active,
		SelectedModel:                 selected,
		DefaultModel:                  defaultModelFromSelected(selected),
		ReasoningEffort:               reasoning,
		ReasoningExplicitlyConfigured: explicit,
		AllowedReasoningEfforts:       allowed,
		RouteAlias:                    RouteID,
		Pro:                           modelConfig(graph, ModelPro),
		Flash:                         modelConfig(graph, ModelFlash),
	}
}

// Service reconciles the DeepSeek provider settings in a config graph through
// a ManagementAPI client.
type Service struct {
	api ManagementAPI
}

func NewService(api ManagementAPI) *Service {
	return &Service{api: api}
}

// TestProvider probes the DeepSeek provider's upstream connection through the
// management API. The gateway resolves the credential with the shared resolver,
// so no key crosses the Wails boundary.
func (s *Service) TestProvider(ctx context.Context) (ConnectionTestResult, error) {
	return s.api.TestProvider(ctx, ProviderID)
}

// Load returns the current DeepSeek snapshot without mutating state. It
// combines the config graph (credential source settings) with the runtime
// credential status the shared resolver recorded at client generation, so a
// stored key that failed to decrypt or migrate reports unavailable instead of
// the optimistic available that graph derivation alone would assume.
func (s *Service) Load(ctx context.Context) (*Snapshot, error) {
	graph, err := s.api.Graph(ctx)
	if err != nil {
		return nil, &ServiceError{
			Kind:      ServiceErrorKindGatewayAPIFailed,
			Message:   "unable to load current configuration",
			Retryable: true,
		}
	}
	snap := SnapshotFromGraph(graph, true)
	s.applyRegistryStatus(&snap, ctx)
	return &snap, nil
}

// applyRegistryStatus overlays the resolver-recorded credential status onto the
// snapshot. Registry entries are authoritative for state: they reflect what the
// runtime actually observed, not what the graph would optimistically imply.
// When the registry has no entry or the status fetch fails, the graph-derived
// state is kept (a configured stored key reports available while running).
func (s *Service) applyRegistryStatus(snap *Snapshot, ctx context.Context) {
	statuses, err := s.api.CredentialStatus(ctx)
	if err != nil {
		return
	}
	for _, info := range statuses {
		if info.ProviderID != ProviderID {
			continue
		}
		switch info.State {
		case provider.StateAvailable:
			snap.CredentialSource = info.Source
			snap.CredentialState = provider.StateAvailable
			snap.CredentialErrorCode = ""
		case provider.StateUnavailable:
			snap.CredentialSource = info.Source
			snap.CredentialState = provider.StateUnavailable
			snap.CredentialErrorCode = info.ErrorCode
		case provider.StateMissing:
			snap.CredentialSource = provider.SourceNone
			snap.CredentialState = provider.StateMissing
			snap.CredentialErrorCode = ""
		}
		return
	}
}

// ApplyMigrationIssues overrides a snapshot's credential fields when a legacy
// plaintext key could not be migrated during the store load. The graph keeps
// the key intact (blanking it would break config validation), so the issue
// records source=stored / state=unavailable / error code — keeping both the
// delete-stored-key and re-entry paths usable. It is the stopped-path
// counterpart to applyRegistryStatus.
func ApplyMigrationIssues(snap *Snapshot, issues []provider.CredentialInfo) {
	for _, iss := range issues {
		if iss.ProviderID != ProviderID {
			continue
		}
		if iss.Source == provider.SourceStored && iss.State == provider.StateUnavailable && iss.ErrorCode != "" {
			snap.CredentialSource = provider.SourceStored
			snap.CredentialState = provider.StateUnavailable
			snap.CredentialErrorCode = iss.ErrorCode
		}
		return
	}
}

// Validate checks the input without requiring a gateway session.
func (s *Service) Validate(input Input) error {
	return input.Validate()
}

// Save reconciles the DeepSeek settings. The whole operation is not atomic: it
// applies a sequence of CreateResource/Patch mutations. Any failure after the
// first committed mutation reports MutationStarted=true. Revision conflicts are
// retried against a fresh graph up to maxReconcileAttempts; after exhaustion a
// residual (non-secret) partial state is reported in Details.
func (s *Service) Save(ctx context.Context, input Input) (*Snapshot, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	input = input.Normalized()

	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		graph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, &ServiceError{
				Kind:      ServiceErrorKindGatewayAPIFailed,
				Message:   "unable to load current configuration",
				Retryable: true,
			}
		}
		if !hasAPIKey(graph) && input.APIKey == "" {
			return nil, &ServiceError{
				Kind:    ServiceErrorKindAPIKeyRequired,
				Message: "DeepSeek API key is required for first-time setup",
			}
		}
		_, outcome := s.reconcile(ctx, graph, input)
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.conflict {
			continue
		}

		finalGraph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, &ServiceError{
				Kind:            ServiceErrorKindGatewayAPIFailed,
				Message:         "configuration saved but could not be re-read for verification",
				MutationStarted: true,
				Retryable:       true,
			}
		}
		if detail, ok := verifyFinalGraph(finalGraph, input); !ok {
			return nil, &ServiceError{
				Kind:            ServiceErrorKindVerifyFailed,
				Message:         "saved configuration does not match the requested state",
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
			Kind:            ServiceErrorKindGatewayAPIFailed,
			Message:         "configuration changed repeatedly",
			MutationStarted: true,
			Retryable:       true,
		}
	}
	return nil, &ServiceError{
		Kind:            ServiceErrorKindRevisionConflictExceeded,
		Message:         "configuration changed repeatedly; retry DeepSeek setup",
		MutationStarted: true,
		Retryable:       true,
		Details:         residualState(finalGraph),
	}
}

// Clear removes the stored DeepSeek API key from the config graph. It is
// idempotent: when the provider is absent or already has no api_key it returns
// the current snapshot unchanged. The api_key_env setting is left untouched so
// an environment variable can take over afterwards.
func (s *Service) Clear(ctx context.Context) (*Snapshot, error) {
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		graph, err := s.api.Graph(ctx)
		if err != nil {
			return nil, &ServiceError{
				Kind:      ServiceErrorKindGatewayAPIFailed,
				Message:   "unable to load current configuration",
				Retryable: true,
			}
		}
		provider := resource(graph, configgraph.ResourceProvider, ProviderID)
		if provider == nil || valueString(provider.Value, "api_key") == "" {
			snap := SnapshotFromGraph(graph, true)
			return &snap, nil
		}
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.Patch(ctx, configgraph.PatchRequest{
				BaseRevision: base,
				Changes: []configgraph.PatchOp{{
					Kind:  configgraph.ResourceProvider,
					ID:    ProviderID,
					Field: "api_key",
					Clear: true,
				}},
			})
		})
		if err != nil {
			return nil, s.mutationError(err, false)
		}
		if conflict {
			continue
		}
		graph = g
		if final := resource(graph, configgraph.ResourceProvider, ProviderID); final != nil && valueString(final.Value, "api_key") != "" {
			return nil, &ServiceError{
				Kind:    ServiceErrorKindVerifyFailed,
				Message: "saved configuration still contains an API key after clear",
			}
		}
		snap := SnapshotFromGraph(graph, true)
		return &snap, nil
	}
	return nil, &ServiceError{
		Kind:      ServiceErrorKindRevisionConflictExceeded,
		Message:   "configuration changed repeatedly; retry clearing the DeepSeek key",
		Retryable: true,
	}
}

type reconcileOutcome struct {
	conflict bool
	mutated  bool
	err      error
}

// reconcile applies the resource mutations. The returned graph is the last
// committed one; on a revision conflict (conflict=true) the caller must refetch
// the graph and recompute the whole reconcile.
func (s *Service) reconcile(ctx context.Context, graph configgraph.Graph, input Input) (configgraph.Graph, reconcileOutcome) {
	out := reconcileOutcome{}

	// 1. models: create missing, patch default_reasoning_level differences.
	for _, model := range []string{ModelPro, ModelFlash} {
		reasoning := input.ProReasoning
		if model == ModelFlash {
			reasoning = input.FlashReasoning
		}
		if resource(graph, configgraph.ResourceModel, model) == nil {
			g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
				return s.api.CreateResource(ctx, base, configgraph.ResourceModel, model, modelValue(model, reasoning))
			})
			if err != nil {
				return graph, reconcileOutcome{mutated: out.mutated, err: s.mutationError(err, out.mutated)}
			}
			if conflict {
				return graph, reconcileOutcome{conflict: true, mutated: out.mutated}
			}
			graph = g
			out.mutated = true
		} else if current := strField(graph, configgraph.ResourceModel, model, "default_reasoning_level"); current != reasoning {
			g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
				return s.api.Patch(ctx, configgraph.PatchRequest{
					BaseRevision: base,
					Changes: []configgraph.PatchOp{{
						Kind: configgraph.ResourceModel, ID: model, Field: "default_reasoning_level", Value: reasoning,
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
		}
	}

	// 2. provider: create if missing, else patch differences (api_key only when
	//    the input carries a key; empty keeps the existing one).
	if resource(graph, configgraph.ResourceProvider, ProviderID) == nil {
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.CreateResource(ctx, base, configgraph.ResourceProvider, ProviderID, providerValue(input.APIKey, input.APIKeyEnvValue()))
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
		current := resource(graph, configgraph.ResourceProvider, ProviderID)
		var changes []configgraph.PatchOp
		add := func(field string, value any) {
			changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceProvider, ID: ProviderID, Field: field, Value: value})
		}
		if valueString(current.Value, "base_url") != BaseURL {
			add("base_url", BaseURL)
		}
		if valueString(current.Value, "protocol") != Protocol {
			add("protocol", Protocol)
		}
		if valueString(current.Value, "version") != Version {
			add("version", Version)
		}
		if valueString(current.Value, "user_agent") != UserAgent {
			add("user_agent", UserAgent)
		}
		extensions := cloneObject(current.Value["extensions"])
		extensions["deepseek_v4"] = map[string]any{"enabled": true}
		if !valueEqual(current.Value["extensions"], extensions) {
			add("extensions", extensions)
		}
		apiKeyEnv := valueString(current.Value, "api_key_env")
		if apiKeyEnv == "" {
			apiKeyEnv = "DEEPSEEK_API_KEY"
		}
		if input.APIKeyEnv != nil {
			apiKeyEnv = *input.APIKeyEnv
		}
		if apiKeyEnv == "" {
			apiKeyEnv = "DEEPSEEK_API_KEY"
		}
		if valueString(current.Value, "api_key_env") != apiKeyEnv {
			add("api_key_env", apiKeyEnv)
		}
		if input.APIKey != "" {
			add("api_key", input.APIKey)
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

	// 3. offers: create missing, patch differences.
	for _, model := range []string{ModelPro, ModelFlash} {
		prices := offerProPrices
		if model == ModelFlash {
			prices = offerFlashPrices
		}
		id := ProviderID + "/" + model
		if resource(graph, configgraph.ResourceProviderOffer, id) == nil {
			g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
				return s.api.CreateResource(ctx, base, configgraph.ResourceProviderOffer, id, offerValue(model, prices))
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
			current := resource(graph, configgraph.ResourceProviderOffer, id)
			expected := offerValue(model, prices)
			var changes []configgraph.PatchOp
			add := func(field string, value any) {
				changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceProviderOffer, ID: id, Field: field, Value: value})
			}
			if valueString(current.Value, "model") != model {
				add("model", model)
			}
			if valueString(current.Value, "upstream_name") != model {
				add("upstream_name", model)
			}
			if !numberEqual(current.Value["priority"], 0) {
				add("priority", 0)
			}
			if !valueEqual(current.Value["pricing"], expected["pricing"]) {
				add("pricing", expected["pricing"])
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
	}

	// 4. route: create if missing, else patch model/provider differences.
	selected := input.SelectedModel()
	if resource(graph, configgraph.ResourceRoute, RouteID) == nil {
		g, conflict, err := s.apply(ctx, graph, func(ctx context.Context, base string) (configgraph.PatchResponse, error) {
			return s.api.CreateResource(ctx, base, configgraph.ResourceRoute, RouteID, routeValue(selected))
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
		current := resource(graph, configgraph.ResourceRoute, RouteID)
		var changes []configgraph.PatchOp
		if valueString(current.Value, "model") != selected {
			changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceRoute, ID: RouteID, Field: "model", Value: selected})
		}
		if valueString(current.Value, "provider") != ProviderID {
			changes = append(changes, configgraph.PatchOp{Kind: configgraph.ResourceRoute, ID: RouteID, Field: "provider", Value: ProviderID})
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
			Kind:    ServiceErrorKindSaveRejected,
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
		Kind:            ServiceErrorKindGatewayAPIFailed,
		Message:         fmt.Sprintf("management API request failed: %v", err),
		MutationStarted: mutated,
		Retryable:       true,
	}
}

func hasAPIKey(graph configgraph.Graph) bool {
	r := resource(graph, configgraph.ResourceProvider, ProviderID)
	if r == nil {
		return false
	}
	return valueString(r.Value, "api_key") != ""
}

// verifyFinalGraph checks the persisted graph against the requested state and
// returns a non-secret mismatch detail when they differ.
func verifyFinalGraph(graph configgraph.Graph, input Input) (string, bool) {
	provider := resource(graph, configgraph.ResourceProvider, ProviderID)
	if provider == nil {
		return "deepseek provider", false
	}
	for _, f := range []struct{ field, want string }{
		{"base_url", BaseURL},
		{"protocol", Protocol},
		{"version", Version},
		{"user_agent", UserAgent},
	} {
		if valueString(provider.Value, f.field) != f.want {
			return "provider " + f.field, false
		}
	}
	if valueString(provider.Value, "api_key") == "" {
		return "provider api_key", false
	}
	if !deepSeekExtensionEnabled(provider.Value["extensions"]) {
		return "deepseek_v4 extension", false
	}

	for _, model := range []string{ModelPro, ModelFlash} {
		reasoning := input.ProReasoning
		if model == ModelFlash {
			reasoning = input.FlashReasoning
		}
		modelRes := resource(graph, configgraph.ResourceModel, model)
		if modelRes == nil {
			return "model " + model, false
		}
		if valueString(modelRes.Value, "default_reasoning_level") != reasoning {
			return "model " + model + " reasoning", false
		}
		id := ProviderID + "/" + model
		if !offerMatches(graph, model) {
			return "offer " + id, false
		}
	}

	route := resource(graph, configgraph.ResourceRoute, RouteID)
	if route == nil {
		return "moonbridge route", false
	}
	if valueString(route.Value, "provider") != ProviderID || valueString(route.Value, "model") != input.SelectedModel() {
		return "moonbridge route target", false
	}
	return "", true
}

func deepSeekExtensionEnabled(extensions any) bool {
	ext, ok := extensions.(map[string]any)
	if !ok {
		return false
	}
	dv, ok := ext["deepseek_v4"].(map[string]any)
	if !ok {
		return false
	}
	enabled, ok := dv["enabled"].(bool)
	return ok && enabled
}

// residualState reports which DeepSeek resources are present after retry
// exhaustion. It never contains secret material.
func residualState(graph configgraph.Graph) map[string]any {
	state := func(kind configgraph.ResourceKind, id string) string {
		if resource(graph, kind, id) != nil {
			return "present"
		}
		return "missing"
	}
	return map[string]any{
		"provider":    state(configgraph.ResourceProvider, ProviderID),
		"model_pro":   state(configgraph.ResourceModel, ModelPro),
		"model_flash": state(configgraph.ResourceModel, ModelFlash),
		"offer_pro":   state(configgraph.ResourceProviderOffer, ProviderID+"/"+ModelPro),
		"offer_flash": state(configgraph.ResourceProviderOffer, ProviderID+"/"+ModelFlash),
		"route":       state(configgraph.ResourceRoute, RouteID),
	}
}

// resource returns a pointer to the matching graph resource, or nil.
func resource(graph configgraph.Graph, kind configgraph.ResourceKind, id string) *configgraph.Resource {
	for i := range graph.Resources {
		if graph.Resources[i].Kind == kind && graph.Resources[i].ID == id {
			return &graph.Resources[i]
		}
	}
	return nil
}

// offerMatches reports whether the persisted offer for model matches the
// requested shape, treating wire-omitted fields as zero. This is the single
// judgment used by both verifyFinalGraph and SnapshotFromGraph so a graph that
// verifies also reports configured=true (no verify-ok-but-snapshot-stale split).
func offerMatches(graph configgraph.Graph, model string) bool {
	offer := resource(graph, configgraph.ResourceProviderOffer, ProviderID+"/"+model)
	if offer == nil {
		return false
	}
	if valueString(offer.Value, "model") != model {
		return false
	}
	if valueString(offer.Value, "upstream_name") != model {
		return false
	}
	expected := offerValue(model, offerPricesFor(model))
	// priority and zero pricing fields are omitted from the wire (omitempty);
	// asNumber treats an absent value as 0 so a missing priority==0 passes.
	if !numberEqual(offer.Value["priority"], expected["priority"]) {
		return false
	}
	return pricingMatches(offer.Value["pricing"], expected["pricing"])
}

func offerPricesFor(model string) offerPrices {
	if model == ModelFlash {
		return offerFlashPrices
	}
	return offerProPrices
}

func strField(graph configgraph.Graph, kind configgraph.ResourceKind, id, field string) string {
	r := resource(graph, kind, id)
	if r == nil {
		return ""
	}
	return valueString(r.Value, field)
}

func valueString(value map[string]any, key string) string {
	if v, ok := value[key].(string); ok {
		return v
	}
	return ""
}

func modelReasoning(graph configgraph.Graph, model string) (string, bool) {
	r := resource(graph, configgraph.ResourceModel, model)
	if r == nil {
		return "", false
	}
	raw := valueString(r.Value, "default_reasoning_level")
	if raw == "" {
		return "", false
	}
	return NormalizeModelReasoningEffort(model, raw), true
}

func modelConfig(graph configgraph.Graph, model string) ModelConfig {
	reasoning, _ := modelReasoning(graph, model)
	if reasoning == "" {
		reasoning = ReasoningHigh
	}
	return ModelConfig{
		ModelID:   model,
		Reasoning: reasoning,
		Supported: AllowedReasoningEfforts(model),
	}
}

func defaultModelFromSelected(selected string) string {
	switch selected {
	case ModelPro:
		return "pro"
	case ModelFlash:
		return "flash"
	}
	return ""
}

// maskAPIKey preserves the irreversible mask form and re-masks any plaintext so
// a plaintext key never reaches a snapshot.
func maskAPIKey(raw string) string {
	if raw == "" {
		return ""
	}
	if raw == apiKeyMasked || raw == "configured" {
		return raw
	}
	return "configured"
}

func modelDisplayName(model string) string {
	if model == ModelPro {
		return "DeepSeek V4 Pro"
	}
	return "DeepSeek V4 Flash"
}

func effortDescription(effort string) string {
	switch effort {
	case ReasoningLow:
		return "Low reasoning effort"
	case ReasoningHigh:
		return "High reasoning effort"
	case ReasoningMax:
		return "Maximum reasoning effort"
	}
	return "Reasoning effort"
}

func modelValue(model, reasoningEffort string) map[string]any {
	var levels []any
	for _, effort := range AllowedReasoningEfforts(model) {
		levels = append(levels, map[string]any{
			"effort":      effort,
			"description": effortDescription(effort),
		})
	}
	return map[string]any{
		"context_window":               1000000,
		"max_output_tokens":            384000,
		"display_name":                 modelDisplayName(model),
		"description":                  "DeepSeek V4 with model-specific low/high/max reasoning effort.",
		"supports_reasoning":           true,
		"default_reasoning_level":      NormalizeReasoningEffort(reasoningEffort),
		"supported_reasoning_levels":   levels,
		"supports_reasoning_summaries": true,
		"default_reasoning_summary":    "auto",
		"input_modalities":             []any{"text"},
	}
}

func providerValue(apiKey string, envNames ...string) map[string]any {
	apiKeyEnv := ""
	if len(envNames) > 0 {
		apiKeyEnv = envNames[0]
	}
	if apiKeyEnv == "" {
		apiKeyEnv = "DEEPSEEK_API_KEY"
	}
	return map[string]any{
		"base_url":    BaseURL,
		"api_key":     apiKey,
		"api_key_env": apiKeyEnv,
		"version":     Version,
		"protocol":    Protocol,
		"user_agent":  UserAgent,
		"extensions":  map[string]any{"deepseek_v4": map[string]any{"enabled": true}},
	}
}

func offerValue(model string, prices offerPrices) map[string]any {
	return map[string]any{
		"model":         model,
		"upstream_name": model,
		"priority":      0,
		"pricing": map[string]any{
			"input_price":       prices.input,
			"output_price":      prices.output,
			"cache_write_price": 1.0,
			"cache_read_price":  prices.cacheRead,
		},
	}
}

func routeValue(selectedModel string) map[string]any {
	return map[string]any{
		"model":        selectedModel,
		"provider":     ProviderID,
		"display_name": "Moon Bridge",
	}
}

// pricingMatches compares pricing maps with numeric equality so float
// round-trips do not produce false mismatches.
func pricingMatches(actual, expected any) bool {
	am, ok := actual.(map[string]any)
	if !ok {
		return false
	}
	em, ok := expected.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"input_price", "output_price", "cache_write_price", "cache_read_price"} {
		av, ok1 := asNumber(am[key])
		ev, ok2 := asNumber(em[key])
		if !ok1 || !ok2 || av != ev {
			return false
		}
	}
	return true
}

// valueEqual compares two graph values, treating JSON numbers consistently.
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
		if _, ok := asNumber(a); ok {
			bf, ok := asNumber(b)
			if !ok {
				return false
			}
			af, _ := asNumber(a)
			return af == bf
		}
		return false
	}
}

func numberEqual(a, b any) bool {
	af, ok := asNumber(a)
	if !ok {
		return false
	}
	bf, ok := asNumber(b)
	if !ok {
		return false
	}
	return af == bf
}

// asNumber normalizes a graph value to a float64. A missing/null value is
// treated as zero: the offer wire format omits zero-valued fields (omitempty),
// so an absent priority or zero pricing field is semantically equal to 0. Only
// numberEqual and pricingMatches rely on this; valueEqual's nil case is
// unaffected (a nil value only matches nil).
func asNumber(v any) (float64, bool) {
	if v == nil {
		return 0, true
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func cloneObject(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	return out
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

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
