// Package trafficanalysis contains the privacy boundary for Codex traffic
// observations. It deliberately has no HTTP or WebSocket dependencies so the
// sanitizer can be tested independently of forwarding.
package trafficanalysis

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
)

const (
	DefaultRingCapacity = 2000
	MaxRawAnalysisBytes = 2 << 20
	MaxDecodedBytes     = 2 << 20
	MaxJSONDepth        = 32
	MaxObjectFields     = 128
	MaxIdentifierHashes = 64
	MaxOpaqueSummaries  = 64
	MaxPathBytes        = 2048
)

type Direction string

const (
	DirectionClientToUpstream Direction = "client_to_upstream"
	DirectionUpstreamToClient Direction = "upstream_to_client"
)

type Transport string

const (
	TransportHTTP      Transport = "http"
	TransportSSE       Transport = "sse"
	TransportWebSocket Transport = "websocket"
)

type PayloadKind string

const (
	PayloadEmpty   PayloadKind = "empty"
	PayloadJSON    PayloadKind = "json"
	PayloadText    PayloadKind = "text"
	PayloadBinary  PayloadKind = "binary"
	PayloadUnknown PayloadKind = "unknown"
)

type ObservationKind string

const (
	ObservationPayload                    ObservationKind = "payload"
	ObservationRoutingResolved            ObservationKind = "routing_resolved"
	ObservationRoutingResolutionDiagnosed ObservationKind = "routing_resolution_diagnosed"
	ObservationProviderRequestPrepared    ObservationKind = "provider_request_prepared"
	ObservationProviderRequestDispatched  ObservationKind = "provider_request_dispatched"
	ObservationProviderResponseReceived   ObservationKind = "provider_response_received"
	ObservationProviderResponseModel     ObservationKind = "provider_response_model"
	ObservationProviderResponseForwarded  ObservationKind = "provider_response_forwarded"
)

type DecodingStatus string

const (
	DecodingIdentity             DecodingStatus = "identity"
	DecodingDecoded              DecodingStatus = "decoded"
	DecodingUnsupported          DecodingStatus = "unsupported_encoding"
	DecodingInvalid              DecodingStatus = "invalid_encoding"
	DecodingRawLimitExceeded     DecodingStatus = "raw_limit_exceeded"
	DecodingDecodedLimitExceeded DecodingStatus = "decoded_limit_exceeded"
)

type Disposition string

const (
	DispositionRecorded            Disposition = "recorded"
	DispositionShapeTruncated      Disposition = "shape_truncated"
	DispositionDroppedCapacity     Disposition = "dropped_capacity"
	DispositionDroppedBackpressure Disposition = "dropped_backpressure"
	DispositionAnalyzerError       Disposition = "analyzer_error"
)

// PayloadInput is the sanitized analyzer input contract. Payload is read by
// the caller and is never retained after Record returns.
type PayloadInput struct {
	Direction Direction
	Transport Transport
	// CorrelationKey is an in-process Capture-to-Gateway bridge. It is mapped
	// to the session-local req#N alias and is never serialized or logged raw.
	CorrelationKey        string
	Method                string
	ReceivedHost          string
	ReceivedPath          string
	UpstreamHost          string
	UpstreamPath          string
	QueryParameterNames   []string
	Headers               http.Header
	ContentType           string
	ContentEncoding       string
	WebSocketMessageType  string
	SSEEventType          string
	StatusCode            int
	Partial               bool
	Payload               []byte
	RequestModelEligible  bool
	rawPayloadSize        int
	rawPayloadHMAC        string
	hasRawPayloadOverride bool
}

// GatewayEventInput is the internal, non-payload observation contract. Raw
// correlation/profile values are accepted only for in-process aliasing and are
// never serialized.
type GatewayEventInput struct {
	Kind             ObservationKind          `json:"kind"`
	CorrelationKey   string                   `json:"-"`
	ProfileID        string                   `json:"-"`
	RequestedModel   string                   `json:"requestedModel,omitempty"`
	RoutingSlot      string                   `json:"routingSlot,omitempty"`
	Provider         string                   `json:"provider,omitempty"`
	UpstreamModel    string                   `json:"upstreamModel,omitempty"`
	Mode             string                   `json:"mode,omitempty"`
	ConfiguredEffort string                   `json:"configuredEffort,omitempty"`
	Protocol         string                   `json:"protocol,omitempty"`
	Model            string                   `json:"model,omitempty"`
	ResponseModel    string                   `json:"responseModel,omitempty"`
	Thinking         string                   `json:"thinking,omitempty"`
	EffectiveEffort  string                   `json:"effectiveEffort,omitempty"`
	CredentialState  string                   `json:"credentialState,omitempty"`
	Direction        Direction                `json:"-"`
	StatusCode       int                      `json:"-"`
	ExchangeIndex    uint64                   `json:"-"`
	Streaming        bool                     `json:"-"`
	Resolver         *ResolverDiagnosticInput `json:"resolver,omitempty"`
}

// ResolverDiagnosticInput is the closed, secret-safe resolver state attached
// to one request-resolution observation. CorrelationKey remains the only raw
// in-process value and is held on GatewayEventInput with json:"-".
type ResolverDiagnosticInput struct {
	RequestedModel     string
	ServerInstance     string
	ResolverGeneration uint64
	ResolverPresent    bool
	InstallSource      string
	ConfigSource       string
	ExtensionState     string
	ActiveProfileState string
	SlotCount          int
	SolState           string
	TerraState         string
	LunaState          string
	BaselineState      string
	NormalResult       string
	ResolvedSlot       string
	FallbackResult     string
	FinalStage         string
	KnownAlias         bool
}

type Observation struct {
	Kind                   ObservationKind      `json:"kind"`
	Sequence               uint64               `json:"sequence"`
	SessionID              string               `json:"sessionId"`
	ConnectionID           string               `json:"connectionId,omitempty"`
	RequestID              string               `json:"requestId,omitempty"`
	Timestamp              time.Time            `json:"timestamp"`
	Direction              Direction            `json:"direction"`
	Transport              Transport            `json:"transport"`
	Method                 string               `json:"method,omitempty"`
	ReceivedHost           string               `json:"receivedHost,omitempty"`
	ReceivedPath           string               `json:"receivedPath,omitempty"`
	UpstreamHost           string               `json:"upstreamHost,omitempty"`
	UpstreamPath           string               `json:"upstreamPath,omitempty"`
	QueryParameterNames    []string             `json:"queryParameterNames,omitempty"`
	ContentType            string               `json:"contentType,omitempty"`
	ContentEncoding        string               `json:"contentEncoding,omitempty"`
	WebSocketMessageType   string               `json:"websocketMessageType,omitempty"`
	SSEEventType           string               `json:"sseEventType,omitempty"`
	StatusCode             int                  `json:"statusCode,omitempty"`
	PayloadKind            PayloadKind          `json:"payloadKind"`
	RawPayloadSize         int                  `json:"rawPayloadSize"`
	RawPayloadHMAC         string               `json:"rawPayloadHmac"`
	DecodedObservationSize int                  `json:"decodedObservationSize"`
	DecodingStatus         DecodingStatus       `json:"decodingStatus"`
	PayloadShape           *PayloadShape        `json:"payloadShape,omitempty"`
	Identifiers            IdentifierSummary    `json:"identifiers"`
	OpaqueFields           []OpaqueFieldSummary `json:"opaqueFields,omitempty"`
	HeaderSummary          HeaderSummary        `json:"headerSummary"`
	Truncated              bool                 `json:"truncated,omitempty"`
	Partial                bool                 `json:"partial,omitempty"`
	Disposition            Disposition          `json:"disposition"`
	ErrorClass             string               `json:"errorClass,omitempty"`
	Usage                  *UsageSummary        `json:"usage,omitempty"`
	GatewayEvent           *GatewayEventSummary `json:"gatewayEvent,omitempty"`
}

type GatewayEventSummary struct {
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
	ResponseModel    string                     `json:"responseModel,omitempty"`
	Thinking         string                     `json:"thinking,omitempty"`
	EffectiveEffort  string                     `json:"effectiveEffort,omitempty"`
	CredentialState  string                     `json:"credentialState,omitempty"`
	Direction        Direction                  `json:"direction,omitempty"`
	StatusCode       int                        `json:"statusCode,omitempty"`
	ExchangeIndex    uint64                     `json:"exchangeIndex,omitempty"`
	Streaming        bool                       `json:"streaming,omitempty"`
	Resolver         *ResolverDiagnosticSummary `json:"resolver,omitempty"`
}

type ResolverDiagnosticSummary struct {
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
	BaselineState      string `json:"baselineState,omitempty"`
	NormalResult       string `json:"normalResult,omitempty"`
	ResolvedSlot       string `json:"resolvedSlot,omitempty"`
	FallbackResult     string `json:"fallbackResult,omitempty"`
	FinalStage         string `json:"finalStage,omitempty"`
	KnownAlias         bool   `json:"knownAlias"`
}

type HeaderSummary struct {
	PresentNames         []string `json:"presentNames,omitempty"`
	ContentType          string   `json:"contentType,omitempty"`
	ContentEncoding      string   `json:"contentEncoding,omitempty"`
	Upgrade              string   `json:"upgrade,omitempty"`
	ConnectionClass      string   `json:"connectionClass,omitempty"`
	UserAgentProduct     string   `json:"userAgentProduct,omitempty"`
	AuthorizationPresent bool     `json:"authorizationPresent,omitempty"`
	CookiePresent        bool     `json:"cookiePresent,omitempty"`
	SetCookiePresent     bool     `json:"setCookiePresent,omitempty"`
}

type PayloadShape struct {
	TopLevelFields            []string               `json:"topLevelFields,omitempty"`
	UnknownTopLevelFieldHMACs []string               `json:"unknownTopLevelFieldHmacs,omitempty"`
	TopLevelTypes             map[string]string      `json:"topLevelTypes,omitempty"`
	ArrayLengths              map[string]int         `json:"arrayLengths,omitempty"`
	ObjectFieldCounts         map[string]int         `json:"objectFieldCounts,omitempty"`
	ModelValue                string                 `json:"modelValue,omitempty"`
	StreamValue               *bool                  `json:"streamValue,omitempty"`
	ReasoningEffort           string                 `json:"reasoningEffort,omitempty"`
	ReasoningSummary          string                 `json:"reasoningSummary,omitempty"`
	RequestModel              string                 `json:"requestModel,omitempty"`
	ToolCount                 int                    `json:"toolCount,omitempty"`
	ToolTypes                 []string               `json:"toolTypes,omitempty"`
	EventType                 string                 `json:"eventType,omitempty"`
	ObjectType                string                 `json:"objectType,omitempty"`
	Status                    string                 `json:"status,omitempty"`
	ShapeTruncated            bool                   `json:"shapeTruncated,omitempty"`
	InputItemCount            int                    `json:"inputItemCount,omitempty"`
	InputItemTypeCounts       map[string]int         `json:"inputItemTypeCounts,omitempty"`
	InputRoleCounts           map[string]int         `json:"inputRoleCounts,omitempty"`
	InputItemFingerprints     []InputItemFingerprint `json:"inputItemFingerprints,omitempty"`
	HasPreviousResponseID     bool                   `json:"hasPreviousResponseId,omitempty"`

	identifiers IdentifierSummary
	opaque      []OpaqueFieldSummary
}

func (s *PayloadShape) Identifiers() *IdentifierSummary { return &s.identifiers }
func (s *PayloadShape) OpaqueFields() []OpaqueFieldSummary {
	return s.opaque
}

type IdentifierSummary struct {
	ResponseIDHMACs           []string `json:"-"`
	ResponseIDAliases         []string `json:"responseIdAliases,omitempty"`
	PreviousResponseIDHMACs   []string `json:"-"`
	PreviousResponseIDAliases []string `json:"previousResponseIdAliases,omitempty"`
	ItemIDHMACs               []string `json:"-"`
	ItemIDAliases             []string `json:"itemIdAliases,omitempty"`
	CallIDHMACs               []string `json:"-"`
	CallIDAliases             []string `json:"callIdAliases,omitempty"`
	ConversationIDHMACs       []string `json:"-"`
	ConversationIDAliases     []string `json:"conversationIdAliases,omitempty"`
	OtherIDHMACs              []string `json:"-"`
	OtherIDAliases            []string `json:"otherIdAliases,omitempty"`
}

type OpaqueFieldSummary struct {
	FieldPath         string `json:"fieldPath"`
	ValueType         string `json:"valueType"`
	Size              int    `json:"size"`
	OpaqueContentHMAC string `json:"opaqueContentHmac"`
}

type InputItemFingerprint struct {
	Index        int               `json:"index"`
	Fields       []string          `json:"fields,omitempty"`
	Type         string            `json:"type,omitempty"`
	Role         string            `json:"role,omitempty"`
	ContentCount int               `json:"contentCount,omitempty"`
	ObjectCount  int               `json:"objectCount,omitempty"`
	ArrayCount   int               `json:"arrayCount,omitempty"`
	Identifiers  IdentifierSummary `json:"identifiers,omitempty"`
}

// UsageSummary holds the safe numeric usage extracted from a single SSE event.
// Pointer fields distinguish "present with value 0" from "absent".
type UsageSummary struct {
	InputTokens       *int `json:"inputTokens,omitempty"`
	OutputTokens      *int `json:"outputTokens,omitempty"`
	TotalTokens       *int `json:"totalTokens,omitempty"`
	CachedInputTokens *int `json:"cachedInputTokens,omitempty"`
	ReasoningTokens   *int `json:"reasoningTokens,omitempty"`
}

// maxUsageTokens is the safety bound for any single usage field value.
// Exceeding this causes the field to be ignored.
const maxUsageTokens = 1_000_000_000

type Analyzer struct {
	key              []byte
	session          string
	buffer           *RingBuffer
	aliasMu          sync.Mutex
	aliases          map[string]string
	nextAlias        uint64
	requestAliases   map[string]string
	profileAliases   map[string]string
	nextRequestAlias uint64
	nextProfileAlias uint64
}

// NewAnalyzer creates a session-scoped analyzer. The HMAC key is held only in
// memory and is not part of the exported or formatted state.
func NewAnalyzer(capacity int) (*Analyzer, error) {
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate analysis key: %w", err)
	}
	return &Analyzer{key: key, session: uuid.NewString(), buffer: NewRingBuffer(capacity), aliases: make(map[string]string), requestAliases: make(map[string]string), profileAliases: make(map[string]string)}, nil
}

// String and GoString prevent accidental formatting of the private HMAC key.
func (a *Analyzer) String() string   { return "traffic analysis session" }
func (a *Analyzer) GoString() string { return "traffic analysis session" }

func (a *Analyzer) SessionID() string { return a.session }

func (a *Analyzer) Record(input PayloadInput) Observation {
	obs := analyzePayload(a.key, input)
	obs.Kind = ObservationPayload
	obs.Identifiers = a.aliasIdentifiers(obs.Identifiers)
	if obs.PayloadShape != nil {
		a.aliasInputFingerprints(obs.PayloadShape.InputItemFingerprints)
	}
	obs.SessionID = a.session
	obs.Timestamp = time.Now().UTC()
	obs.ConnectionID = a.hmacString(uuid.NewString())
	if input.CorrelationKey != "" {
		a.aliasMu.Lock()
		obs.RequestID = a.aliasGatewayKey(a.requestAliases, input.CorrelationKey, "req", &a.nextRequestAlias)
		a.aliasMu.Unlock()
	} else {
		obs.RequestID = a.hmacString(uuid.NewString())
	}
	return a.buffer.Append(obs)
}

func (a *Analyzer) RecordGatewayEvent(input GatewayEventInput) Observation {
	kind := sanitizeObservationKind(input.Kind)
	if kind == "" {
		return Observation{}
	}
	a.aliasMu.Lock()
	requestAlias := a.aliasGatewayKey(a.requestAliases, input.CorrelationKey, "req", &a.nextRequestAlias)
	profileAlias := ""
	if input.ProfileID != "" {
		profileAlias = a.aliasGatewayKey(a.profileAliases, input.ProfileID, "profile", &a.nextProfileAlias)
	}
	a.aliasMu.Unlock()

	direction := input.Direction
	if direction == "" {
		direction = DirectionClientToUpstream
	}
	var resolver *ResolverDiagnosticSummary
	if input.Resolver != nil {
		resolver = safeResolverDiagnostic(input.Resolver)
	}
	requestedModel := safeRequestedModel(input.RequestedModel)
	if kind == ObservationRoutingResolutionDiagnosed {
		requestedModel = ""
	}
	obs := Observation{Kind: kind, SessionID: a.session, Timestamp: time.Now().UTC(), Direction: direction, Transport: TransportHTTP, StatusCode: input.StatusCode, PayloadKind: PayloadEmpty, DecodingStatus: DecodingIdentity, Disposition: DispositionRecorded, RequestID: requestAlias, GatewayEvent: &GatewayEventSummary{
		RequestAlias: requestAlias, RequestedModel: requestedModel, RoutingSlot: safeEnum(input.RoutingSlot, "sol", "terra", "luna"), ActiveProfile: profileAlias, Provider: safeIdentifier(input.Provider), UpstreamModel: safeIdentifier(input.UpstreamModel), Mode: safeEnum(input.Mode, "normal", "thinking"), ConfiguredEffort: safeEnumDefault(input.ConfiguredEffort, "none", "high", "max"), Protocol: safeEnum(input.Protocol, "anthropic", "openai-chat", "google-genai", "openai-response"), Model: safeIdentifier(input.Model), ResponseModel: safeIdentifier(input.ResponseModel), Thinking: safeEnumDefault(input.Thinking, "none", "enabled", "disabled", "not_applicable"), EffectiveEffort: safeEnumDefault(input.EffectiveEffort, "none", "high", "max"), CredentialState: safeCredentialState(input.CredentialState), Resolver: resolver,
		Direction: direction, StatusCode: input.StatusCode, ExchangeIndex: input.ExchangeIndex, Streaming: input.Streaming,
	}}
	return a.buffer.Append(obs)
}

func (a *Analyzer) aliasGatewayKey(m map[string]string, raw, prefix string, next *uint64) string {
	if raw == "" {
		return ""
	}
	if alias, ok := m[raw]; ok {
		return alias
	}
	*next = *next + 1
	alias := fmt.Sprintf("%s#%d", prefix, *next)
	m[raw] = alias
	return alias
}

func sanitizeObservationKind(kind ObservationKind) ObservationKind {
	if kind == ObservationRoutingResolved || kind == ObservationRoutingResolutionDiagnosed || kind == ObservationProviderRequestPrepared || kind == ObservationProviderRequestDispatched || kind == ObservationProviderResponseReceived || kind == ObservationProviderResponseModel || kind == ObservationProviderResponseForwarded {
		return kind
	}
	return ""
}

func safeResolverDiagnostic(input *ResolverDiagnosticInput) *ResolverDiagnosticSummary {
	if input == nil {
		return nil
	}
	return &ResolverDiagnosticSummary{
		RequestedModel: safeResolverRequestedModel(input.RequestedModel), ServerInstance: safeServerInstance(input.ServerInstance), ResolverGeneration: input.ResolverGeneration, ResolverPresent: input.ResolverPresent,
		InstallSource: safeEnumDefault(input.InstallSource, "none", "startup", "profile_refresh"), ConfigSource: safeEnumDefault(input.ConfigSource, "unknown", "file_seed", "persisted_store"),
		ExtensionState: safeEnumDefault(input.ExtensionState, "unknown", "absent", "valid", "invalid"), ActiveProfileState: safeEnumDefault(input.ActiveProfileState, "unknown", "present_valid", "missing", "unknown", "invalid"), SlotCount: clampSlotCount(input.SlotCount),
		SolState: safeEnumDefault(input.SolState, "unknown", "ready", "missing", "invalid", "reference_unresolved"), TerraState: safeEnumDefault(input.TerraState, "unknown", "ready", "missing", "invalid", "reference_unresolved"), LunaState: safeEnumDefault(input.LunaState, "unknown", "ready", "missing", "invalid", "reference_unresolved"), BaselineState: safeEnumDefault(input.BaselineState, "unknown", "ready", "missing", "invalid"),
		NormalResult: safeEnumDefault(input.NormalResult, "unknown", "explicit_route", "slot_hit", "resolver_absent", "alias_miss", "slot_target_unresolved"), ResolvedSlot: safeEnumDefault(input.ResolvedSlot, "unknown", "sol", "terra", "luna", "unknown"), FallbackResult: safeEnumDefault(input.FallbackResult, "not_consulted", "hit", "miss", "target_unresolved", "not_consulted"), FinalStage: safeEnumDefault(input.FinalStage, "unknown", "explicit_route", "exact_slot", "fallback", "not_found"), KnownAlias: input.KnownAlias,
	}
}

func clampSlotCount(value int) int {
	if value < 0 {
		return 0
	}
	if value > 3 {
		return 3
	}
	return value
}

func safeResolverRequestedModel(value string) string {
	switch value {
	case "gpt-5.6-sol", "known_sol":
		return "known_sol"
	case "gpt-5.6-terra", "known_terra":
		return "known_terra"
	case "gpt-5.6-luna", "known_luna":
		return "known_luna"
	default:
		return "unknown"
	}
}

func safeServerInstance(value string) string {
	if !strings.HasPrefix(value, "server#") || len(value) <= len("server#") || !digitsOnly(value[len("server#"):]) {
		return "unknown"
	}
	return value
}

func digitsOnly(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func safeRequestedModel(value string) string {
	switch value {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return value
	}
	return "unknown"
}

func safeIdentifier(value string) string {
	if value == "" || len(value) > 128 || !tokenPattern.MatchString(value) {
		return "unknown"
	}
	return value
}

func safeEnum(value string, allowed ...string) string {
	for _, item := range allowed {
		if value == item {
			return value
		}
	}
	return "unknown"
}

func safeEnumDefault(value, zero string, allowed ...string) string {
	if value == "" {
		return zero
	}
	return safeEnum(value, allowed...)
}

func safeCredentialState(value string) string {
	switch value {
	case "available", "missing", "unavailable", "unverified":
		return value
	default:
		return "unknown"
	}
}

func (a *Analyzer) Snapshot(after uint64) ([]Observation, uint64) {
	return a.buffer.Snapshot(after)
}

func (a *Analyzer) DroppedCapacity() uint64 { return a.buffer.Dropped() }
func (a *Analyzer) Capacity() int           { return a.buffer.Capacity() }
func (a *Analyzer) Clear()                  { a.buffer.Clear() }

func (a *Analyzer) hmacString(value string) string {
	return hmacHex(a.key, []byte(value))
}

func (a *Analyzer) aliasIdentifiers(ids IdentifierSummary) IdentifierSummary {
	a.aliasMu.Lock()
	defer a.aliasMu.Unlock()
	return a.aliasIdentifiersLocked(ids)
}

func (a *Analyzer) aliasInputFingerprints(fingerprints []InputItemFingerprint) {
	a.aliasMu.Lock()
	defer a.aliasMu.Unlock()
	for i := range fingerprints {
		fingerprints[i].Identifiers = a.aliasIdentifiersLocked(fingerprints[i].Identifiers)
	}
}

func (a *Analyzer) aliasIdentifiersLocked(ids IdentifierSummary) IdentifierSummary {
	ids.ResponseIDAliases = a.aliasList(ids.ResponseIDHMACs)
	ids.PreviousResponseIDAliases = a.aliasList(ids.PreviousResponseIDHMACs)
	ids.ItemIDAliases = a.aliasList(ids.ItemIDHMACs)
	ids.CallIDAliases = a.aliasList(ids.CallIDHMACs)
	ids.ConversationIDAliases = a.aliasList(ids.ConversationIDHMACs)
	ids.OtherIDAliases = a.aliasList(ids.OtherIDHMACs)
	ids.ResponseIDHMACs = nil
	ids.PreviousResponseIDHMACs = nil
	ids.ItemIDHMACs = nil
	ids.CallIDHMACs = nil
	ids.ConversationIDHMACs = nil
	ids.OtherIDHMACs = nil
	return ids
}

func (a *Analyzer) aliasList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		alias, ok := a.aliases[value]
		if !ok {
			a.nextAlias++
			alias = fmt.Sprintf("id#%d", a.nextAlias)
			a.aliases[value] = alias
		}
		out = append(out, alias)
	}
	return out
}

type RingBuffer struct {
	mu       sync.Mutex
	capacity int
	next     uint64
	dropped  uint64
	items    []Observation
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	return &RingBuffer{capacity: capacity, items: make([]Observation, 0, capacity)}
}

func (r *RingBuffer) Append(obs Observation) Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	obs.Sequence = r.next
	if len(r.items) == r.capacity {
		copy(r.items, r.items[1:])
		r.items = r.items[:len(r.items)-1]
		r.dropped++
	}
	r.items = append(r.items, cloneObservation(obs))
	return obs
}

func (r *RingBuffer) Snapshot(after uint64) ([]Observation, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]Observation, 0, len(r.items))
	for _, item := range r.items {
		if item.Sequence > after {
			items = append(items, cloneObservation(item))
		}
	}
	return items, r.dropped
}

func (r *RingBuffer) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

func (r *RingBuffer) Capacity() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capacity
}

func (r *RingBuffer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = r.items[:0]
}

func cloneObservation(in Observation) Observation {
	out := in
	out.QueryParameterNames = append([]string(nil), in.QueryParameterNames...)
	out.OpaqueFields = append([]OpaqueFieldSummary(nil), in.OpaqueFields...)
	out.HeaderSummary.PresentNames = append([]string(nil), in.HeaderSummary.PresentNames...)
	out.Identifiers = cloneIdentifiers(in.Identifiers)
	if in.PayloadShape != nil {
		shape := *in.PayloadShape
		shape.RequestModel = in.PayloadShape.RequestModel
		shape.TopLevelFields = append([]string(nil), in.PayloadShape.TopLevelFields...)
		shape.UnknownTopLevelFieldHMACs = append([]string(nil), in.PayloadShape.UnknownTopLevelFieldHMACs...)
		shape.ToolTypes = append([]string(nil), in.PayloadShape.ToolTypes...)
		shape.TopLevelTypes = cloneStringMap(in.PayloadShape.TopLevelTypes)
		shape.ArrayLengths = cloneIntMap(in.PayloadShape.ArrayLengths)
		shape.ObjectFieldCounts = cloneIntMap(in.PayloadShape.ObjectFieldCounts)
		shape.InputItemTypeCounts = cloneIntMap(in.PayloadShape.InputItemTypeCounts)
		shape.InputRoleCounts = cloneIntMap(in.PayloadShape.InputRoleCounts)
		shape.InputItemFingerprints = cloneInputFingerprints(in.PayloadShape.InputItemFingerprints)
		shape.identifiers = cloneIdentifiers(in.PayloadShape.identifiers)
		shape.opaque = append([]OpaqueFieldSummary(nil), in.PayloadShape.opaque...)
		if in.PayloadShape.StreamValue != nil {
			value := *in.PayloadShape.StreamValue
			shape.StreamValue = &value
		}
		out.PayloadShape = &shape
	}
	if in.Usage != nil {
		usage := *in.Usage
		out.Usage = &usage
	}
	if in.GatewayEvent != nil {
		event := *in.GatewayEvent
		out.GatewayEvent = &event
	}
	return out
}

func cloneIdentifiers(in IdentifierSummary) IdentifierSummary {
	return IdentifierSummary{
		ResponseIDHMACs:           append([]string(nil), in.ResponseIDHMACs...),
		ResponseIDAliases:         append([]string(nil), in.ResponseIDAliases...),
		PreviousResponseIDHMACs:   append([]string(nil), in.PreviousResponseIDHMACs...),
		PreviousResponseIDAliases: append([]string(nil), in.PreviousResponseIDAliases...),
		ItemIDHMACs:               append([]string(nil), in.ItemIDHMACs...),
		ItemIDAliases:             append([]string(nil), in.ItemIDAliases...),
		CallIDHMACs:               append([]string(nil), in.CallIDHMACs...),
		CallIDAliases:             append([]string(nil), in.CallIDAliases...),
		ConversationIDHMACs:       append([]string(nil), in.ConversationIDHMACs...),
		ConversationIDAliases:     append([]string(nil), in.ConversationIDAliases...),
		OtherIDHMACs:              append([]string(nil), in.OtherIDHMACs...),
		OtherIDAliases:            append([]string(nil), in.OtherIDAliases...),
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func analyzePayload(key []byte, input PayloadInput) Observation {
	contentType := normalizeMediaType(input.ContentType)
	if contentType == "" && input.Headers != nil {
		contentType = normalizeMediaType(input.Headers.Get("Content-Type"))
	}
	contentEncoding := strings.ToLower(strings.TrimSpace(input.ContentEncoding))
	if contentEncoding == "" && input.Headers != nil {
		contentEncoding = strings.ToLower(strings.TrimSpace(input.Headers.Get("Content-Encoding")))
	}
	obs := Observation{
		Direction:            input.Direction,
		Transport:            input.Transport,
		Method:               safeToken(input.Method),
		ReceivedHost:         safeHost(input.ReceivedHost),
		ReceivedPath:         safePath(input.ReceivedPath, key),
		UpstreamHost:         safeHost(input.UpstreamHost),
		UpstreamPath:         safePath(input.UpstreamPath, key),
		QueryParameterNames:  safeQueryNames(input.QueryParameterNames),
		ContentType:          contentType,
		ContentEncoding:      safeContentEncoding(contentEncoding),
		WebSocketMessageType: safeMessageType(input.WebSocketMessageType),
		SSEEventType:         safeEventType(input.SSEEventType),
		StatusCode:           input.StatusCode,
		Partial:              input.Partial,
		HeaderSummary:        summarizeHeaders(input.Headers),
		RawPayloadSize:       len(input.Payload),
		RawPayloadHMAC:       hmacHex(key, input.Payload),
		DecodingStatus:       DecodingIdentity,
		PayloadKind:          inferPayloadKind(contentType, input.Payload),
		Disposition:          DispositionRecorded,
	}
	if input.hasRawPayloadOverride {
		obs.RawPayloadSize = input.rawPayloadSize
		obs.RawPayloadHMAC = input.rawPayloadHMAC
	}

	analysisBytes := input.Payload
	if len(input.Payload) > MaxRawAnalysisBytes {
		obs.Truncated = true
		obs.DecodingStatus = DecodingRawLimitExceeded
		obs.Disposition = DispositionShapeTruncated
		analysisBytes = input.Payload[:MaxRawAnalysisBytes]
	} else if contentEncoding == "gzip" {
		decoded, status := decodeGzipBounded(input.Payload)
		obs.DecodingStatus = status
		if status == DecodingDecoded {
			analysisBytes = decoded
		} else {
			analysisBytes = nil
			if status == DecodingDecodedLimitExceeded {
				obs.Truncated = true
				obs.Disposition = DispositionShapeTruncated
			}
		}
	} else if contentEncoding == "zstd" {
		decoded, status := decodeZstdBounded(input.Payload)
		obs.DecodingStatus = status
		if status == DecodingDecoded {
			analysisBytes = decoded
		} else {
			analysisBytes = nil
			if status == DecodingDecodedLimitExceeded {
				obs.Truncated = true
				obs.Disposition = DispositionShapeTruncated
			}
		}
	} else if contentEncoding != "" && contentEncoding != "identity" {
		obs.DecodingStatus = DecodingUnsupported
		analysisBytes = nil
	}
	if obs.DecodingStatus == DecodingDecoded || obs.DecodingStatus == DecodingIdentity {
		obs.PayloadKind = inferPayloadKind(contentType, analysisBytes)
	}
	obs.DecodedObservationSize = len(analysisBytes)
	if shape, ok := buildShape(key, analysisBytes, input.RequestModelEligible); ok {
		obs.PayloadShape = shape
		obs.Identifiers = cloneIdentifiers(shape.identifiers)
		obs.OpaqueFields = append([]OpaqueFieldSummary(nil), shape.opaque...)
		if shape.ShapeTruncated {
			obs.Truncated = true
			obs.Disposition = DispositionShapeTruncated
		}
	}
	if obs.SSEEventType == "response.completed" {
		obs.Usage = extractUsageSummary(analysisBytes)
	}
	return obs
}

// extractUsageSummary extracts safe numeric usage from a single event JSON
// payload. It mirrors the existing server semantics: response.usage is preferred;
// top-level usage is the fallback. Both are checked for validity (at least one of
// input_tokens, output_tokens, or cached_tokens must be non-zero).
//
// SSE framing is not handled here — Capture already splits events and passes
// the decoded data JSON directly.
func extractUsageSummary(payload []byte) *UsageSummary {
	if len(payload) == 0 {
		return nil
	}
	var envelope struct {
		Usage    json.RawMessage `json:"usage"`
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil
	}
	if len(envelope.Response.Usage) > 0 {
		if u := parseUsageJSON(envelope.Response.Usage); u != nil {
			return u
		}
	}
	if len(envelope.Usage) > 0 {
		if u := parseUsageJSON(envelope.Usage); u != nil {
			return u
		}
	}
	return nil
}

// parseUsageJSON parses a usage JSON object and returns a UsageSummary.
// Returns nil when the usage is empty (all relevant fields zero/absent) or malformed.
//
// Allowed JSON paths (fixed allowlist):
//
//	input_tokens
//	output_tokens
//	total_tokens
//	input_tokens_details.cached_tokens
//	output_tokens_details.reasoning_tokens
func parseUsageJSON(data json.RawMessage) *UsageSummary {
	var raw struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
		InputDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	// Mirror server validity check: at least one of these must be non-zero.
	if raw.InputTokens == 0 && raw.OutputTokens == 0 && raw.InputDetails.CachedTokens == 0 {
		return nil
	}
	u := &UsageSummary{}
	if raw.InputTokens > 0 && raw.InputTokens <= maxUsageTokens {
		v := raw.InputTokens
		u.InputTokens = &v
	}
	if raw.OutputTokens > 0 && raw.OutputTokens <= maxUsageTokens {
		v := raw.OutputTokens
		u.OutputTokens = &v
	}
	if raw.TotalTokens > 0 && raw.TotalTokens <= maxUsageTokens {
		v := raw.TotalTokens
		u.TotalTokens = &v
	}
	if raw.InputDetails.CachedTokens > 0 && raw.InputDetails.CachedTokens <= maxUsageTokens {
		v := raw.InputDetails.CachedTokens
		u.CachedInputTokens = &v
	}
	if raw.OutputDetails.ReasoningTokens > 0 && raw.OutputDetails.ReasoningTokens <= maxUsageTokens {
		v := raw.OutputDetails.ReasoningTokens
		u.ReasoningTokens = &v
	}
	return u
}

func decodeZstdBounded(payload []byte) ([]byte, DecodingStatus) {
	reader, err := zstd.NewReader(bytes.NewReader(payload),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(MaxDecodedBytes),
	)
	if err != nil {
		return nil, DecodingInvalid
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, MaxDecodedBytes+1))
	if err != nil {
		return nil, DecodingInvalid
	}
	if len(decoded) > MaxDecodedBytes {
		return nil, DecodingDecodedLimitExceeded
	}
	return decoded, DecodingDecoded
}

func decodeGzipBounded(payload []byte) ([]byte, DecodingStatus) {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, DecodingInvalid
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, MaxDecodedBytes+1))
	if err != nil {
		return nil, DecodingInvalid
	}
	if len(decoded) > MaxDecodedBytes {
		return nil, DecodingDecodedLimitExceeded
	}
	return decoded, DecodingDecoded
}

func buildShape(key, payload []byte, requestModelEligible bool) (*PayloadShape, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false
	}
	shape := &PayloadShape{TopLevelTypes: map[string]string{}, ArrayLengths: map[string]int{}, ObjectFieldCounts: map[string]int{}}
	if object, ok := value.(map[string]any); ok {
		if requestModelEligible {
			if model, ok := safeString(object["model"], modelPattern); ok {
				shape.RequestModel = model
			}
		}
		keys := make([]string, 0, len(object))
		for field := range object {
			keys = append(keys, field)
		}
		sort.Strings(keys)
		for _, field := range keys {
			if len(shape.TopLevelFields)+len(shape.UnknownTopLevelFieldHMACs) >= MaxObjectFields {
				shape.ShapeTruncated = true
				break
			}
			fieldKey := field
			if !knownTopLevelField(field) {
				hash := hmacHex(key, []byte("field:"+field))
				shape.UnknownTopLevelFieldHMACs = append(shape.UnknownTopLevelFieldHMACs, hash)
				fieldKey = hash
			} else {
				shape.TopLevelFields = append(shape.TopLevelFields, field)
			}
			shape.TopLevelTypes[fieldKey] = jsonType(object[field])
			collectStructure(key, shape, []string{field}, object[field], 1)
		}
		if model, ok := safeString(object["model"], modelPattern); ok {
			shape.ModelValue = model
		}
		if value, ok := object["stream"].(bool); ok {
			shape.StreamValue = &value
		}
		shape.ToolCount, shape.ToolTypes = collectTools(object["tools"])
		if shape.ToolCount > MaxArrayLength {
			shape.ToolCount = MaxArrayLength
			shape.ShapeTruncated = true
		}
		shape.ReasoningEffort = safeEnumFromValue(object["reasoning"], "effort", reasoningEfforts)
		if shape.ReasoningEffort == "" {
			shape.ReasoningEffort = safeStringValue(object["reasoning_effort"], reasoningEfforts)
		}
		shape.ReasoningSummary = safeStringValue(object["reasoning_summary"], reasoningSummaries)
		shape.EventType = safeStringValue(object["event"], protocolEvents)
		shape.ObjectType = safeStringValue(object["object"], protocolObjects)
		shape.Status = safeStringValue(object["status"], protocolStatuses)
		collectInputSummary(key, shape, object)
		shape.HasPreviousResponseID = hasPreviousResponseID(object)
	} else {
		shape.ShapeTruncated = true
	}
	collectIdentifiersAndOpaque(key, shape, value, nil, 0)
	if len(shape.TopLevelTypes) == 0 {
		shape.TopLevelTypes = nil
	}
	if len(shape.ArrayLengths) == 0 {
		shape.ArrayLengths = nil
	}
	if len(shape.ObjectFieldCounts) == 0 {
		shape.ObjectFieldCounts = nil
	}
	return shape, true
}

func collectStructure(key []byte, shape *PayloadShape, path []string, value any, depth int) {
	if depth > MaxJSONDepth {
		shape.ShapeTruncated = true
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) > MaxObjectFields {
			shape.ShapeTruncated = true
		}
		shape.ObjectFieldCounts[safeShapePath(key, path)] = boundedObjectCount(len(typed), shape)
		for field, nested := range typed {
			collectStructure(key, shape, append(path, field), nested, depth+1)
		}
	case []any:
		shape.ArrayLengths[safeShapePath(key, path)] = boundedArrayCount(len(typed), shape)
		for index, nested := range typed {
			if index >= MaxObjectFields {
				shape.ShapeTruncated = true
				break
			}
			collectStructure(key, shape, append(path, fmt.Sprintf("[%d]", index)), nested, depth+1)
		}
	}
}

func collectIdentifiersAndOpaque(key []byte, shape *PayloadShape, value any, path []string, depth int) {
	if depth > MaxJSONDepth {
		shape.ShapeTruncated = true
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for field, nested := range typed {
			lower := strings.ToLower(field)
			if isOpaqueField(lower) {
				if len(shape.opaque) < MaxOpaqueSummaries {
					shape.opaque = append(shape.opaque, OpaqueFieldSummary{
						FieldPath:         safeOpaquePath(key, append(path, field)),
						ValueType:         jsonType(nested),
						Size:              jsonValueSize(nested),
						OpaqueContentHMAC: hmacHex(key, []byte("opaque:"+jsonValue(nested))),
					})
				}
				continue
			}
			if identifier, ok := nested.(string); ok {
				if bucket, matched := identifierBucket(lower, path); matched {
					shape.addIdentifier(bucket, hmacHex(key, []byte("id:"+identifier)))
				}
			}
			collectIdentifiersAndOpaque(key, shape, nested, append(path, field), depth+1)
		}
	case []any:
		for index, nested := range typed {
			if index >= MaxObjectFields {
				shape.ShapeTruncated = true
				break
			}
			collectIdentifiersAndOpaque(key, shape, nested, append(path, fmt.Sprintf("[%d]", index)), depth+1)
		}
	}
}

func (s *PayloadShape) addIdentifier(bucket, value string) {
	switch bucket {
	case "response":
		s.identifiers.ResponseIDHMACs = appendBounded(s.identifiers.ResponseIDHMACs, value)
	case "previous_response":
		s.identifiers.PreviousResponseIDHMACs = appendBounded(s.identifiers.PreviousResponseIDHMACs, value)
	case "conversation":
		s.identifiers.ConversationIDHMACs = appendBounded(s.identifiers.ConversationIDHMACs, value)
	case "item":
		s.identifiers.ItemIDHMACs = appendBounded(s.identifiers.ItemIDHMACs, value)
	case "call":
		s.identifiers.CallIDHMACs = appendBounded(s.identifiers.CallIDHMACs, value)
	default:
		s.identifiers.OtherIDHMACs = appendBounded(s.identifiers.OtherIDHMACs, value)
	}
}

func identifierBucket(field string, path []string) (string, bool) {
	if field == "previous_response_id" {
		return "previous_response", true
	}
	if field == "call_id" {
		return "call", true
	}
	if field == "id" && len(path) > 0 {
		switch path[len(path)-1] {
		case "response":
			return "response", true
		case "item":
			return "item", true
		default:
			return "other", true
		}
	}
	if isIdentifierField(field) {
		switch {
		case strings.Contains(field, "response"):
			return "response", true
		case strings.Contains(field, "conversation"):
			return "conversation", true
		case strings.Contains(field, "item"):
			return "item", true
		case strings.Contains(field, "call"):
			return "call", true
		default:
			return "other", true
		}
	}
	return "", false
}

func collectInputSummary(key []byte, shape *PayloadShape, value any) {
	input, ok := value.(map[string]any)["input"].([]any)
	if !ok {
		return
	}
	shape.InputItemCount = len(input)
	shape.InputItemTypeCounts = map[string]int{}
	shape.InputRoleCounts = map[string]int{}
	shape.InputItemFingerprints = make([]InputItemFingerprint, 0, minInt(len(input), MaxObjectFields))
	for index, item := range input {
		if index >= MaxObjectFields {
			shape.ShapeTruncated = true
			break
		}
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fingerprint := InputItemFingerprint{Index: index, Fields: safeObjectFields(object)}
		if kind, ok := object["type"].(string); ok && safeInputClass(kind) != "" {
			fingerprint.Type = kind
			shape.InputItemTypeCounts[kind]++
		}
		if role, ok := object["role"].(string); ok && safeInputClass(role) != "" {
			fingerprint.Role = role
			shape.InputRoleCounts[role]++
		}
		if content, ok := object["content"].([]any); ok {
			fingerprint.ContentCount = boundedArrayCount(len(content), shape)
		}
		fingerprint.ObjectCount, fingerprint.ArrayCount = safeNestedCounts(object)
		itemShape := &PayloadShape{}
		collectIdentifiersAndOpaque(key, itemShape, object, []string{"input", fmt.Sprintf("[%d]", index)}, 0)
		fingerprint.Identifiers = cloneIdentifiers(itemShape.identifiers)
		shape.InputItemFingerprints = append(shape.InputItemFingerprints, fingerprint)
	}
	if len(shape.InputItemTypeCounts) == 0 {
		shape.InputItemTypeCounts = nil
	}
	if len(shape.InputRoleCounts) == 0 {
		shape.InputRoleCounts = nil
	}
}

func safeObjectFields(object map[string]any) []string {
	fields := make([]string, 0, len(object))
	for field := range object {
		if tokenPattern.MatchString(field) && !containsSensitiveMarker(field) {
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	if len(fields) > MaxObjectFields {
		fields = fields[:MaxObjectFields]
	}
	return fields
}

func safeNestedCounts(value any) (int, int) {
	objects, arrays := 0, 0
	var walk func(any, int)
	walk = func(current any, depth int) {
		if depth > MaxJSONDepth {
			return
		}
		switch typed := current.(type) {
		case map[string]any:
			objects++
			for _, nested := range typed {
				walk(nested, depth+1)
			}
		case []any:
			arrays++
			for index, nested := range typed {
				if index >= MaxObjectFields {
					break
				}
				walk(nested, depth+1)
			}
		}
	}
	walk(value, 0)
	return objects, arrays
}

func cloneInputFingerprints(values []InputItemFingerprint) []InputItemFingerprint {
	if len(values) == 0 {
		return nil
	}
	out := make([]InputItemFingerprint, len(values))
	for index, value := range values {
		out[index] = value
		out[index].Fields = append([]string(nil), value.Fields...)
		out[index].Identifiers = cloneIdentifiers(value.Identifiers)
	}
	return out
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func safeInputClass(value string) string {
	if containsSensitiveMarker(value) || len(value) > 64 || !tokenPattern.MatchString(value) {
		return ""
	}
	return value
}

func hasPreviousResponseID(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, ok = object["previous_response_id"].(string)
	return ok
}

func appendBounded(values []string, value string) []string {
	if len(values) < MaxIdentifierHashes {
		return append(values, value)
	}
	return values
}

func collectTools(value any) (int, []string) {
	items, ok := value.([]any)
	if !ok {
		return 0, nil
	}
	types := make(map[string]struct{})
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			if toolType := safeStringValue(object["type"], toolTypes); toolType != "" {
				types[toolType] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(types))
	for toolType := range types {
		out = append(out, toolType)
	}
	sort.Strings(out)
	return len(items), out
}

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9./:-]{1,128}$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var userAgentPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,32}/[A-Za-z0-9._-]{1,32}$`)
var headerNamePattern = regexp.MustCompile(`^[a-z0-9!#$%&'*+.^_` + "`" + `|~-]{1,128}$`)

const MaxArrayLength = 100000

var knownContentTypes = map[string]bool{
	"application/json":         true,
	"application/problem+json": true,
	"application/x-ndjson":     true,
	"application/octet-stream": true,
	"text/event-stream":        true,
	"text/plain":               true,
}

func boundedObjectCount(value int, shape *PayloadShape) int {
	if value > MaxObjectFields {
		shape.ShapeTruncated = true
		return MaxObjectFields
	}
	return value
}

func boundedArrayCount(value int, shape *PayloadShape) int {
	if value > MaxArrayLength {
		shape.ShapeTruncated = true
		return MaxArrayLength
	}
	return value
}

var knownFields = map[string]struct{}{
	"model": {}, "input": {}, "tools": {}, "reasoning": {}, "stream": {}, "include": {}, "metadata": {},
	"previous_response_id": {}, "type": {}, "object": {}, "status": {}, "event": {}, "response": {}, "item": {},
	"output": {}, "instructions": {}, "store": {}, "parallel_tool_calls": {}, "temperature": {}, "max_output_tokens": {},
}

var reasoningEfforts = map[string]struct{}{"minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {}}
var reasoningSummaries = map[string]struct{}{"auto": {}, "concise": {}, "detailed": {}, "none": {}}
var protocolEvents = map[string]struct{}{"response.created": {}, "response.completed": {}, "response.failed": {}, "response.in_progress": {}, "response.output_text.delta": {}, "response.output_item.added": {}, "response.output_item.done": {}, "response.function_call_arguments.delta": {}, "response.function_call_arguments.done": {}}
var protocolObjects = map[string]struct{}{"response": {}, "response.output_item": {}, "response.function_call": {}, "response.reasoning": {}, "error": {}}
var protocolStatuses = map[string]struct{}{"in_progress": {}, "completed": {}, "failed": {}, "incomplete": {}, "queued": {}}
var toolTypes = map[string]struct{}{"function": {}, "computer_use_preview": {}, "web_search": {}, "file_search": {}, "code_interpreter": {}, "custom": {}}

func knownTopLevelField(field string) bool {
	_, ok := knownFields[field]
	return ok
}

func safeString(value any, pattern *regexp.Regexp) (string, bool) {
	stringValue, ok := value.(string)
	return stringValue, ok && pattern.MatchString(stringValue) && !containsSensitiveMarker(stringValue)
}

func safeStringValue(value any, allowlist map[string]struct{}) string {
	stringValue, ok := value.(string)
	if !ok || containsSensitiveMarker(stringValue) {
		return ""
	}
	if _, ok := allowlist[stringValue]; !ok {
		return ""
	}
	return stringValue
}

func safeEnumFromValue(value any, field string, allowlist map[string]struct{}) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return safeStringValue(object[field], allowlist)
}

func isIdentifierField(field string) bool {
	return strings.HasSuffix(field, "_id") || strings.HasSuffix(field, "_ids") || strings.Contains(field, "response_id") || strings.Contains(field, "conversation_id") || strings.Contains(field, "item_id") || strings.Contains(field, "call_id")
}

func isOpaqueField(field string) bool {
	return field == "encrypted_content" || strings.Contains(field, "encrypted") || strings.Contains(field, "ciphertext") || field == "signature" || strings.Contains(field, "access_token") || field == "authorization" || field == "cookie" || strings.HasSuffix(field, "_token")
}

func jsonType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func jsonValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return jsonType(value)
	}
	return string(encoded)
}

func jsonValueSize(value any) int { return len(jsonValue(value)) }

func hmacHex(key, value []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil))
}

func normalizeMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err == nil {
		mediaType = strings.ToLower(mediaType)
		if knownContentTypes[mediaType] {
			return mediaType
		}
		return ""
	}
	value = strings.TrimSpace(strings.Split(value, ";")[0])
	if strings.ContainsAny(value, "\r\n") {
		return ""
	}
	value = strings.ToLower(value)
	if knownContentTypes[value] {
		return value
	}
	return ""
}

func summarizeHeaders(headers http.Header) HeaderSummary {
	if headers == nil {
		return HeaderSummary{}
	}
	seen := make(map[string]struct{}, len(headers))
	for name := range headers {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" && headerNamePattern.MatchString(name) && !containsSensitiveMarker(name) {
			seen[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	summary := HeaderSummary{
		PresentNames:         names,
		ContentType:          normalizeMediaType(headers.Get("Content-Type")),
		ContentEncoding:      safeContentEncoding(headers.Get("Content-Encoding")),
		AuthorizationPresent: headers.Get("Authorization") != "",
		CookiePresent:        headers.Get("Cookie") != "",
		SetCookiePresent:     headers.Get("Set-Cookie") != "",
	}
	if strings.EqualFold(headers.Get("Upgrade"), "websocket") {
		summary.Upgrade = "websocket"
	}
	if strings.Contains(strings.ToLower(headers.Get("Connection")), "upgrade") {
		summary.ConnectionClass = "upgrade"
	} else if headers.Get("Connection") != "" {
		summary.ConnectionClass = "present"
	}
	summary.UserAgentProduct = classifyUserAgent(headers.Get("User-Agent"))
	return summary
}

func classifyUserAgent(value string) string {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) == 0 || !userAgentPattern.MatchString(parts[0]) || containsSensitiveMarker(parts[0]) {
		return ""
	}
	return parts[0]
}

func inferPayloadKind(contentType string, payload []byte) PayloadKind {
	if len(payload) == 0 {
		return PayloadEmpty
	}
	if strings.Contains(contentType, "json") {
		var value any
		if json.Unmarshal(payload, &value) == nil {
			return PayloadJSON
		}
	}
	if strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "event-stream") || isUTF8(payload) {
		return PayloadText
	}
	return PayloadBinary
}

func isUTF8(value []byte) bool {
	return strings.ToValidUTF8(string(value), "") == string(value)
}

func safeToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 32 || !tokenPattern.MatchString(value) || containsSensitiveMarker(value) {
		return ""
	}
	return value
}

func safeContentEncoding(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "identity", "gzip", "br", "deflate", "zstd":
		return value
	default:
		return ""
	}
}

func safeHost(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 255 || strings.ContainsAny(value, "\r\n/") || containsSensitiveMarker(value) {
		return ""
	}
	return strings.ToLower(value)
}

func safePath(value string, key []byte) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > MaxPathBytes || strings.ContainsAny(value, "\r\n\x00") || containsSensitiveMarker(value) {
		return hmacHex(key, []byte("path:"+value))
	}
	return value
}

func safeQueryNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 128 || !tokenPattern.MatchString(value) || containsSensitiveMarker(value) {
			continue
		}
		seen[value] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func safeMessageType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "text", "binary", "ping", "pong", "close":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func safeEventType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, ok := protocolEvents[value]; ok {
		return value
	}
	return "unknown"
}

func safeShapePath(key []byte, path []string) string {
	parts := make([]string, 0, len(path))
	for _, part := range path {
		if tokenPattern.MatchString(part) && !containsSensitiveMarker(part) {
			parts = append(parts, part)
		} else {
			parts = append(parts, hmacHex(key, []byte("path-part:"+part)))
		}
	}
	return strings.Join(parts, ".")
}

func safeOpaquePath(key []byte, path []string) string {
	if len(path) > 0 {
		last := strings.ToLower(path[len(path)-1])
		if last == "encrypted_content" || last == "signature" || last == "authorization" || last == "cookie" {
			return last
		}
	}
	return hmacHex(key, []byte("opaque-path:"+strings.Join(path, ".")))
}

func containsSensitiveMarker(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"sentinel", "secret", "password", "credential", "authorization", "cookie", "prompt", "source", "token"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
