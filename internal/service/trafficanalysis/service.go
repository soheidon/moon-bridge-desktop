package trafficanalysis

import (
	"context"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// ManagementMode is the external control domain for Capture. It is orthogonal
// to the CaptureProxy's internal state (stopped/ready/capturing/draining/
// passthrough/failed): mode describes who is allowed to mutate the capture,
// while CaptureState describes the wire-level relay condition.
type ManagementMode string

const (
	ModeIdle        ManagementMode = "idle"
	ModeCaptureOnly ManagementMode = "capture_only"
	ModeDesktop     ManagementMode = "desktop_managed"
	ModeRecovery    ManagementMode = "recovery_required"
)

// ErrorKind is a coarse, safe category of a Service operation failure. Raw
// lower-level strings are never exposed.
type ErrorKind string

const (
	KindCaptureAlreadyActive         ErrorKind = "traffic_capture_already_active"
	KindCaptureNotActive             ErrorKind = "traffic_capture_not_active"
	KindCaptureStartFailed           ErrorKind = "traffic_capture_start_failed"
	KindCaptureStopFailed            ErrorKind = "traffic_capture_stop_failed"
	KindCaptureClosing               ErrorKind = "traffic_capture_closing"
	KindCaptureStartSuperseded       ErrorKind = "traffic_capture_start_superseded"
	KindGatewayMismatch              ErrorKind = "traffic_gateway_mismatch"
	KindGatewayNotBound              ErrorKind = "traffic_gateway_not_bound"
	KindCaptureGenerationMismatch    ErrorKind = "traffic_capture_generation_mismatch"
	KindCaptureOperationSuperseded   ErrorKind = "traffic_capture_operation_superseded"
	KindModelMappingInvalid          ErrorKind = "traffic_model_mapping_invalid"
	KindIntegrationManagedByDesktop  ErrorKind = "traffic_integration_managed_by_desktop"
	KindRecoveryConfirmationRequired ErrorKind = "recovery_confirmation_required"
)

type OperationState string

const (
	OperationNone     OperationState = ""
	OperationStarting OperationState = "starting"
	OperationClosing  OperationState = "closing"
)

// Error is a safe Service operation error. It carries only the kind and a
// sanitized message; secrets, paths, and raw errors never appear.
type Error struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func newError(kind ErrorKind, message string) error {
	return &Error{Kind: kind, Message: message}
}

// State is a thread-safe external snapshot of the Service. It never carries
// secrets (opts.GatewayToken), absolute paths, hashes, or raw relay bodies.
type State struct {
	Generation           uint64         `json:"generation"`
	Mode                 ManagementMode `json:"mode"`
	CaptureState         string         `json:"captureState"`
	GatewayInstanceID    string         `json:"gatewayInstanceId,omitempty"`
	GatewayAddress       string         `json:"gatewayAddress,omitempty"`
	ListeningAddress     string         `json:"listeningAddress,omitempty"`
	ObservationCount     uint64         `json:"observationCount"`
	ObservationCapacity  uint64         `json:"observationCapacity"`
	HTTPRequests         uint64         `json:"httpRequests"`
	SSEStreams           uint64         `json:"sseStreams"`
	WebSocketConnections uint64         `json:"websocketConnections"`
	DroppedObservations  uint64         `json:"droppedObservations"`
	LastError            string         `json:"lastError,omitempty"`
	Operation            OperationState `json:"operation,omitempty"`
}

// captureProxy is the narrow seam used by Service. Production uses the real
// CaptureProxy; tests inject a deterministic fake so ownership races and
// fault paths are exercised without relying on socket timing.
type captureProxy interface {
	Start() error
	Pause() error
	Resume() error
	Stop(context.Context) error
	Close() error
	Status() CaptureStatus
	Observations(uint64) ([]Observation, uint64)
	Clear()
	StateStopped() bool
	StateFailed() bool
}

// RecordGatewayEvent is the optional observation sink used by Gateway. It is
// deliberately not part of captureProxy so deterministic fakes need not know
// about gateway internals.
func (s *Service) RecordGatewayEvent(input GatewayEventInput) {
	s.mu.Lock()
	proxy := s.proxy
	s.mu.Unlock()
	if proxy == nil {
		return
	}
	if recorder, ok := proxy.(interface{ RecordGatewayEvent(GatewayEventInput) }); ok {
		recorder.RecordGatewayEvent(input)
	}
}

type proxyFactory func(CaptureConfig) captureProxy

type closeOperation struct {
	proxy      captureProxy
	generation uint64
	done       chan struct{}
	state      State
	err        error
}

type proxyOperationKind string

const (
	proxyOperationPause      proxyOperationKind = "pause"
	proxyOperationResume     proxyOperationKind = "resume"
	proxyOperationStop       proxyOperationKind = "stop"
	proxyOperationClear      proxyOperationKind = "clear"
	proxyOperationGatewayEnd proxyOperationKind = "gateway_end"
)

// proxyOperation is the ownership token for a blocking mutation against a
// committed proxy. The token is deliberately private: callers only need the
// public starting/closing states, while the service needs the exact proxy and
// generation to prevent a late result from committing against newer state.
type proxyOperation struct {
	id         uint64
	kind       proxyOperationKind
	proxy      captureProxy
	generation uint64
	ownerID    string
	done       chan struct{}
}

// modelMapping is process-local routing evidence. It is intentionally not
// part of State or any persisted recovery representation.
type modelMapping struct {
	sourceModel    string
	sourceBound    bool
	targetRoute    string
	generation     uint64
	gatewayID      string
	gatewayAddress string
	ownerID        string
}

// modelMappingDiag is a secret-free tri-state observation of the mapping
// guards. It exists only for diagnostic logging and never feeds back into
// resolution. Model names, target aliases, and config contents are never
// rendered; each field is "true", "false", or "n/a" (not evaluable).
type modelMappingDiag struct {
	mappingPresent    string // "true"/"false"
	sourceState       string // "unbound"/"bound"/"n/a"
	sourceModelMatch  string // "true"/"false"/"n/a"
	generationMatch   string // "true"/"false"/"n/a"
	gatewayMatch      string // "true"/"false"/"n/a"
	relayActive       string // "true"/"false"/"n/a"
	ownerMatch        string // "true"/"false"/"n/a"
	lazyBindAttempted string // "true"/"false"/"n/a"
	lazyBindSuccess   string // "true"/"false"/"n/a"
}

const (
	triNA      = "n/a"
	triTrue    = "true"
	triFalse   = "false"
	triUnbound = "unbound"
	triBound   = "bound"
)

func boolToTri(b bool) string {
	if b {
		return triTrue
	}
	return triFalse
}

// StartOptions describe one Capture start. Gateway identity (instance ID,
// address) is never carried in StartOptions — it is always set via
// BindGatewayRun before StartCapture is called. GatewayToken is used only in
// memory for proxy wiring and never appears in State, snapshots, errors, or
// logs.
type StartOptions struct {
	GatewayToken    string
	UpstreamBase    string
	ListenAddr      string
	HTTPClient      *http.Client
	WebSocketDialer *websocket.Dialer
}

// Service owns at most one CaptureProxy at a time and is the single path by
// which the Gateway management API, the desktop control surface, and (later)
// Desktop transactions reach that proxy. It is safe for concurrent use: every
// public method serializes on the internal mutex, and blocking proxy calls
// (listener bind, connection drains) are made outside the lock with the
// owned proxy pointer captured under it.
type Service struct {
	mu sync.Mutex

	proxy       captureProxy
	newProxy    proxyFactory
	generation  uint64
	mode        ManagementMode
	gatewayID   string
	gatewayAddr string
	listenAddr  string
	lastError   string
	// desktopOwnerID is private ownership evidence for the current desktop
	// transaction. It is never included in State, errors, or logs.
	desktopOwnerID string
	modelMapping   *modelMapping

	// startSeq is a monotonically increasing counter used to generate unique
	// reservation IDs for each StartCapture attempt. activeStartID records
	// the reservation ID of the currently in-progress start (0 = no active
	// reservation). A reservation is confirmed by setting activeStartID =
	// myStartID; it is cleared on commit or abandon. This prevents the ABA
	// problem where an old Start could steal a newer reservation's token.
	startSeq      uint64
	activeStartID uint64
	operationSeq  uint64
	activeOp      *proxyOperation
	closeOp       *closeOperation
}

// NewService returns an idle, proxy-less Service.
func NewService() *Service {
	return newService(nil)
}

func newService(factory proxyFactory) *Service {
	if factory == nil {
		factory = func(cfg CaptureConfig) captureProxy { return NewCaptureProxy(cfg) }
	}
	return &Service{mode: ModeIdle, newProxy: factory}
}

// Status returns a snapshot of the current Service state.
func (s *Service) Status() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) snapshotLocked() State {
	st := State{
		Generation:        s.generation,
		Mode:              s.mode,
		CaptureState:      "stopped",
		GatewayInstanceID: s.gatewayID,
		GatewayAddress:    s.gatewayAddr,
		ListeningAddress:  s.listenAddr,
		LastError:         s.lastError,
	}
	if s.closeOp != nil {
		st.Operation = OperationClosing
	} else if s.activeStartID != 0 {
		st.Operation = OperationStarting
	}
	if s.proxy != nil {
		ps := s.proxy.Status()
		st.CaptureState = normalizeCaptureState(ps.State)
		st.ListeningAddress = ps.CaptureAddress
		st.ObservationCount = ps.ObservationCount
		st.ObservationCapacity = ps.ObservationCapacity
		st.HTTPRequests = ps.HTTPRequests
		st.SSEStreams = ps.SSEStreams
		st.WebSocketConnections = ps.WebSocketConnections
		st.DroppedObservations = ps.DroppedObservations
	}
	return st
}

// normalizeCaptureState hides the transitional "ready" state from the external
// surface, normalizing it to the stopped value as the plan requires.
func normalizeCaptureState(proxyState string) string {
	if proxyState == "ready" {
		return "stopped"
	}
	return proxyState
}

// StartCapture starts a new capture. It is the only production path that
// creates a CaptureProxy.
//
// A start while a capture is already running is rejected with
// KindCaptureAlreadyActive. A start while a proxy is stopped (after
// StopCapture) auto-closes the stopped proxy and starts fresh.
//
// Gateway identity must already be bound via BindGatewayRun. If no identity
// is bound, the start is rejected with KindGatewayNotBound.
//
// Each start attempt receives a unique reservation ID (startSeq) to prevent
// the ABA problem where an old Start could steal a newer reservation's token.
// The gateway identity is also captured at reservation time and verified at
// commit to detect gateway rebinds that occurred during start.
func (s *Service) StartCapture(opts StartOptions) (State, error) {
	for {
		s.mu.Lock()
		if s.closeOp != nil {
			s.mu.Unlock()
			return State{}, newError(KindCaptureClosing, "capture close is in progress")
		}
		if s.activeOp != nil {
			op := s.activeOp
			s.mu.Unlock()
			<-op.done
			continue
		}
		switch s.mode {
		case ModeRecovery:
			s.mu.Unlock()
			return State{}, newError(KindRecoveryConfirmationRequired, "capture requires recovery confirmation")
		case ModeDesktop:
			s.mu.Unlock()
			return State{}, newError(KindIntegrationManagedByDesktop, "capture is managed by the desktop")
		}
		// Identity must be bound before capture can start.
		if s.gatewayID == "" {
			s.mu.Unlock()
			return State{}, newError(KindGatewayNotBound, "gateway identity not bound; call BindGatewayRun first")
		}

		// Capture identity and reservation under the lock.
		s.startSeq++
		myStartID := s.startSeq
		myGatewayID := s.gatewayID
		myGatewayAddr := s.gatewayAddr
		myGeneration := s.generation

		if s.proxy != nil {
			if s.proxy.StateStopped() {
				// Proxy is stopped (after StopCapture): reserve the restart.
				old := s.proxy
				s.proxy = nil
				s.modelMapping = nil
				s.activeStartID = myStartID
				s.mu.Unlock()
				_ = old.Close()
			} else {
				s.mu.Unlock()
				return State{}, newError(KindCaptureAlreadyActive, "capture is already active")
			}
		} else if s.activeStartID != 0 {
			// Another StartCapture is already building a proxy.
			s.mu.Unlock()
			return State{}, newError(KindCaptureAlreadyActive, "capture is already active")
		} else {
			s.activeStartID = myStartID
			s.mu.Unlock()
		}

		cfg := CaptureConfig{
			ListenAddr:      opts.ListenAddr,
			UpstreamBase:    opts.UpstreamBase,
			InstanceID:      myGatewayID,
			HTTPClient:      opts.HTTPClient,
			WebSocketDialer: opts.WebSocketDialer,
		}
		// Build and bind outside the lock: Start binds a listener.
		proxy := s.newProxy(cfg)
		startErr := proxy.Start()

		s.mu.Lock()
		ownsReservation := s.activeStartID == myStartID
		identityChanged := s.gatewayID != myGatewayID || s.gatewayAddr != myGatewayAddr
		generationChanged := s.generation != myGeneration
		proxyChanged := s.proxy != nil
		if ownsReservation {
			s.activeStartID = 0
		}

		if !ownsReservation || generationChanged || proxyChanged {
			st := s.snapshotLocked()
			s.mu.Unlock()
			_ = proxy.Close()
			return st, newError(KindCaptureStartSuperseded, "capture start was superseded")
		}
		if identityChanged {
			st := s.snapshotLocked()
			s.mu.Unlock()
			_ = proxy.Close()
			return st, newError(KindGatewayMismatch, "gateway identity changed during capture start")
		}
		if startErr != nil {
			s.lastError = "capture start failed"
			st := s.snapshotLocked()
			s.mu.Unlock()
			_ = proxy.Close()
			return st, newError(KindCaptureStartFailed, "capture start failed")
		}
		s.proxy = proxy
		s.generation++
		s.modelMapping = nil
		s.mode = ModeCaptureOnly
		s.listenAddr = cfg.ListenAddr
		s.lastError = ""
		st := s.snapshotLocked()
		s.mu.Unlock()
		return st, nil
	}
}

// CloseCapture stops and closes the owned proxy, releasing the capture
// listener and clearing ownership back to idle. It is idempotent when nothing
// is active. Recovery mode refuses; desktop-managed mode is refused (the
// Desktop path, which arrives in 4D, is the only close for a managed capture).
// If a StartCapture is in progress (starting=true), CloseCapture cancels that
// reservation by bumping the generation so the late commit detects the stale
// generation and drops its proxy.
func (s *Service) CloseCapture(ctx context.Context) (State, error) {
	for {
		s.mu.Lock()
		if s.closeOp != nil {
			op := s.closeOp
			s.mu.Unlock()
			return waitCloseOperation(ctx, op)
		}
		if s.activeOp != nil {
			op := s.activeOp
			s.mu.Unlock()
			if err := waitProxyOperation(ctx, op); err != nil {
				return State{}, err
			}
			continue
		}
		switch s.mode {
		case ModeRecovery:
			s.mu.Unlock()
			return State{}, newError(KindRecoveryConfirmationRequired, "capture requires recovery confirmation")
		case ModeDesktop:
			s.mu.Unlock()
			return State{}, newError(KindIntegrationManagedByDesktop, "capture is managed by the desktop")
		}
		if s.proxy == nil {
			// A reservation-only close invalidates the in-flight Start without
			// waiting for its fake or real listener bind.
			if s.activeStartID != 0 {
				s.activeStartID = 0
				s.generation++
				s.modelMapping = nil
			}
			s.gatewayID = ""
			s.gatewayAddr = ""
			s.listenAddr = ""
			s.lastError = ""
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st, nil
		}
		op := &closeOperation{
			proxy:      s.proxy,
			generation: s.generation,
			done:       make(chan struct{}),
		}
		s.closeOp = op
		// Close invalidates routing evidence immediately. If proxy cleanup fails
		// and the service enters recovery, no request may use a closing relay's
		// stale model mapping.
		s.modelMapping = nil
		s.mu.Unlock()

		// CaptureProxy.Close has its own bounded cleanup timeout. It must not use
		// the first caller's context, otherwise a canceled waiter could abandon
		// the shared close operation for every other caller.
		closeErr := op.proxy.Close()

		s.mu.Lock()
		if closeErr != nil {
			s.mode = ModeRecovery
			s.lastError = "capture close failed"
			op.err = newError(KindCaptureStopFailed, "capture close failed")
		} else if s.proxy == op.proxy && s.generation == op.generation {
			s.proxy = nil
			s.generation++
			s.modelMapping = nil
			s.activeStartID = 0
			s.mode = ModeIdle
			s.gatewayID = ""
			s.gatewayAddr = ""
			s.listenAddr = ""
			s.lastError = ""
		} else {
			// This should be unreachable because mutations are rejected while the
			// operation is registered. Keep the committed proxy visible if it is
			// ever observed, rather than publishing a false idle state.
			s.mode = ModeRecovery
			s.lastError = "capture close state changed"
			op.err = newError(KindCaptureStopFailed, "capture close failed")
		}
		s.closeOp = nil
		op.state = s.snapshotLocked()
		close(op.done)
		result, err := op.state, op.err
		s.mu.Unlock()
		return result, err
	}
}

func waitCloseOperation(ctx context.Context, op *closeOperation) (State, error) {
	select {
	case <-op.done:
		return op.state, op.err
	case <-ctx.Done():
		return State{}, ctx.Err()
	}
}

func waitProxyOperation(ctx context.Context, op *proxyOperation) error {
	select {
	case <-op.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) reserveProxyOperationLocked(kind proxyOperationKind, proxy captureProxy) *proxyOperation {
	s.operationSeq++
	op := &proxyOperation{
		id:         s.operationSeq,
		kind:       kind,
		proxy:      proxy,
		generation: s.generation,
		ownerID:    s.desktopOwnerID,
		done:       make(chan struct{}),
	}
	s.activeOp = op
	return op
}

func (s *Service) ownsProxyOperationLocked(op *proxyOperation) bool {
	return s.activeOp == op && s.proxy == op.proxy && s.generation == op.generation && s.desktopOwnerID == op.ownerID
}

func (s *Service) finishProxyOperationLocked(op *proxyOperation) {
	if s.activeOp == op {
		s.activeOp = nil
	}
	close(op.done)
}

// PauseCapture pauses observation while keeping the capture relay alive.
func (s *Service) PauseCapture(ctx context.Context) (State, error) {
	return s.runProxyMutation(ctx, proxyOperationPause, func(proxy captureProxy) error {
		return proxy.Pause()
	})
}

// ResumeCapture resumes observation after a pause.
func (s *Service) ResumeCapture(ctx context.Context) (State, error) {
	return s.runProxyMutation(ctx, proxyOperationResume, func(proxy captureProxy) error {
		return proxy.Resume()
	})
}

// StopCapture stops the capture listener. The proxy reference is retained so
// Status can report its stopped state; mode and ownership are not reset by a
// stop alone. Repeated Stop when already stopped is safe.
func (s *Service) StopCapture(ctx context.Context) (State, error) {
	return s.runProxyMutation(ctx, proxyOperationStop, func(proxy captureProxy) error {
		return proxy.Stop(ctx)
	})
}

func (s *Service) runProxyMutation(
	ctx context.Context,
	kind proxyOperationKind,
	mutate func(captureProxy) error,
) (State, error) {
	for {
		s.mu.Lock()
		if s.closeOp != nil {
			s.mu.Unlock()
			return State{}, newError(KindCaptureClosing, "capture close is in progress")
		}
		switch s.mode {
		case ModeRecovery:
			s.mu.Unlock()
			return State{}, newError(KindRecoveryConfirmationRequired, "capture requires recovery confirmation")
		case ModeDesktop:
			s.mu.Unlock()
			return State{}, newError(KindIntegrationManagedByDesktop, "capture is managed by the desktop")
		}
		if s.activeOp != nil {
			op := s.activeOp
			s.mu.Unlock()
			if err := waitProxyOperation(ctx, op); err != nil {
				return State{}, err
			}
			continue
		}
		proxy := s.proxy
		if proxy == nil {
			s.mu.Unlock()
			return State{}, newError(KindCaptureNotActive, "no capture is active")
		}
		op := s.reserveProxyOperationLocked(kind, proxy)
		s.mu.Unlock()

		err := mutate(proxy)

		s.mu.Lock()
		ownsOperation := s.ownsProxyOperationLocked(op)
		if ownsOperation && err != nil && proxy.StateFailed() {
			s.proxy = nil
			s.lastError = "capture failed"
		}
		s.finishProxyOperationLocked(op)
		st := s.snapshotLocked()
		s.mu.Unlock()
		if err != nil {
			return st, newError(KindCaptureStopFailed, "capture operation failed")
		}
		return st, nil
	}
}

// Observations returns the observations recorded since the given sequence.
func (s *Service) Observations(after uint64) ([]Observation, uint64) {
	s.mu.Lock()
	proxy := s.proxy
	s.mu.Unlock()
	if proxy == nil {
		return nil, 0
	}
	return proxy.Observations(after)
}

// Clear discards all recorded observations. It is rejected in
// desktop_managed (would destroy 4D autolog/checkpoint/finalize targets) and
// recovery_required (must confirm recovery first).
func (s *Service) Clear() error {
	for {
		s.mu.Lock()
		if s.closeOp != nil {
			s.mu.Unlock()
			return newError(KindCaptureClosing, "capture close is in progress")
		}
		switch s.mode {
		case ModeDesktop:
			s.mu.Unlock()
			return newError(KindIntegrationManagedByDesktop, "service is desktop-managed; clear is not allowed")
		case ModeRecovery:
			s.mu.Unlock()
			return newError(KindRecoveryConfirmationRequired, "service is in recovery; clear is not allowed")
		}
		if s.activeOp != nil {
			op := s.activeOp
			s.mu.Unlock()
			<-op.done
			continue
		}
		proxy := s.proxy
		if proxy == nil {
			s.mu.Unlock()
			return nil
		}
		op := s.reserveProxyOperationLocked(proxyOperationClear, proxy)
		s.mu.Unlock()

		proxy.Clear()

		s.mu.Lock()
		s.finishProxyOperationLocked(op)
		s.mu.Unlock()
		return nil
	}
}

// ownership transitions (Boundary 4C): claim/release API for the Desktop
// transaction path (4D) without performing any config or recovery work.

// ClaimDesktop promotes a capture_only capture to desktop_managed when the
// claimed gateway identity matches the recorded one. It never re-runs
// Capture.Start. A mismatch is an explicit conflict and leaves mode/proxy
// untouched.
func (s *Service) ClaimDesktop(gatewayInstanceID, gatewayAddress string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.proxy == nil {
		return State{}, newError(KindCaptureNotActive, "no capture is active")
	}
	ps := s.proxy.Status()
	if !isCapturing(ps.State) {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not capturing")
	}
	if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
		return s.snapshotLocked(), newError(KindGatewayMismatch, "gateway identity does not match the capture")
	}
	s.mode = ModeDesktop
	s.desktopOwnerID = ""
	return s.snapshotLocked(), nil
}

// ClaimDesktopExpected promotes a capture_only capture to desktop_managed
// using a generation and Gateway identity compare-and-swap. It is additive to
// ClaimDesktop so the existing public management contract is unchanged.
func (s *Service) ClaimDesktopExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress, ownerID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if ownerID == "" {
		return s.snapshotLocked(), newError(KindCaptureOperationSuperseded, "desktop ownership is invalid")
	}
	if s.activeStartID != 0 || s.activeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureAlreadyActive, "capture operation is in progress")
	}
	if s.mode == ModeRecovery {
		return s.snapshotLocked(), newError(KindRecoveryConfirmationRequired, "capture requires recovery confirmation")
	}
	if s.mode != ModeCaptureOnly {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not capture-only")
	}
	if s.generation != expectedGeneration {
		return s.snapshotLocked(), newError(KindCaptureGenerationMismatch, "capture generation does not match")
	}
	if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
		return s.snapshotLocked(), newError(KindGatewayMismatch, "gateway identity does not match the capture")
	}
	if s.proxy == nil {
		return State{}, newError(KindCaptureNotActive, "no capture is active")
	}
	if !isCapturing(s.proxy.Status().State) {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not capturing")
	}
	s.mode = ModeDesktop
	s.desktopOwnerID = ownerID
	return s.snapshotLocked(), nil
}

// ValidateCaptureExpected performs the non-mutating half of the Desktop
// adoption CAS. It checks the same generation, identity, mode, close, start,
// and proxy-operation guards that ClaimDesktopExpected will check again. The
// returned State never exposes the private Desktop owner ID.
func (s *Service) ValidateCaptureExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.activeStartID != 0 || s.activeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureAlreadyActive, "capture operation is in progress")
	}
	if s.mode != ModeCaptureOnly {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not capture-only")
	}
	if s.generation != expectedGeneration {
		return s.snapshotLocked(), newError(KindCaptureGenerationMismatch, "capture generation does not match")
	}
	if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
		return s.snapshotLocked(), newError(KindGatewayMismatch, "capture Gateway identity does not match")
	}
	if s.proxy == nil || !isCapturing(s.proxy.Status().State) {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not capturing")
	}
	return s.snapshotLocked(), nil
}

// ValidateCaptureOnlyExpected verifies the non-desktop, passthrough side of
// the Disable final CAS without exposing the private owner ID.
func (s *Service) ValidateCaptureOnlyExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress, expectedListener string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.activeStartID != 0 || s.activeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureAlreadyActive, "capture operation is in progress")
	}
	if s.mode != ModeCaptureOnly || s.desktopOwnerID != "" || s.generation != expectedGeneration || s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
		return s.snapshotLocked(), newError(KindGatewayMismatch, "capture ownership evidence does not match")
	}
	if s.proxy == nil {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "no capture is active")
	}
	ps := s.proxy.Status()
	if ps.State != "passthrough" || ps.CaptureAddress != expectedListener {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not in passthrough")
	}
	return s.snapshotLocked(), nil
}

// ValidateIdleExpected verifies that CloseCapture completed the relay teardown
// and that no proxy, listener, or operation remains. CloseCapture advances the
// generation when it commits the idle state, so the expected value is the
// generation observed before Finish began.
func (s *Service) ValidateIdleExpected(expectedGeneration uint64) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.activeStartID != 0 || s.activeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureAlreadyActive, "capture operation is in progress")
	}
	if s.mode != ModeIdle || s.proxy != nil || s.listenAddr != "" || s.desktopOwnerID != "" || s.generation <= expectedGeneration {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not fully idle")
	}
	if s.gatewayID != "" || s.gatewayAddr != "" {
		return s.snapshotLocked(), newError(KindGatewayMismatch, "idle capture retains gateway identity")
	}
	return s.snapshotLocked(), nil
}

// ValidateDesktopOwnershipExpected is a private-owner evidence check for the
// transaction coordinator. It returns only a boolean and never exposes the
// owner ID through State, errors, logs, or bindings.
func (s *Service) ValidateDesktopOwnershipExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress, ownerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil || s.activeStartID != 0 || s.activeOp != nil || ownerID == "" {
		return false
	}
	if s.mode != ModeDesktop || s.desktopOwnerID != ownerID || s.generation != expectedGeneration {
		return false
	}
	if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress || s.proxy == nil {
		return false
	}
	return isCapturing(s.proxy.Status().State)
}

// ValidateDesktopIntegrationExpected atomically validates the complete live
// Traffic ownership evidence under the Service mutex. It is the final
// transaction success gate; callers must not combine separate Status and
// owner reads for this decision.
func (s *Service) ValidateDesktopIntegrationExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress, ownerID, expectedListener string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.activeStartID != 0 || s.activeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureAlreadyActive, "capture operation is in progress")
	}
	if s.mode != ModeDesktop || s.desktopOwnerID == "" || s.desktopOwnerID != ownerID {
		return s.snapshotLocked(), newError(KindCaptureOperationSuperseded, "desktop ownership was superseded")
	}
	if s.generation != expectedGeneration {
		return s.snapshotLocked(), newError(KindCaptureGenerationMismatch, "capture generation does not match")
	}
	if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
		return s.snapshotLocked(), newError(KindGatewayMismatch, "capture Gateway identity does not match")
	}
	if s.proxy == nil {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "no capture is active")
	}
	ps := s.proxy.Status()
	if !isCapturing(ps.State) || ps.CaptureAddress != expectedListener {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture listener or state does not match")
	}
	return s.snapshotLocked(), nil
}

// ValidateDesktopPassthroughExpected atomically validates the paused side of
// the Disable transaction while retaining the private Desktop owner.
func (s *Service) ValidateDesktopPassthroughExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress, ownerID, expectedListener string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.activeStartID != 0 || s.activeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureAlreadyActive, "capture operation is in progress")
	}
	if s.mode != ModeDesktop || s.desktopOwnerID == "" || s.desktopOwnerID != ownerID {
		return s.snapshotLocked(), newError(KindCaptureOperationSuperseded, "desktop ownership was superseded")
	}
	if s.generation != expectedGeneration {
		return s.snapshotLocked(), newError(KindCaptureGenerationMismatch, "capture generation does not match")
	}
	if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
		return s.snapshotLocked(), newError(KindGatewayMismatch, "capture Gateway identity does not match")
	}
	if s.proxy == nil {
		return State{}, newError(KindCaptureNotActive, "no capture is active")
	}
	ps := s.proxy.Status()
	if ps.State != "passthrough" || ps.CaptureAddress != expectedListener {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not in passthrough")
	}
	return s.snapshotLocked(), nil
}

// PauseDesktopExpected pauses observation while retaining desktop ownership.
// The public PauseCapture path continues to reject desktop_managed captures;
// this method is the narrowly-scoped transaction primitive for Disable.
func (s *Service) PauseDesktopExpected(ctx context.Context, expectedGeneration uint64, gatewayInstanceID, gatewayAddress, ownerID string) (State, error) {
	for {
		if err := ctx.Err(); err != nil {
			return State{}, err
		}
		s.mu.Lock()
		if s.closeOp != nil {
			s.mu.Unlock()
			return State{}, newError(KindCaptureClosing, "capture close is in progress")
		}
		if ownerID == "" || s.desktopOwnerID != ownerID {
			s.mu.Unlock()
			return State{}, newError(KindCaptureOperationSuperseded, "desktop ownership was superseded")
		}
		if s.activeStartID != 0 || s.activeOp != nil {
			s.mu.Unlock()
			return State{}, newError(KindCaptureAlreadyActive, "capture operation is in progress")
		}
		if s.mode == ModeRecovery {
			s.mu.Unlock()
			return State{}, newError(KindRecoveryConfirmationRequired, "capture requires recovery confirmation")
		}
		if s.mode != ModeDesktop {
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st, newError(KindIntegrationManagedByDesktop, "capture is not desktop-managed")
		}
		if s.generation != expectedGeneration {
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st, newError(KindCaptureGenerationMismatch, "capture generation does not match")
		}
		if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st, newError(KindGatewayMismatch, "gateway identity does not match the capture")
		}
		if s.proxy == nil {
			s.mu.Unlock()
			return State{}, newError(KindCaptureNotActive, "no capture is active")
		}
		proxy := s.proxy
		proxyState := proxy.Status().State
		if proxyState == "passthrough" {
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st, nil
		}
		if !isCapturing(proxyState) {
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st, newError(KindCaptureNotActive, "capture is not capturing")
		}
		op := s.reserveProxyOperationLocked(proxyOperationPause, proxy)
		s.mu.Unlock()

		pauseErr := proxy.Pause()

		s.mu.Lock()
		ownsOperation := s.ownsProxyOperationLocked(op)
		if ownsOperation && pauseErr != nil {
			s.mode = ModeRecovery
			s.lastError = "capture pause failed"
		}
		if ownsOperation {
			s.finishProxyOperationLocked(op)
			st := s.snapshotLocked()
			s.mu.Unlock()
			if pauseErr != nil {
				return st, newError(KindCaptureStopFailed, "capture pause failed")
			}
			return st, nil
		}
		st := s.snapshotLocked()
		s.finishProxyOperationLocked(op)
		s.mu.Unlock()
		return st, newError(KindCaptureOperationSuperseded, "capture pause was superseded")
	}
}

// ReleaseDesktop returns an active desktop-managed capture to the caller's
// chosen next mode (idle or capture_only). It never stops the capture itself.
func (s *Service) ReleaseDesktop(next ManagementMode) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.mode != ModeDesktop {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not desktop-managed")
	}
	switch next {
	case ModeIdle, ModeCaptureOnly:
		s.mode = next
	default:
		return s.snapshotLocked(), newError(KindCaptureNotActive, "invalid release target mode")
	}
	s.desktopOwnerID = ""
	return s.snapshotLocked(), nil
}

// ReleaseDesktopExpected returns desktop ownership to capture_only without
// touching the proxy, listener, generation, identity, or observations. The
// expected generation and owner ID prevent a stale transaction from releasing
// a newer Desktop ownership. Idle is reached only through CloseCapture.
func (s *Service) ReleaseDesktopExpected(expectedGeneration uint64, ownerID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return State{}, newError(KindCaptureClosing, "capture close is in progress")
	}
	if ownerID == "" || s.desktopOwnerID != ownerID {
		return s.snapshotLocked(), newError(KindCaptureOperationSuperseded, "desktop ownership was superseded")
	}
	if s.activeStartID != 0 || s.activeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureAlreadyActive, "capture operation is in progress")
	}
	if s.mode != ModeDesktop {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not desktop-managed")
	}
	if s.generation != expectedGeneration {
		return s.snapshotLocked(), newError(KindCaptureGenerationMismatch, "capture generation does not match")
	}
	if s.proxy == nil {
		return State{}, newError(KindCaptureNotActive, "no capture is active")
	}
	if ownerID == "" || s.desktopOwnerID != ownerID {
		return s.snapshotLocked(), newError(KindCaptureOperationSuperseded, "desktop ownership was superseded")
	}
	s.mode = ModeCaptureOnly
	s.desktopOwnerID = ""
	return s.snapshotLocked(), nil
}

// SetDesktopModelMappingExpected registers the target and ownership evidence
// for one owned Traffic relay as an unbound (pending) mapping. The exact source
// model is lazily bound from the first observed relay request; the sourceModel
// argument is informational and not stored. The mapping is accepted only after
// all current generation, Gateway identity, and Desktop ownership evidence
// matches.
func (s *Service) SetDesktopModelMappingExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress, ownerID, sourceModel, targetRoute string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if targetRoute == "" || ownerID == "" {
		return newError(KindModelMappingInvalid, "model mapping is invalid")
	}
	if s.closeOp != nil || s.activeStartID != 0 || s.activeOp != nil {
		return newError(KindCaptureOperationSuperseded, "capture operation is in progress")
	}
	if s.mode != ModeDesktop || s.desktopOwnerID != ownerID {
		return newError(KindCaptureOperationSuperseded, "desktop ownership was superseded")
	}
	if s.generation != expectedGeneration {
		return newError(KindCaptureGenerationMismatch, "capture generation does not match")
	}
	if s.gatewayID != gatewayInstanceID || s.gatewayAddr != gatewayAddress {
		return newError(KindGatewayMismatch, "gateway identity does not match the capture")
	}
	if s.proxy == nil || !isCapturing(s.proxy.Status().State) {
		return newError(KindCaptureNotActive, "capture is not capturing")
	}
	if s.modelMapping != nil {
		// Pending registrations carry no source, so redeclaration is compared
		// on identity and target only; the source is lazily bound later.
		if s.modelMapping.targetRoute == targetRoute && s.modelMapping.generation == expectedGeneration &&
			s.modelMapping.gatewayID == gatewayInstanceID && s.modelMapping.gatewayAddress == gatewayAddress &&
			s.modelMapping.ownerID == ownerID {
			return nil
		}
		return newError(KindCaptureOperationSuperseded, "model mapping is already claimed")
	}
	s.modelMapping = &modelMapping{
		sourceBound: false, targetRoute: targetRoute, generation: expectedGeneration,
		gatewayID: gatewayInstanceID, gatewayAddress: gatewayAddress, ownerID: ownerID,
	}
	return nil
}

// ClearDesktopModelMappingExpected clears the process-local mapping during a
// transaction backout. It is idempotent for an absent mapping, but refuses to
// clear a mapping belonging to another generation, Gateway, or owner.
func (s *Service) ClearDesktopModelMappingExpected(expectedGeneration uint64, gatewayInstanceID, gatewayAddress, ownerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelMapping == nil {
		return nil
	}
	m := s.modelMapping
	if m.generation != expectedGeneration || m.gatewayID != gatewayInstanceID || m.gatewayAddress != gatewayAddress || m.ownerID != ownerID {
		return newError(KindCaptureOperationSuperseded, "model mapping ownership was superseded")
	}
	s.modelMapping = nil
	return nil
}

// modelMappingDiagnosticLocked renders the mapping guard states as tri-state
// strings. It must be called with s.mu held. It is read-only observational:
// its output never influences ModelMappingFor's own decision.
func (s *Service) modelMappingDiagnosticLocked(sourceModel string) modelMappingDiag {
	d := modelMappingDiag{
		mappingPresent:    triFalse,
		sourceState:       triNA,
		sourceModelMatch:  triNA,
		generationMatch:   triNA,
		gatewayMatch:      triNA,
		relayActive:       triNA,
		ownerMatch:        triNA,
		lazyBindAttempted: triNA,
		lazyBindSuccess:   triNA,
	}
	m := s.modelMapping
	if m == nil {
		return d
	}
	d.mappingPresent = triTrue
	if m.sourceBound {
		d.sourceState = triBound
		d.sourceModelMatch = boolToTri(m.sourceModel == sourceModel)
	} else {
		d.sourceState = triUnbound
	}
	d.generationMatch = boolToTri(m.generation == s.generation)
	d.gatewayMatch = boolToTri(m.gatewayID == s.gatewayID && m.gatewayAddress == s.gatewayAddr)
	if s.proxy != nil {
		state := s.proxy.Status().State
		relayOK := state == "capturing" || state == "passthrough"
		if s.mode == ModeCaptureOnly {
			relayOK = state == "passthrough"
		}
		d.relayActive = boolToTri(relayOK)
	} else {
		d.relayActive = triFalse
	}
	if s.mode == ModeDesktop {
		d.ownerMatch = boolToTri(s.desktopOwnerID == m.ownerID)
	}
	return d
}

// logMappingDiag renders the secret-free tri-state routing diagnostic. It
// contains no model names, target aliases, gateway identities, or URLs.
func logMappingDiag(d modelMappingDiag) {
	log.Printf("traffic model routing: mapping_present=%s source_state=%s source_model_match=%s generation_match=%s gateway_match=%s relay_active=%s owner_match=%s lazy_bind_attempted=%s lazy_bind_success=%s",
		d.mappingPresent, d.sourceState, d.sourceModelMatch, d.generationMatch, d.gatewayMatch, d.relayActive, d.ownerMatch, d.lazyBindAttempted, d.lazyBindSuccess)
}

// mappingExactMatchLocked returns the target route for an exact source match on
// a bound mapping. It must be called with s.mu held and mirrors the relay
// contract: Desktop requires owner match, CaptureOnly requires passthrough.
func (s *Service) mappingExactMatchLocked(sourceModel string) (string, bool) {
	m := s.modelMapping
	if sourceModel == "" || m == nil || !m.sourceBound || m.sourceModel != sourceModel ||
		m.generation != s.generation || m.gatewayID != s.gatewayID ||
		m.gatewayAddress != s.gatewayAddr || s.proxy == nil {
		return "", false
	}
	state := s.proxy.Status().State
	switch s.mode {
	case ModeDesktop:
		if s.desktopOwnerID != m.ownerID {
			return "", false
		}
	case ModeCaptureOnly:
		if state != "passthrough" {
			return "", false
		}
	default:
		return "", false
	}
	if state != "capturing" && state != "passthrough" {
		return "", false
	}
	return m.targetRoute, true
}

// ModelMappingFor returns a target route only for an active exact mapping.
// It is an in-process read surface for the Gateway resolver; model names are
// never included in public Traffic state, recovery JSON, or logs.
func (s *Service) ModelMappingFor(sourceModel string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	logMappingDiag(s.modelMappingDiagnosticLocked(sourceModel))
	return s.mappingExactMatchLocked(sourceModel)
}

// ObservedModelFor routes an observed request model through the active relay
// mapping. On an unbound (pending) mapping it lazily binds the first request
// that provably traversed the Capture relay — relayMarker must equal the owning
// Gateway instance ID — while capturing. On a bound mapping it only ever exact
// matches, in capturing or passthrough, and never rebinds. The first request is
// resolved within the same call so it is never a 404-then-success.
func (s *Service) ObservedModelFor(sourceModel, relayMarker string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.modelMapping
	if m == nil {
		logMappingDiag(s.modelMappingDiagnosticLocked(sourceModel))
		return "", false
	}
	if !m.sourceBound {
		bound := sourceModel != "" && relayMarker == s.gatewayID && s.mode == ModeDesktop &&
			s.desktopOwnerID == m.ownerID && s.generation == m.generation &&
			s.gatewayID == m.gatewayID && s.gatewayAddr == m.gatewayAddress &&
			s.proxy != nil && isCapturing(s.proxy.Status().State)
		if bound {
			m.sourceModel = sourceModel
			m.sourceBound = true
		}
		diag := s.modelMappingDiagnosticLocked(sourceModel)
		if bound {
			diag.lazyBindAttempted = triTrue
			diag.lazyBindSuccess = triTrue
		}
		logMappingDiag(diag)
		if bound {
			return m.targetRoute, true
		}
		return "", false
	}
	logMappingDiag(s.modelMappingDiagnosticLocked(sourceModel))
	return s.mappingExactMatchLocked(sourceModel)
}

// MarkRecovery moves any mode into recovery_required, recording that an
// explicit confirmation is needed before capture is touched again. It never
// auto-clears.
func (s *Service) MarkRecovery() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = ModeRecovery
	return s.snapshotLocked(), nil
}

// ClearRecovery leaves recovery_required to the caller's chosen mode. It is
// explicit-only; nothing automatically clears recovery.
func (s *Service) ClearRecovery(next ManagementMode) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != ModeRecovery {
		return s.snapshotLocked(), newError(KindCaptureNotActive, "capture is not in recovery")
	}
	switch next {
	case ModeIdle, ModeCaptureOnly:
		s.mode = next
		s.desktopOwnerID = ""
	default:
		return s.snapshotLocked(), newError(KindCaptureNotActive, "invalid recovery target mode")
	}
	return s.snapshotLocked(), nil
}

// gateway lifecycle (internal, driven by Gateway via TrafficLifecycle callbacks)

// BindGatewayRun registers a gateway run identity. A committed proxy cannot
// be rebound to a different run; a start reservation may be superseded by a
// new bind and will fail its commit gate without changing committed state.
func (s *Service) BindGatewayRun(instanceID, address string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOp != nil {
		return s.snapshotLocked(), newError(KindCaptureClosing, "capture close is in progress")
	}
	if s.proxy != nil {
		if s.gatewayID == instanceID && s.gatewayAddr == address {
			return s.snapshotLocked(), nil
		}
		return s.snapshotLocked(), newError(KindGatewayMismatch, "capture is owned by another gateway run")
	}
	s.gatewayID = instanceID
	s.gatewayAddr = address
	return s.snapshotLocked(), nil
}

// MarkGatewayLost moves the capture to a safe state when the terminating run
// matches the recorded gateway identity. A stale run (instance id mismatch) is
// a no-op and never disturbs a newer capture. Normal stops (graceful shutdown,
// restart, user stop) pause or stop the capture and clear identity without
// entering recovery. Abnormal termination (crash, unexpected failure) enters
// recovery_required. In desktop_managed mode it does not finalize any recovery
// transaction; it only transitions mode so the next capture operation requires
// explicit confirmation.
//
// Blocking proxy operations (Pause/Stop) are performed outside the lock.
// After the proxy operation completes, the method re-acquires the lock and
// verifies the proxy is still the same one it started with.
func (s *Service) MarkGatewayLost(instanceID string, abnormal bool) State {
	for {
		s.mu.Lock()
		if s.closeOp != nil {
			op := s.closeOp
			s.mu.Unlock()
			// Close owns the proxy. EndRun joins it and must not issue a
			// second Stop/Pause after that operation completes.
			_, _ = waitCloseOperation(context.Background(), op)
			return s.Status()
		}
		if s.activeOp != nil {
			op := s.activeOp
			s.mu.Unlock()
			_ = waitProxyOperation(context.Background(), op)
			continue
		}
		if s.gatewayID == "" || s.gatewayID != instanceID {
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st
		}
		// A Gateway run ending invalidates all in-process routing evidence before
		// any proxy cleanup, including abnormal/recovery transitions.
		s.modelMapping = nil

		proxy := s.proxy
		if proxy == nil {
			if abnormal {
				s.mode = ModeRecovery
				s.lastError = "gateway run lost"
			} else {
				s.gatewayID = ""
				s.gatewayAddr = ""
				s.mode = ModeIdle
				s.lastError = ""
			}
			st := s.snapshotLocked()
			s.mu.Unlock()
			return st
		}

		op := s.reserveProxyOperationLocked(proxyOperationGatewayEnd, proxy)
		if abnormal {
			s.mode = ModeRecovery
			s.lastError = "gateway run lost"
		}
		s.mu.Unlock()

		var mutationErr error
		if abnormal {
			mutationErr = proxy.Pause()
		} else {
			mutationErr = proxy.Stop(context.Background())
		}

		s.mu.Lock()
		if s.ownsProxyOperationLocked(op) {
			if abnormal {
				if mutationErr != nil {
					s.lastError = "gateway run lost; capture pause failed"
				}
			} else if mutationErr != nil {
				s.mode = ModeRecovery
				s.lastError = "gateway stop failed; capture may still be active"
			} else {
				s.gatewayID = ""
				s.gatewayAddr = ""
				s.mode = ModeIdle
				s.lastError = ""
			}
		}
		s.finishProxyOperationLocked(op)
		st := s.snapshotLocked()
		s.mu.Unlock()
		return st
	}
}

// isCapturing reports whether the proxy wire state is actively capturing
// (as opposed to stopped/ready/passthrough/failed).
func isCapturing(state string) bool {
	switch state {
	case "capturing":
		return true
	}
	return false
}

// ManagementHandler returns the HTTP handler the desktopcontrol surface and
// external management API mount. It is bound to this Service so every caller
// reaches the same proxy/state/observations.
func (s *Service) ManagementHandler() http.Handler {
	return s.managementHandler()
}
