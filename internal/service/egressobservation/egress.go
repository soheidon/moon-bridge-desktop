// Package egressobservation provides a context-scoped, secret-free HTTP
// transport seam for provider egress observations.
package egressobservation

import (
	"context"
	"net/http"
	"sync"
)

type EventKind string

const (
	RequestDispatched    EventKind = "provider_request_dispatched"
	ResponseReceived     EventKind = "provider_response_received"
	ResponseModelObserved EventKind = "provider_response_model"
	ResponseForwarded    EventKind = "provider_response_forwarded"
)

type Event struct {
	Kind             EventKind `json:"kind"`
	Correlation      string    `json:"-"`
	ProfileID        string    `json:"-"`
	RequestedModel   string    `json:"requestedModel,omitempty"`
	RoutingSlot      string    `json:"routingSlot,omitempty"`
	Mode             string    `json:"mode,omitempty"`
	Provider         string    `json:"provider,omitempty"`
	UpstreamModel    string    `json:"upstreamModel,omitempty"`
	ConfiguredEffort string    `json:"configuredEffort,omitempty"`
	Protocol         string    `json:"protocol,omitempty"`
	Model            string    `json:"model,omitempty"`
	ResponseModel    string    `json:"responseModel,omitempty"`
	Thinking         string    `json:"thinking,omitempty"`
	EffectiveEffort  string    `json:"effectiveEffort,omitempty"`
	CredentialState  string    `json:"credentialState,omitempty"`
	ExchangeIndex    uint64    `json:"exchangeIndex,omitempty"`
	StatusCode       int       `json:"statusCode,omitempty"`
	Streaming        bool      `json:"streaming,omitempty"`
}

type Recorder func(Event)

type metadataKey struct{}

type metadataState struct {
	mu          sync.Mutex
	meta        Event
	recorder    Recorder
	next        uint64
	lastSuccess uint64
	lastStatus  int
	forwarded   bool
}

// WithMetadata attaches safe, in-process routing metadata to a request
// context. It is never serialized and is only consumed by the wrapped client.
func WithMetadata(ctx context.Context, meta Event, recorder Recorder) context.Context {
	return context.WithValue(ctx, metadataKey{}, &metadataState{meta: meta, recorder: recorder})
}

func stateFrom(ctx context.Context) *metadataState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(metadataKey{}).(*metadataState)
	return state
}

// MarkForwarded records forwarding only after the downstream response has
// been successfully written by the gateway.
func MarkForwarded(ctx context.Context) {
	state := stateFrom(ctx)
	if state == nil || state.recorder == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.forwarded || state.lastSuccess == 0 || state.lastStatus < 200 || state.lastStatus >= 300 {
		return
	}
	state.forwarded = true
	event := state.meta
	event.Kind = ResponseForwarded
	event.ExchangeIndex = state.lastSuccess
	event.StatusCode = state.lastStatus
	state.recorder(event)
}

// RecordResponseModel records the model reported in the provider's response
// body. It fires after the response body is parsed and before the gateway
// forwards it downstream, so response-side model identity is observable
// without logging any raw body, header, or identifier.
func RecordResponseModel(ctx context.Context, model string) {
	state := stateFrom(ctx)
	if state == nil || state.recorder == nil || model == "" {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	event := state.meta
	event.Kind = ResponseModelObserved
	event.ResponseModel = model
	event.ExchangeIndex = state.lastSuccess
	event.StatusCode = state.lastStatus
	state.recorder(event)
}

type roundTripper struct {
	base http.RoundTripper
}

func (t roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	state := stateFrom(req.Context())
	if state == nil || state.recorder == nil {
		return base.RoundTrip(req)
	}
	state.mu.Lock()
	state.next++
	index := state.next
	event := state.meta
	event.Kind = RequestDispatched
	event.ExchangeIndex = index
	state.mu.Unlock()
	state.recorder(event)

	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	state.mu.Lock()
	state.lastSuccess = index
	state.lastStatus = resp.StatusCode
	state.forwarded = false
	event = state.meta
	event.Kind = ResponseReceived
	event.ExchangeIndex = index
	event.StatusCode = resp.StatusCode
	state.mu.Unlock()
	state.recorder(event)
	return resp, nil
}

// WrapClient preserves the caller's client settings while adding the
// context-driven transport seam. Calling it on a client more than once is
// harmless for correctness; only contexts carrying metadata emit events.
func WrapClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	if _, alreadyWrapped := client.Transport.(roundTripper); alreadyWrapped {
		return client
	}
	clone.Transport = roundTripper{base: client.Transport}
	return &clone
}
