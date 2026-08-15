package main

import (
	"context"
	"errors"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/codexlauncher"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/routingprofile"
	"moonbridge/internal/service/trafficanalysis"
)

// DesktopCommandResult is the single envelope for the DeepSeek / Codex config /
// Codex launcher bindings. OK=true → read Value; OK=false → Value is nil and
// Error is read (safe partial-success info rides in Error.Details, never in
// Value).
type DesktopCommandResult struct {
	OK    bool             `json:"ok"`
	Value *DesktopSnapshot `json:"value,omitempty"`
	Error *CommandError    `json:"error,omitempty"`
}

type DesktopSnapshot struct {
	RouteMutationResult string                   `json:"routeMutationResult,omitempty"`
	CleanupStatus       string                   `json:"cleanupStatus,omitempty"`
	CleanupPending      bool                     `json:"cleanupPending,omitempty"`
	Gateway             *SafeGatewaySnapshot     `json:"gateway,omitempty"`
	Codex               *CodexState              `json:"codex,omitempty"`
	Config              *CodexConfigSnapshot     `json:"config,omitempty"`
	DeepSeek            *DeepSeekSnapshot        `json:"deepseek,omitempty"`
	TrafficAnalysis     *TrafficAnalysisSnapshot `json:"trafficAnalysis,omitempty"`
	SaveDialog          *SaveDialogSnapshot      `json:"saveDialog,omitempty"`
	Export              *TrafficExportSnapshot   `json:"export,omitempty"`
	RevealExport        *TrafficRevealSnapshot   `json:"revealExport,omitempty"`
	Recovery            *RecoverySnapshot        `json:"recovery,omitempty"`
	App                 *AppLifecycleSnapshot    `json:"app,omitempty"`
	Backups             []CodexBackupInfo        `json:"backups,omitempty"`
	TrafficObservations []TrafficObservation     `json:"trafficObservations,omitempty"`
	RoutingProfiles     *RoutingProfileSnapshot  `json:"routingProfiles,omitempty"`
	// Connection-test fields are flat because the command envelope unwraps Value
	// (a *DesktopSnapshot) and the hook reads operationId/result/warning/gatewaySnapshot
	// at the top level. They are present only on a TestDeepSeekConnection result.
	ConnectionTestOperationID    string                  `json:"operationId,omitempty"`
	ConnectionTest               *DeepSeekConnectionTest `json:"result,omitempty"`
	ConnectionTestWarning        *string                 `json:"warning,omitempty"`
	ConnectionGatewaySnapshot    *GatewaySnapshot        `json:"gatewaySnapshot,omitempty"`
	ConnectionGatewayLeftRunning bool                    `json:"gatewayLeftRunning,omitempty"`
}

// DeepSeekConnectionTest is the structured, secret-free connection probe result.
// Code is allowlisted by the gateway; Message is gateway-authored, never an
// upstream error body.
type DeepSeekConnectionTest struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Model   string `json:"model"`
}

type SafeGatewaySnapshot struct {
	State     string `json:"state"`
	Listening bool   `json:"listening"`
}

// RuntimeConfigurationSnapshot is the Desktop-safe view of the currently
// installed Gateway configuration. It intentionally contains no raw config,
// profile, provider, credential, or connection identifiers.
type RuntimeConfigurationSnapshot struct {
	State                 string                 `json:"state"`
	ServerInstance        string                 `json:"serverInstance,omitempty"`
	ResolverGeneration    uint64                 `json:"resolverGeneration"`
	InstallSource         string                 `json:"installSource"`
	ConfigSource          string                 `json:"configSource"`
	ResolverPresent       bool                   `json:"resolverPresent"`
	RoutingExtensionState string                 `json:"routingExtensionState"`
	ActiveProfileState    string                 `json:"activeProfileState"`
	ReadySlotCount        int                    `json:"readySlotCount"`
	CredentialState       string                 `json:"credentialState"`
	Slots                 RuntimeSlotSnapshotSet `json:"slots"`
}

type RuntimeSlotSnapshotSet struct {
	Sol   RuntimeSlotSnapshot `json:"sol"`
	Terra RuntimeSlotSnapshot `json:"terra"`
	Luna  RuntimeSlotSnapshot `json:"luna"`
}

type RuntimeSlotSnapshot struct {
	State            string `json:"state"`
	Provider         string `json:"provider,omitempty"`
	UpstreamModel    string `json:"upstreamModel,omitempty"`
	Mode             string `json:"mode,omitempty"`
	ConfiguredEffort string `json:"configuredEffort,omitempty"`
	CredentialState  string `json:"credentialState,omitempty"`
}

// TrafficAnalysisSnapshot is the deliberately reduced Wails view of the
// long-lived Capture service. It contains state and counts only; identities,
// addresses, hashes, transaction IDs, and raw errors are never exposed.
type TrafficAnalysisSnapshot struct {
	Mode                 string `json:"mode"`
	CaptureState         string `json:"captureState"`
	Operation            string `json:"operation,omitempty"`
	Generation           uint64 `json:"generation"`
	GatewayMatches       bool   `json:"gatewayMatches"`
	RelayActive          bool   `json:"relayActive"`
	IntegrationActive    bool   `json:"integrationActive"`
	Listening            bool   `json:"listening"`
	HTTPRequests         uint64 `json:"httpRequests"`
	SSEStreams           uint64 `json:"sseStreams"`
	WebSocketConnections uint64 `json:"websocketConnections"`
	ObservationCount     uint64 `json:"observationCount"`
	ObservationCapacity  uint64 `json:"observationCapacity"`
	DroppedObservations  uint64 `json:"droppedObservations"`
	UnsavedObservations  bool   `json:"unsavedObservations"`
	AutoSaveStatus       string `json:"autoSaveStatus,omitempty"`
}

// SaveDialogSnapshot is the result of the native Save File Dialog: Path is
// empty and Canceled is true when the user cancelled.
type SaveDialogSnapshot struct {
	Path     string `json:"path"`
	Canceled bool   `json:"canceled"`
}

// TrafficExportSnapshot describes a completed ログを保存 export.
type TrafficExportSnapshot struct {
	OperationID      string `json:"operationId,omitempty"`
	Destination      string `json:"destination"`
	ObservationCount uint64 `json:"observationCount"`
}

// TrafficRevealSnapshot describes a 保存先フォルダーを開く reveal request.
type TrafficRevealSnapshot struct {
	OperationID string `json:"operationId,omitempty"`
	Destination string `json:"destination"`
}

// TrafficObservation is the deliberately reduced, secret-free Desktop summary
// of one recorded observation. Payload observations retain their existing
// reduction; structured gateway events may carry only validated, allowlisted
// routing labels and aliases.
type TrafficObservation struct {
	Kind                   string                   `json:"kind"`
	Sequence               uint64                   `json:"sequence"`
	Timestamp              string                   `json:"timestamp"`
	RequestAlias           string                   `json:"requestAlias,omitempty"`
	Direction              string                   `json:"direction,omitempty"`
	Transport              string                   `json:"transport"`
	Method                 string                   `json:"method,omitempty"`
	StatusCode             int                      `json:"statusCode,omitempty"`
	PayloadKind            string                   `json:"payloadKind"`
	SSEEventType           string                   `json:"sseEventType,omitempty"`
	ContentEncoding        string                   `json:"contentEncoding,omitempty"`
	RawPayloadSize         int                      `json:"rawPayloadSize"`
	DecodedObservationSize int                      `json:"decodedObservationSize"`
	DecodingStatus         string                   `json:"decodingStatus"`
	PayloadShape           *TrafficPayloadShape     `json:"payloadShape,omitempty"`
	Identifiers            TrafficIdentifierSummary `json:"identifiers,omitempty"`
	Partial                bool                     `json:"partial,omitempty"`
	Truncated              bool                     `json:"truncated,omitempty"`
	Disposition            string                   `json:"disposition"`
	ErrorClass             string                   `json:"errorClass,omitempty"`
	Usage                  *TrafficUsageSummary     `json:"usage,omitempty"`
	GatewayEvent           *TrafficGatewayEvent     `json:"gatewayEvent,omitempty"`
}

type TrafficGatewayEvent struct {
	RequestAlias     string                     `json:"requestAlias"`
	RequestedModel   string                     `json:"requestedModel,omitempty"`
	RoutingSlot      string                     `json:"routingSlot,omitempty"`
	ActiveProfile    string                     `json:"activeProfile,omitempty"`
	Provider         string                     `json:"provider,omitempty"`
	UpstreamModel    string                     `json:"upstreamModel,omitempty"`
	Mode             string                     `json:"mode,omitempty"`
	ConfiguredEffort string                     `json:"configuredEffort,omitempty"`
	Protocol         string                     `json:"protocol,omitempty"`
	Model            string                     `json:"model,omitempty"`
	Thinking         string                     `json:"thinking,omitempty"`
	EffectiveEffort  string                     `json:"effectiveEffort,omitempty"`
	CredentialState  string                     `json:"credentialState,omitempty"`
	Direction        string                     `json:"direction,omitempty"`
	StatusCode       int                        `json:"statusCode,omitempty"`
	ExchangeIndex    uint64                     `json:"exchangeIndex,omitempty"`
	Streaming        bool                       `json:"streaming,omitempty"`
	Resolver         *TrafficResolverDiagnostic `json:"resolver,omitempty"`
}

type TrafficResolverDiagnostic struct {
	RequestedModel     string `json:"requestedModel,omitempty"`
	ServerInstance     string `json:"serverInstance,omitempty"`
	ResolverGeneration uint64 `json:"resolverGeneration,omitempty"`
	ResolverPresent    bool   `json:"resolverPresent"`
	InstallSource      string `json:"installSource,omitempty"`
	ConfigSource       string `json:"configSource,omitempty"`
	ExtensionState     string `json:"extensionState,omitempty"`
	ActiveProfileState string `json:"activeProfileState,omitempty"`
	SlotCount          int    `json:"slotCount"`
	SolState           string `json:"solState,omitempty"`
	TerraState         string `json:"terraState,omitempty"`
	LunaState          string `json:"lunaState,omitempty"`
	NormalResult       string `json:"normalResult,omitempty"`
	ResolvedSlot       string `json:"resolvedSlot,omitempty"`
	FallbackResult     string `json:"fallbackResult,omitempty"`
	FinalStage         string `json:"finalStage,omitempty"`
	KnownAlias         bool   `json:"knownAlias"`
}

type TrafficUsageSummary struct {
	InputTokens       *int `json:"inputTokens,omitempty"`
	OutputTokens      *int `json:"outputTokens,omitempty"`
	TotalTokens       *int `json:"totalTokens,omitempty"`
	CachedInputTokens *int `json:"cachedInputTokens,omitempty"`
	ReasoningTokens   *int `json:"reasoningTokens,omitempty"`
}

type TrafficPayloadShape struct {
	TopLevelFields        []string           `json:"topLevelFields,omitempty"`
	RequestModel          string             `json:"requestModel,omitempty"`
	TopLevelTypes         map[string]string  `json:"topLevelTypes,omitempty"`
	ArrayLengths          map[string]int     `json:"arrayLengths,omitempty"`
	ObjectFieldCounts     map[string]int     `json:"objectFieldCounts,omitempty"`
	InputItemCount        int                `json:"inputItemCount,omitempty"`
	InputItemTypeCounts   map[string]int     `json:"inputItemTypeCounts,omitempty"`
	InputRoleCounts       map[string]int     `json:"inputRoleCounts,omitempty"`
	InputItemFingerprints []TrafficInputItem `json:"inputItemFingerprints,omitempty"`
	HasPreviousResponseID bool               `json:"hasPreviousResponseId,omitempty"`
	ToolCount             int                `json:"toolCount,omitempty"`
	ToolTypes             []string           `json:"toolTypes,omitempty"`
	EventType             string             `json:"eventType,omitempty"`
	ObjectType            string             `json:"objectType,omitempty"`
	Status                string             `json:"status,omitempty"`
	ShapeTruncated        bool               `json:"shapeTruncated,omitempty"`
}

type TrafficInputItem struct {
	Index        int                      `json:"index"`
	Fields       []string                 `json:"fields,omitempty"`
	Type         string                   `json:"type,omitempty"`
	Role         string                   `json:"role,omitempty"`
	ContentCount int                      `json:"contentCount,omitempty"`
	ObjectCount  int                      `json:"objectCount,omitempty"`
	ArrayCount   int                      `json:"arrayCount,omitempty"`
	Identifiers  TrafficIdentifierSummary `json:"identifiers,omitempty"`
}

type TrafficIdentifierSummary struct {
	ResponseIDAliases         []string `json:"responseIdAliases,omitempty"`
	PreviousResponseIDAliases []string `json:"previousResponseIdAliases,omitempty"`
	ItemIDAliases             []string `json:"itemIdAliases,omitempty"`
	CallIDAliases             []string `json:"callIdAliases,omitempty"`
	ConversationIDAliases     []string `json:"conversationIdAliases,omitempty"`
	OtherIDAliases            []string `json:"otherIdAliases,omitempty"`
}

func safeTrafficShape(shape *trafficanalysis.PayloadShape) *TrafficPayloadShape {
	if shape == nil {
		return nil
	}
	return &TrafficPayloadShape{
		TopLevelFields:        append([]string(nil), shape.TopLevelFields...),
		RequestModel:          shape.RequestModel,
		TopLevelTypes:         cloneStringMap(shape.TopLevelTypes),
		ArrayLengths:          cloneIntMap(shape.ArrayLengths),
		ObjectFieldCounts:     cloneIntMap(shape.ObjectFieldCounts),
		InputItemCount:        shape.InputItemCount,
		InputItemTypeCounts:   cloneIntMap(shape.InputItemTypeCounts),
		InputRoleCounts:       cloneIntMap(shape.InputRoleCounts),
		InputItemFingerprints: safeTrafficInputItems(shape.InputItemFingerprints),
		HasPreviousResponseID: shape.HasPreviousResponseID,
		ToolCount:             shape.ToolCount,
		ToolTypes:             append([]string(nil), shape.ToolTypes...),
		EventType:             shape.EventType,
		ObjectType:            shape.ObjectType,
		Status:                shape.Status,
		ShapeTruncated:        shape.ShapeTruncated,
	}
}

func safeTrafficInputItems(items []trafficanalysis.InputItemFingerprint) []TrafficInputItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]TrafficInputItem, len(items))
	for index, item := range items {
		out[index] = TrafficInputItem{
			Index: item.Index, Fields: append([]string(nil), item.Fields...), Type: item.Type, Role: item.Role,
			ContentCount: item.ContentCount, ObjectCount: item.ObjectCount, ArrayCount: item.ArrayCount,
			Identifiers: safeTrafficIdentifiers(item.Identifiers),
		}
	}
	return out
}

func safeTrafficIdentifiers(ids trafficanalysis.IdentifierSummary) TrafficIdentifierSummary {
	return TrafficIdentifierSummary{
		ResponseIDAliases:         append([]string(nil), ids.ResponseIDAliases...),
		PreviousResponseIDAliases: append([]string(nil), ids.PreviousResponseIDAliases...),
		ItemIDAliases:             append([]string(nil), ids.ItemIDAliases...),
		CallIDAliases:             append([]string(nil), ids.CallIDAliases...),
		ConversationIDAliases:     append([]string(nil), ids.ConversationIDAliases...),
		OtherIDAliases:            append([]string(nil), ids.OtherIDAliases...),
	}
}

func safeTrafficUsage(usage *trafficanalysis.UsageSummary) *TrafficUsageSummary {
	if usage == nil {
		return nil
	}
	return &TrafficUsageSummary{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		TotalTokens:       usage.TotalTokens,
		CachedInputTokens: usage.CachedInputTokens,
		ReasoningTokens:   usage.ReasoningTokens,
	}
}

func cloneStringMap(value map[string]string) map[string]string {
	if len(value) == 0 {
		return nil
	}
	out := make(map[string]string, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func cloneIntMap(value map[string]int) map[string]int {
	if len(value) == 0 {
		return nil
	}
	out := make(map[string]int, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

type RecoverySnapshot struct {
	Exists               bool   `json:"exists"`
	Phase                string `json:"phase,omitempty"`
	ReconciliationStatus string `json:"reconciliationStatus,omitempty"`
	IntegrationActive    bool   `json:"integrationActive"`
	RestoreRequired      bool   `json:"restoreRequired"`
	RecoveryRequired     bool   `json:"recoveryRequired"`
	Conflict             bool   `json:"conflict"`
	ConfirmationRequired bool   `json:"confirmationRequired"`
	RestartAttempted     bool   `json:"restartAttempted"`
	UnsavedObservations  bool   `json:"unsavedObservations"`
}

type AppLifecycleSnapshot struct {
	Started bool `json:"started"`
	Ready   bool `json:"ready"`
	Closing bool `json:"closing"`
	Closed  bool `json:"closed"`
}

// CodexStatus is the terminal-session lifecycle status.
type CodexStatus string

// CodexState describes one codex terminal session. PID is the PowerShell
// (terminal) process, not the codex binary; ExitCode is likewise the
// terminal's exit code.
type CodexState struct {
	Status     CodexStatus `json:"status"` // idle|starting|running|stopping|stopped|error
	PID        int         `json:"pid"`
	CodexHome  string      `json:"codexHome"`
	StartedAt  string      `json:"startedAt,omitempty"`
	StoppedAt  string      `json:"stoppedAt,omitempty"`
	ExitCode   *int        `json:"exitCode,omitempty"`
	StopReason string      `json:"stopReason,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type CodexConfigSnapshot struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	Model         string `json:"model,omitempty"`
	ModelProvider string `json:"modelProvider,omitempty"`
	BaseURL       string `json:"baseUrl,omitempty"`
}

// DeepSeekSnapshot is the old DeepSeekStatus-compatible shape plus per-model
// config. APIKeyMasked is always a mask form; plaintext never surfaces here.
type DeepSeekSnapshot struct {
	GatewayRunning                bool                `json:"gatewayRunning"`
	ProviderExists                bool                `json:"providerExists"`
	APIKeySet                     bool                `json:"apiKeySet"`
	APIKeyMasked                  string              `json:"apiKeyMasked,omitempty"`
	APIKeyEnv                     string              `json:"apiKeyEnv"`
	CredentialSource              string              `json:"credentialSource"`
	CredentialState               string              `json:"credentialState"`
	CredentialErrorCode           string              `json:"credentialErrorCode,omitempty"`
	Configured                    bool                `json:"configured"`
	Active                        bool                `json:"active"`
	SelectedModel                 string              `json:"selectedModel,omitempty"`
	DefaultModel                  string              `json:"defaultModel"`
	ReasoningEffort               string              `json:"reasoningEffort"`
	ReasoningExplicitlyConfigured bool                `json:"reasoningExplicitlyConfigured"`
	AllowedReasoningEfforts       []string            `json:"allowedReasoningEfforts"`
	RouteAlias                    string              `json:"routeAlias"`
	Pro                           DeepSeekModelConfig `json:"pro"`
	Flash                         DeepSeekModelConfig `json:"flash"`
}

type DeepSeekModelConfig struct {
	ModelID   string   `json:"modelId"`
	Reasoning string   `json:"reasoning"`
	Supported []string `json:"supported"`
}

// RoutingProfileSnapshot is the secret-free Wails view of the Codex routing
// profile table. ActiveProfileID comes from routing_profiles config.active_profile
// (the moonbridge route provider only as a bootstrap fallback when the extension
// is absent); "" means no profile is active. Profiles is never null.
type RoutingProfileSnapshot struct {
	Profiles        []RoutingProfileCard `json:"profiles"`
	ActiveProfileID string               `json:"activeProfileId"`
	GatewayRunning  bool                 `json:"gatewayRunning"`
}

// RoutingProfileCard is one routing profile. Active is backend-confirmed from
// the graph's active profile (config.active_profile, or the route provider only
// for bootstrap), never a local selection.
type RoutingProfileCard struct {
	ID          string        `json:"id"`
	DisplayName string        `json:"displayName"`
	Active      bool          `json:"active"`
	Configured  bool          `json:"configured"`
	Slots       []RoutingSlot `json:"slots"`
}

// RoutingSlot is one Codex routing slot (sol/terra/luna). Reasoning is omitted
// (undefined on the wire) when the slot carries no override (Luna).
type RoutingSlot struct {
	ID            string  `json:"id"`
	DisplayName   string  `json:"displayName"`
	ProviderID    string  `json:"providerId"`
	ProviderLabel string  `json:"providerLabel"`
	UpstreamModel string  `json:"upstreamModel"`
	Mode          string  `json:"mode"`
	Reasoning     *string `json:"reasoning,omitempty"`
}

type CodexBackupInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	CreatedAt string `json:"createdAt"`
	Size      int64  `json:"size"`
}

// ---- envelope constructors ----

func okDesktop(value *DesktopSnapshot) DesktopCommandResult {
	return DesktopCommandResult{OK: true, Value: value}
}

func errDesktop(operation, stage, code, message string, retryable bool) DesktopCommandResult {
	return DesktopCommandResult{
		OK: false,
		Error: &CommandError{
			Operation: operation,
			Stage:     stage,
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	}
}

func hostClosed(operation string) DesktopCommandResult {
	return errDesktop(operation, "host", "codex_host_closed", "desktop host is shut down", false)
}

// ---- snapshot mappers ----

func desktopCodexState(st codexlauncher.State) *CodexState {
	return &CodexState{
		Status:     CodexStatus(st.Status),
		PID:        st.PID,
		CodexHome:  st.CodexHome,
		StartedAt:  publicTime(st.StartedAt),
		StoppedAt:  publicTime(st.StoppedAt),
		ExitCode:   st.ExitCode,
		StopReason: string(st.StopReason),
		Error:      st.Error,
	}
}

func publicTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func desktopCodexConfig(s codexconfig.Snapshot) *CodexConfigSnapshot {
	return &CodexConfigSnapshot{
		Path:          s.Path,
		Exists:        s.Exists,
		Model:         s.Model,
		ModelProvider: s.ModelProvider,
		BaseURL:       s.BaseURL,
	}
}

func desktopDeepSeek(s *deepseek.Snapshot) *DeepSeekSnapshot {
	if s == nil {
		return nil
	}
	return &DeepSeekSnapshot{
		GatewayRunning:                s.GatewayRunning,
		ProviderExists:                s.ProviderExists,
		APIKeySet:                     s.APIKeySet,
		APIKeyMasked:                  s.APIKeyMasked,
		APIKeyEnv:                     s.APIKeyEnv,
		CredentialSource:              s.CredentialSource,
		CredentialState:               s.CredentialState,
		CredentialErrorCode:           s.CredentialErrorCode,
		Configured:                    s.Configured,
		Active:                        s.Active,
		SelectedModel:                 s.SelectedModel,
		DefaultModel:                  s.DefaultModel,
		ReasoningEffort:               s.ReasoningEffort,
		ReasoningExplicitlyConfigured: s.ReasoningExplicitlyConfigured,
		AllowedReasoningEfforts:       s.AllowedReasoningEfforts,
		RouteAlias:                    s.RouteAlias,
		Pro:                           desktopDeepSeekModel(s.Pro),
		Flash:                         desktopDeepSeekModel(s.Flash),
	}
}

func desktopDeepSeekModel(m deepseek.ModelConfig) DeepSeekModelConfig {
	return DeepSeekModelConfig{ModelID: m.ModelID, Reasoning: m.Reasoning, Supported: m.Supported}
}

// desktopRoutingProfiles maps the routingprofile service snapshot to the
// Desktop DTO. The service snapshot is already secret-free; this copy keeps the
// Wails boundary owned by the desktop package so the wire shape can evolve
// without touching the service.
func desktopRoutingProfiles(snap *routingprofile.Snapshot) *RoutingProfileSnapshot {
	if snap == nil {
		return nil
	}
	profiles := make([]RoutingProfileCard, 0, len(snap.Profiles))
	for _, p := range snap.Profiles {
		profiles = append(profiles, RoutingProfileCard{
			ID:          p.ID,
			DisplayName: p.DisplayName,
			Active:      p.Active,
			Configured:  p.Configured,
			Slots:       desktopRoutingSlots(p.Slots),
		})
	}
	return &RoutingProfileSnapshot{
		Profiles:        profiles,
		ActiveProfileID: snap.ActiveProfileID,
		GatewayRunning:  snap.GatewayRunning,
	}
}

func desktopRoutingSlots(slots []routingprofile.Slot) []RoutingSlot {
	out := make([]RoutingSlot, 0, len(slots))
	for _, s := range slots {
		slot := RoutingSlot{
			ID:            s.ID,
			DisplayName:   s.DisplayName,
			ProviderID:    s.ProviderID,
			ProviderLabel: s.ProviderLabel,
			UpstreamModel: s.UpstreamModel,
			Mode:          s.Mode,
		}
		if s.Reasoning != nil {
			r := *s.Reasoning
			slot.Reasoning = &r
		}
		out = append(out, slot)
	}
	return out
}

func desktopBackups(bs []codexconfig.BackupInfo) []CodexBackupInfo {
	out := make([]CodexBackupInfo, 0, len(bs))
	for _, b := range bs {
		out = append(out, CodexBackupInfo{
			ID:        b.ID,
			Name:      b.Name,
			Path:      b.Path,
			CreatedAt: publicTime(b.CreatedAt),
			Size:      b.Size,
		})
	}
	return out
}

// desktopObservations maps internal Observations to the secret-free Desktop
// summary DTO. Fields that could carry prompts, bodies, responses, header
// values, URL paths/query, API keys, or model/provider names are dropped here
// at the backend boundary.
func desktopObservations(items []trafficanalysis.Observation) []TrafficObservation {
	out := make([]TrafficObservation, 0, len(items))
	for _, o := range items {
		out = append(out, TrafficObservation{
			Kind:                   string(o.Kind),
			Sequence:               o.Sequence,
			Timestamp:              publicTime(o.Timestamp),
			RequestAlias:           o.RequestID,
			Direction:              string(o.Direction),
			Transport:              string(o.Transport),
			Method:                 o.Method,
			StatusCode:             o.StatusCode,
			PayloadKind:            string(o.PayloadKind),
			SSEEventType:           o.SSEEventType,
			ContentEncoding:        o.ContentEncoding,
			RawPayloadSize:         o.RawPayloadSize,
			PayloadShape:           safeTrafficShape(o.PayloadShape),
			Identifiers:            safeTrafficIdentifiers(o.Identifiers),
			DecodedObservationSize: o.DecodedObservationSize,
			DecodingStatus:         string(o.DecodingStatus),
			Partial:                o.Partial,
			Truncated:              o.Truncated,
			Disposition:            string(o.Disposition),
			ErrorClass:             o.ErrorClass,
			Usage:                  safeTrafficUsage(o.Usage),
			GatewayEvent:           safeTrafficGatewayEvent(o.GatewayEvent),
		})
	}
	return out
}

func safeTrafficGatewayEvent(event *trafficanalysis.GatewayEventSummary) *TrafficGatewayEvent {
	if event == nil {
		return nil
	}
	var resolver *TrafficResolverDiagnostic
	if event.Resolver != nil {
		r := event.Resolver
		resolver = &TrafficResolverDiagnostic{RequestedModel: r.RequestedModel, ServerInstance: r.ServerInstance, ResolverGeneration: r.ResolverGeneration, ResolverPresent: r.ResolverPresent, InstallSource: r.InstallSource, ConfigSource: r.ConfigSource, ExtensionState: r.ExtensionState, ActiveProfileState: r.ActiveProfileState, SlotCount: r.SlotCount, SolState: r.SolState, TerraState: r.TerraState, LunaState: r.LunaState, NormalResult: r.NormalResult, ResolvedSlot: r.ResolvedSlot, FallbackResult: r.FallbackResult, FinalStage: r.FinalStage, KnownAlias: r.KnownAlias}
	}
	return &TrafficGatewayEvent{RequestAlias: event.RequestAlias, RequestedModel: event.RequestedModel, RoutingSlot: event.RoutingSlot, ActiveProfile: event.ActiveProfile, Provider: event.Provider, UpstreamModel: event.UpstreamModel, Mode: event.Mode, ConfiguredEffort: event.ConfiguredEffort, Protocol: event.Protocol, Model: event.Model, Thinking: event.Thinking, EffectiveEffort: event.EffectiveEffort, CredentialState: event.CredentialState, Direction: string(event.Direction), StatusCode: event.StatusCode, ExchangeIndex: event.ExchangeIndex, Streaming: event.Streaming, Resolver: resolver}
}

// ---- error conversion ----

// deepSeekError maps a deepseek.ServiceError to the CommandError envelope.
// defaultCode is the fallback for the kind classes without a dedicated code
// (e.g. deepseek_load_failed for Load, deepseek_save_failed for Save).
func deepSeekError(operation, stage, defaultCode string, err error) DesktopCommandResult {
	var se *deepseek.ServiceError
	if !errors.As(err, &se) {
		return errDesktop(operation, stage, defaultCode, "deepseek operation failed", true)
	}
	code := defaultCode
	switch se.Kind {
	case deepseek.ServiceErrorKindInvalidInput:
		code = "deepseek_validate_failed"
	case deepseek.ServiceErrorKindAPIKeyRequired:
		code = "deepseek_api_key_required"
	case deepseek.ServiceErrorKindSaveRejected:
		code = "deepseek_save_failed"
	case deepseek.ServiceErrorKindRevisionConflictExceeded:
		code = "deepseek_save_failed"
	case deepseek.ServiceErrorKindVerifyFailed:
		code = "deepseek_save_failed"
	}
	msg := se.Message
	if msg == "" {
		msg = "deepseek operation failed"
	}
	e := &CommandError{
		Operation:       operation,
		Stage:           stage,
		Code:            code,
		Message:         msg,
		Retryable:       se.Retryable,
		MutationStarted: se.MutationStarted,
	}
	if se.Field != nil {
		f := *se.Field
		e.Field = &f
	}
	if len(se.Details) > 0 {
		e.Details = se.Details
	}
	return DesktopCommandResult{OK: false, Error: e}
}

// codexConfigError maps a codexconfig.Error to the CommandError envelope.
func codexConfigError(operation, stage string, err error) DesktopCommandResult {
	var ce *codexconfig.Error
	if !errors.As(err, &ce) {
		return errDesktop(operation, stage, "codex_config_update_failed", "codex config operation failed", true)
	}
	code := "codex_config_update_failed"
	switch ce.Kind {
	case codexconfig.KindNotFound:
		code = "codex_config_not_found"
	case codexconfig.KindParseFailed:
		code = "codex_config_parse_failed"
	case codexconfig.KindValidationFailed:
		code = "codex_config_validation_failed"
	case codexconfig.KindEditUnsupported:
		code = "codex_config_edit_unsupported"
	case codexconfig.KindUpdateFailed:
		code = "codex_config_update_failed"
	case codexconfig.KindVerifyFailed:
		code = "codex_config_verify_failed"
	case codexconfig.KindBackupFailed:
		code = "codex_config_backup_failed"
	case codexconfig.KindRestoreFailed:
		code = "codex_config_restore_failed"
	case codexconfig.KindNoBackups:
		code = "codex_config_no_backups"
	}
	msg := ce.Message
	if msg == "" {
		msg = "codex config operation failed"
	}
	e := &CommandError{Operation: operation, Stage: stage, Code: code, Message: msg}
	if ce.Field != nil {
		f := *ce.Field
		e.Field = &f
	}
	if len(ce.Details) > 0 {
		e.Details = ce.Details
	}
	return DesktopCommandResult{OK: false, Error: e}
}

// codexError maps a codexlauncher.Error to the CommandError envelope. A
// cancellation propagated from the run-scoped context means the host shut down
// mid-operation (the launcher already cleaned up or owns the run).
func codexError(operation, stage string, err error) DesktopCommandResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return hostClosed(operation)
	}
	var le *codexlauncher.Error
	if !errors.As(err, &le) {
		return errDesktop(operation, stage, "codex_start_failed", "codex operation failed", true)
	}
	code := "codex_start_failed"
	switch le.Kind {
	case codexlauncher.KindNotFound:
		code = "codex_not_found"
	case codexlauncher.KindInvalidExecutable:
		code = "codex_invalid_executable"
	case codexlauncher.KindVersionProbeFailed:
		code = "codex_version_probe_failed"
	case codexlauncher.KindRouteNotFound:
		code = "codex_route_not_found"
	case codexlauncher.KindConfigGenerationFailed:
		code = "codex_config_generation_failed"
	case codexlauncher.KindConfigPublishFailed:
		code = "codex_config_publish_failed"
	case codexlauncher.KindAlreadyRunning:
		code = "codex_already_running"
	case codexlauncher.KindStartFailed:
		code = "codex_start_failed"
	case codexlauncher.KindProjectInvalid:
		code = "codex_project_invalid"
	case codexlauncher.KindProjectNotFound:
		code = "codex_project_not_found"
	case codexlauncher.KindProjectNotDirectory:
		code = "codex_project_not_directory"
	case codexlauncher.KindStopFailed:
		code = "codex_stop_failed"
	}
	msg := le.Message
	if msg == "" {
		msg = "codex operation failed"
	}
	retryable := code != "codex_already_running" && code != "codex_stop_failed"
	e := &CommandError{Operation: operation, Stage: stage, Code: code, Message: msg, Retryable: retryable}
	if len(le.Details) > 0 {
		e.Details = le.Details
	}
	return DesktopCommandResult{OK: false, Error: e}
}

// deepSeekGatewayNotRunning is the no-session error for DeepSeek read/write.
func deepSeekGatewayNotRunning(operation string) DesktopCommandResult {
	return errDesktop(operation, "gateway_check", "deepseek_gateway_not_running", "gateway is not running", false)
}

// routingProfileError maps a routingprofile.ServiceError to the CommandError
// envelope. defaultCode is the fallback for kind classes without a dedicated
// code (e.g. routing_profile_load_failed for Load).
func routingProfileError(operation, stage, defaultCode string, err error) DesktopCommandResult {
	var se *routingprofile.ServiceError
	if !errors.As(err, &se) {
		return errDesktop(operation, stage, defaultCode, "routing profile operation failed", true)
	}
	code := defaultCode
	switch se.Kind {
	case routingprofile.KindInvalidInput:
		code = "routing_profile_validate_failed"
	case routingprofile.KindSaveRejected, routingprofile.KindRevisionConflictExceeded, routingprofile.KindVerifyFailed:
		code = "routing_profile_save_failed"
	}
	msg := se.Message
	if msg == "" {
		msg = "routing profile operation failed"
	}
	e := &CommandError{
		Operation:       operation,
		Stage:           stage,
		Code:            code,
		Message:         msg,
		Retryable:       se.Retryable,
		MutationStarted: se.MutationStarted,
	}
	if se.Field != nil {
		f := *se.Field
		e.Field = &f
	}
	if len(se.Details) > 0 {
		e.Details = se.Details
	}
	return DesktopCommandResult{OK: false, Error: e}
}

// routingProfileGatewayNotRunning is the no-session error for routing profile
// read/write (the graph is only reachable through a live gateway session).
func routingProfileGatewayNotRunning(operation string) DesktopCommandResult {
	return errDesktop(operation, "gateway_check", "routing_profile_gateway_not_running", "gateway is not running", false)
}
