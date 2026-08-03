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
	Direction             Direction
	Transport             Transport
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
	rawPayloadSize        int
	rawPayloadHMAC        string
	hasRawPayloadOverride bool
}

type Observation struct {
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
	TopLevelFields            []string          `json:"topLevelFields,omitempty"`
	UnknownTopLevelFieldHMACs []string          `json:"unknownTopLevelFieldHmacs,omitempty"`
	TopLevelTypes             map[string]string `json:"topLevelTypes,omitempty"`
	ArrayLengths              map[string]int    `json:"arrayLengths,omitempty"`
	ObjectFieldCounts         map[string]int    `json:"objectFieldCounts,omitempty"`
	ModelValue                string            `json:"modelValue,omitempty"`
	StreamValue               *bool             `json:"streamValue,omitempty"`
	ReasoningEffort           string            `json:"reasoningEffort,omitempty"`
	ReasoningSummary          string            `json:"reasoningSummary,omitempty"`
	ToolCount                 int               `json:"toolCount,omitempty"`
	ToolTypes                 []string          `json:"toolTypes,omitempty"`
	EventType                 string            `json:"eventType,omitempty"`
	ObjectType                string            `json:"objectType,omitempty"`
	Status                    string            `json:"status,omitempty"`
	ShapeTruncated            bool              `json:"shapeTruncated,omitempty"`

	identifiers IdentifierSummary
	opaque      []OpaqueFieldSummary
}

func (s *PayloadShape) Identifiers() *IdentifierSummary { return &s.identifiers }
func (s *PayloadShape) OpaqueFields() []OpaqueFieldSummary {
	return s.opaque
}

type IdentifierSummary struct {
	ResponseIDHMACs     []string `json:"responseIdHmacs,omitempty"`
	ItemIDHMACs         []string `json:"itemIdHmacs,omitempty"`
	CallIDHMACs         []string `json:"callIdHmacs,omitempty"`
	ConversationIDHMACs []string `json:"conversationIdHmacs,omitempty"`
	OtherIDHMACs        []string `json:"otherIdHmacs,omitempty"`
}

type OpaqueFieldSummary struct {
	FieldPath         string `json:"fieldPath"`
	ValueType         string `json:"valueType"`
	Size              int    `json:"size"`
	OpaqueContentHMAC string `json:"opaqueContentHmac"`
}

type Analyzer struct {
	key     []byte
	session string
	buffer  *RingBuffer
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
	return &Analyzer{key: key, session: uuid.NewString(), buffer: NewRingBuffer(capacity)}, nil
}

// String and GoString prevent accidental formatting of the private HMAC key.
func (a *Analyzer) String() string   { return "traffic analysis session" }
func (a *Analyzer) GoString() string { return "traffic analysis session" }

func (a *Analyzer) SessionID() string { return a.session }

func (a *Analyzer) Record(input PayloadInput) Observation {
	obs := analyzePayload(a.key, input)
	obs.SessionID = a.session
	obs.Timestamp = time.Now().UTC()
	obs.ConnectionID = a.hmacString(uuid.NewString())
	obs.RequestID = a.hmacString(uuid.NewString())
	return a.buffer.Append(obs)
}

func (a *Analyzer) Snapshot(after uint64) ([]Observation, uint64) {
	return a.buffer.Snapshot(after)
}

func (a *Analyzer) DroppedCapacity() uint64 { return a.buffer.Dropped() }
func (a *Analyzer) Clear()                  { a.buffer.Clear() }

func (a *Analyzer) hmacString(value string) string {
	return hmacHex(a.key, []byte(value))
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
		shape.TopLevelFields = append([]string(nil), in.PayloadShape.TopLevelFields...)
		shape.UnknownTopLevelFieldHMACs = append([]string(nil), in.PayloadShape.UnknownTopLevelFieldHMACs...)
		shape.ToolTypes = append([]string(nil), in.PayloadShape.ToolTypes...)
		shape.TopLevelTypes = cloneStringMap(in.PayloadShape.TopLevelTypes)
		shape.ArrayLengths = cloneIntMap(in.PayloadShape.ArrayLengths)
		shape.ObjectFieldCounts = cloneIntMap(in.PayloadShape.ObjectFieldCounts)
		shape.identifiers = cloneIdentifiers(in.PayloadShape.identifiers)
		shape.opaque = append([]OpaqueFieldSummary(nil), in.PayloadShape.opaque...)
		if in.PayloadShape.StreamValue != nil {
			value := *in.PayloadShape.StreamValue
			shape.StreamValue = &value
		}
		out.PayloadShape = &shape
	}
	return out
}

func cloneIdentifiers(in IdentifierSummary) IdentifierSummary {
	return IdentifierSummary{
		ResponseIDHMACs:     append([]string(nil), in.ResponseIDHMACs...),
		ItemIDHMACs:         append([]string(nil), in.ItemIDHMACs...),
		CallIDHMACs:         append([]string(nil), in.CallIDHMACs...),
		ConversationIDHMACs: append([]string(nil), in.ConversationIDHMACs...),
		OtherIDHMACs:        append([]string(nil), in.OtherIDHMACs...),
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
		SSEEventType:         safeEventType(input.SSEEventType, key),
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
	} else if contentEncoding != "" && contentEncoding != "identity" {
		obs.DecodingStatus = DecodingUnsupported
		analysisBytes = nil
	}
	obs.DecodedObservationSize = len(analysisBytes)
	if shape, ok := buildShape(key, analysisBytes); ok {
		obs.PayloadShape = shape
		obs.Identifiers = cloneIdentifiers(shape.identifiers)
		obs.OpaqueFields = append([]OpaqueFieldSummary(nil), shape.opaque...)
		if shape.ShapeTruncated {
			obs.Truncated = true
			obs.Disposition = DispositionShapeTruncated
		}
	}
	return obs
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

func buildShape(key, payload []byte) (*PayloadShape, bool) {
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
			if isIdentifierField(lower) {
				if identifier, ok := nested.(string); ok {
					shape.addIdentifier(lower, hmacHex(key, []byte("id:"+identifier)))
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

func (s *PayloadShape) addIdentifier(field, value string) {
	switch {
	case strings.Contains(field, "response"):
		s.identifiers.ResponseIDHMACs = appendBounded(s.identifiers.ResponseIDHMACs, value)
	case strings.Contains(field, "conversation"):
		s.identifiers.ConversationIDHMACs = appendBounded(s.identifiers.ConversationIDHMACs, value)
	case strings.Contains(field, "item"):
		s.identifiers.ItemIDHMACs = appendBounded(s.identifiers.ItemIDHMACs, value)
	case strings.Contains(field, "call") || strings.Contains(field, "tool"):
		s.identifiers.CallIDHMACs = appendBounded(s.identifiers.CallIDHMACs, value)
	default:
		s.identifiers.OtherIDHMACs = appendBounded(s.identifiers.OtherIDHMACs, value)
	}
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
	case "identity", "gzip", "br", "deflate":
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

func safeEventType(value string, key []byte) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, ok := protocolEvents[value]; ok {
		return value
	}
	return hmacHex(key, []byte("event:"+value))
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
