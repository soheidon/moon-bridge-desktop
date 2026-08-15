package trafficanalysis

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultCaptureAddress = "127.0.0.1:38441"
	DefaultUpstreamBase   = "https://chatgpt.com/backend-api/codex"
	managementPathPrefix  = "/api/v1/system/traffic-analysis/"
)

// RelayMarkerHeader names the origin-proof header the Capture relay stamps on
// forwarded requests. Its value is the owning gateway instance ID; the Gateway
// validates it before lazily binding a source model. A client-supplied value is
// always discarded, and the header never reaches traces or the upstream.
const RelayMarkerHeader = "X-Moonbridge-Relay"

// RequestCorrelationHeader is an internal loopback-only bridge from the
// Capture request observation to the Gateway event context. Capture removes
// any client-supplied value, stamps a short-lived local key, and Gateway
// consumes it before tracing or provider dispatch.
const RequestCorrelationHeader = "X-Moonbridge-Request"

type CaptureConfig struct {
	ListenAddr      string
	UpstreamBase    string
	InstanceID      string
	QueueSize       int
	RingCapacity    int
	HTTPClient      *http.Client
	WebSocketDialer *websocket.Dialer
}

type CaptureStatus struct {
	InstanceID                 string     `json:"instanceId"`
	State                      string     `json:"state"`
	SessionID                  string     `json:"sessionId,omitempty"`
	CaptureAddress             string     `json:"captureAddress"`
	UpstreamHost               string     `json:"upstreamHost"`
	StartedAt                  *time.Time `json:"startedAt,omitempty"`
	HTTPRequests               uint64     `json:"httpRequests"`
	SSEStreams                 uint64     `json:"sseStreams"`
	WebSocketConnections       uint64     `json:"websocketConnections"`
	ObservationCount           uint64     `json:"observationCount"`
	ObservationCapacity        uint64     `json:"observationCapacity"`
	DroppedObservations        uint64     `json:"droppedObservations"`
	DroppedBackpressure        uint64     `json:"droppedBackpressure"`
	ActiveHTTPRequests         uint64     `json:"activeHttpRequests"`
	ActiveWebSocketConnections uint64     `json:"activeWebsocketConnections"`
	LastSequence               uint64     `json:"lastSequence"`
	LastSafeError              string     `json:"lastSafeError,omitempty"`
}

type capturedWork struct {
	analyzer *Analyzer
	input    PayloadInput
	sample   []byte
	rawSize  int
	rawHMAC  string
	barrier  chan struct{}
}

type CaptureProxy struct {
	mu              sync.Mutex
	config          CaptureConfig
	server          *http.Server
	listener        net.Listener
	analyzer        *Analyzer
	queue           chan capturedWork
	workerDone      chan struct{}
	closed          chan struct{}
	observationGate sync.RWMutex
	recording       atomic.Bool

	state               string
	pauseDone           chan struct{}
	pauseResult         error
	startedAt           time.Time
	lastError           string
	activeHTTP          uint64
	activeWS            uint64
	httpCount           uint64
	sseCount            uint64
	wsCount             uint64
	droppedBackpressure uint64
	requestSequence     uint64

	connections map[*websocket.Conn]struct{}

	// pauseBarrierDelay is a test-only seam used to exercise the drain timeout
	// without exposing a production fault-injection endpoint or environment
	// variable.
	pauseBarrierDelay time.Duration
}

func NewCaptureProxy(config CaptureConfig) *CaptureProxy {
	if config.ListenAddr == "" {
		config.ListenAddr = DefaultCaptureAddress
	}
	if config.UpstreamBase == "" {
		config.UpstreamBase = DefaultUpstreamBase
	}
	if config.QueueSize <= 0 {
		config.QueueSize = 256
	}
	if config.RingCapacity <= 0 {
		config.RingCapacity = DefaultRingCapacity
	}
	analyzer, err := NewAnalyzer(config.RingCapacity)
	if err != nil {
		panic("traffic analysis: unable to create analyzer")
	}
	p := &CaptureProxy{
		config:      config,
		analyzer:    analyzer,
		queue:       make(chan capturedWork, config.QueueSize),
		workerDone:  make(chan struct{}),
		closed:      make(chan struct{}),
		state:       "stopped",
		connections: make(map[*websocket.Conn]struct{}),
		pauseDone:   make(chan struct{}),
	}
	// The analyzer is available for network-independent relay tests before the
	// listener starts; the listener itself is the production readiness boundary.
	p.recording.Store(true)
	go p.consume()
	return p
}

// captureStartFailure classifies a proxy.Start failure with a fixed, secret-free
// stage so the transaction binding can log the exact sub-branch. Error() returns
// only the stage; Unwrap preserves the underlying cause (which may contain an
// address and must never be logged).
type captureStartFailure struct {
	stage string
	err   error
}

func (e *captureStartFailure) Error() string { return e.stage }
func (e *captureStartFailure) Unwrap() error { return e.err }

func startFailure(stage string, err error) error {
	return &captureStartFailure{stage: stage, err: err}
}

func captureStartStage(err error) string {
	var f *captureStartFailure
	if errors.As(err, &f) {
		return f.stage
	}
	return ""
}

func (p *CaptureProxy) Start() error {
	p.mu.Lock()
	if p.state == "capturing" || p.state == "ready" {
		p.mu.Unlock()
		return nil
	}
	if p.state == "passthrough" {
		p.mu.Unlock()
		return startFailure("relay_active", errors.New("capture relay is still active"))
	}
	if !isLoopbackCaptureAddress(p.config.ListenAddr) {
		p.mu.Unlock()
		return startFailure("loopback", errors.New("capture listener must use a loopback address"))
	}
	listener, err := net.Listen("tcp", p.config.ListenAddr)
	if err != nil {
		p.state = "failed"
		p.lastError = safeNetworkError(err)
		p.mu.Unlock()
		return startFailure("bind", fmt.Errorf("start capture listener: %w", err))
	}
	analyzer, err := NewAnalyzer(p.config.RingCapacity)
	if err != nil {
		_ = listener.Close()
		p.state = "failed"
		p.lastError = "analyzer_initialization_failed"
		p.mu.Unlock()
		return startFailure("analyzer", err)
	}
	p.listener = listener
	p.analyzer = analyzer
	p.server = &http.Server{Handler: http.HandlerFunc(p.serveHTTP)}
	server := p.server
	p.startedAt = time.Now().UTC()
	p.state = "capturing"
	p.lastError = ""
	p.recording.Store(true)
	p.mu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.mu.Lock()
			p.state = "failed"
			p.lastError = safeNetworkError(err)
			p.mu.Unlock()
		}
	}()
	return nil
}

// Pause stops observation recording while keeping the Capture listener and all
// relay connections alive for a Codex client that has not reloaded its config.
func (p *CaptureProxy) Pause() error {
	p.mu.Lock()
	if p.state == "passthrough" {
		p.mu.Unlock()
		return nil
	}
	if p.state == "stopped" {
		p.mu.Unlock()
		return nil
	}
	if p.state == "draining" {
		done := p.pauseDone
		p.mu.Unlock()
		select {
		case <-done:
			p.mu.Lock()
			err := p.pauseResult
			p.mu.Unlock()
			return err
		case <-time.After(5 * time.Second):
			return errors.New("capture pause drain timeout")
		}
	}
	if p.state != "capturing" && p.state != "ready" {
		p.mu.Unlock()
		return errors.New("capture is not pausable")
	}
	p.state = "draining"
	p.pauseDone = make(chan struct{})
	done := p.pauseDone
	p.observationGate.Lock()
	p.recording.Store(false)
	p.observationGate.Unlock()
	barrier := make(chan struct{})
	select {
	case p.queue <- capturedWork{barrier: barrier}:
	case <-p.closed:
		p.state = "capturing"
		p.recording.Store(true)
		close(done)
		p.mu.Unlock()
		return errors.New("capture is closed")
	}
	p.mu.Unlock()
	select {
	case <-barrier:
		p.mu.Lock()
		p.state = "passthrough"
		p.pauseResult = nil
		close(done)
		p.mu.Unlock()
		return nil
	case <-time.After(5 * time.Second):
		p.mu.Lock()
		p.recording.Store(true)
		p.state = "capturing"
		p.pauseResult = errors.New("capture pause drain timeout")
		close(done)
		p.mu.Unlock()
		return p.pauseResult
	}
}

// Resume returns a paused (passthrough) capture back to the capturing state
// while keeping the same listener and relay connections. It is a no-op when
// the proxy is already capturing/ready.
func (p *CaptureProxy) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "passthrough" {
		p.state = "capturing"
		p.recording.Store(true)
		return nil
	}
	if p.state == "capturing" || p.state == "ready" {
		return nil
	}
	return errors.New("capture is not resumable")
}

// Stop stops the capture listener while keeping the proxy reference alive.
func (p *CaptureProxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	server := p.server
	p.observationGate.Lock()
	p.recording.Store(false)
	p.observationGate.Unlock()
	p.state = "draining"
	p.mu.Unlock()
	if server == nil {
		p.mu.Lock()
		p.state = "stopped"
		p.mu.Unlock()
		return nil
	}
	p.closeConnections()
	err := server.Shutdown(ctx)
	p.mu.Lock()
	p.server = nil
	p.listener = nil
	if err != nil {
		p.state = "failed"
		p.lastError = safeNetworkError(err)
	} else {
		p.state = "stopped"
	}
	p.mu.Unlock()
	return err
}

func (p *CaptureProxy) Close() error {
	select {
	case <-p.closed:
		return nil
	default:
		close(p.closed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.Stop(ctx)
	close(p.queue)
	<-p.workerDone
	return err
}

func (p *CaptureProxy) Status() CaptureStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := CaptureStatus{
		InstanceID:                 p.config.InstanceID,
		State:                      p.state,
		CaptureAddress:             p.config.ListenAddr,
		UpstreamHost:               upstreamHost(p.config.UpstreamBase),
		LastSafeError:              p.lastError,
		ActiveHTTPRequests:         atomic.LoadUint64(&p.activeHTTP),
		ActiveWebSocketConnections: atomic.LoadUint64(&p.activeWS),
		HTTPRequests:               atomic.LoadUint64(&p.httpCount),
		SSEStreams:                 atomic.LoadUint64(&p.sseCount),
		WebSocketConnections:       atomic.LoadUint64(&p.wsCount),
		ObservationCapacity:        uint64(p.config.RingCapacity),
		DroppedBackpressure:        atomic.LoadUint64(&p.droppedBackpressure),
	}
	if !p.startedAt.IsZero() {
		started := p.startedAt
		status.StartedAt = &started
	}
	if p.listener != nil {
		status.CaptureAddress = p.listener.Addr().String()
	}
	if p.analyzer != nil {
		status.SessionID = p.analyzer.SessionID()
		items, dropped := p.analyzer.Snapshot(0)
		status.ObservationCount = uint64(len(items))
		status.DroppedObservations = dropped + status.DroppedBackpressure
		if len(items) > 0 {
			status.LastSequence = items[len(items)-1].Sequence
		}
	}
	return status
}

func (p *CaptureProxy) Observations(after uint64) ([]Observation, uint64) {
	p.mu.Lock()
	analyzer := p.analyzer
	droppedBackpressure := atomic.LoadUint64(&p.droppedBackpressure)
	p.mu.Unlock()
	if analyzer == nil {
		return nil, droppedBackpressure
	}
	items, dropped := analyzer.Snapshot(after)
	return items, dropped + droppedBackpressure
}

// RecordGatewayEvent records a secret-safe internal event in the same ring as
// payload observations. It is a no-op while paused or after shutdown.
func (p *CaptureProxy) RecordGatewayEvent(input GatewayEventInput) {
	p.observationGate.RLock()
	defer p.observationGate.RUnlock()
	if !p.recording.Load() {
		return
	}
	p.mu.Lock()
	analyzer := p.analyzer
	p.mu.Unlock()
	if analyzer != nil {
		analyzer.RecordGatewayEvent(input)
	}
}

func (p *CaptureProxy) Clear() {
	p.mu.Lock()
	analyzer := p.analyzer
	p.mu.Unlock()
	if analyzer != nil {
		analyzer.Clear()
	}
}

// StateFailed reports whether the proxy has entered the failed state. It is
// used by the owning Service to decide whether a failed operation should
// release the proxy for replacement. It is safe to call concurrently.
func (p *CaptureProxy) StateFailed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == "failed"
}

// StateStopped reports whether the proxy's listener is stopped (after
// StopCapture). It is used by the owning Service to decide whether a
// stopped proxy can be auto-closed for restart. It is safe to call concurrently.
func (p *CaptureProxy) StateStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == "stopped"
}

// setPauseBarrierDelay is intentionally private and only used by package
// tests to reproduce a stalled observation worker.
func (p *CaptureProxy) setPauseBarrierDelay(delay time.Duration) {
	p.mu.Lock()
	p.pauseBarrierDelay = delay
	p.mu.Unlock()
}

func (p *CaptureProxy) consume() {
	defer close(p.workerDone)
	for work := range p.queue {
		if work.barrier != nil {
			p.mu.Lock()
			delay := p.pauseBarrierDelay
			p.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			close(work.barrier)
			continue
		}
		input := work.input
		input.Payload = work.sample
		input.rawPayloadSize = work.rawSize
		input.rawPayloadHMAC = work.rawHMAC
		input.hasRawPayloadOverride = true
		work.analyzer.Record(input)
	}
}

func (p *CaptureProxy) enqueue(analyzer *Analyzer, input PayloadInput, sample []byte, rawSize int, rawHMAC string) {
	p.observationGate.RLock()
	defer p.observationGate.RUnlock()
	if !p.recording.Load() {
		return
	}
	work := capturedWork{analyzer: analyzer, input: input, sample: append([]byte(nil), sample...), rawSize: rawSize, rawHMAC: rawHMAC}
	select {
	case p.queue <- work:
	default:
		atomic.AddUint64(&p.droppedBackpressure, 1)
	}
}

func (p *CaptureProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__moonbridge_capture_ready" && r.Method == http.MethodGet {
		writeCaptureJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	upstream, err := composeUpstreamURL(p.config.UpstreamBase, r)
	if err != nil {
		logCaptureRequestTarget(r, p.config.UpstreamBase, err)
		http.Error(w, "invalid capture request target", http.StatusBadRequest)
		return
	}
	logCaptureRequestTarget(r, p.config.UpstreamBase, nil)
	if websocket.IsWebSocketUpgrade(r) {
		p.serveWebSocket(w, r, upstream)
		return
	}
	p.serveHTTPForward(w, r, upstream)
}

func (p *CaptureProxy) serveHTTPForward(w http.ResponseWriter, r *http.Request, upstream *url.URL) {
	atomic.AddUint64(&p.httpCount, 1)
	atomic.AddUint64(&p.activeHTTP, 1)
	defer atomic.AddUint64(&p.activeHTTP, ^uint64(0))
	p.mu.Lock()
	analyzer := p.analyzer
	recording := p.recording.Load()
	state := p.state
	p.mu.Unlock()
	if analyzer == nil || state == "stopped" || state == "failed" {
		http.Error(w, "capture is not ready", http.StatusServiceUnavailable)
		return
	}
	requestPath := r.URL.EscapedPath()
	correlationKey := fmt.Sprintf("capture-request-%d", atomic.AddUint64(&p.requestSequence, 1))
	inputHeaders := r.Header.Clone()
	inputHeaders.Del(RequestCorrelationHeader)
	requestInput := PayloadInput{Direction: DirectionClientToUpstream, Transport: TransportHTTP, Method: r.Method,
		RequestModelEligible: r.Method == http.MethodPost && (requestPath == "/responses" || requestPath == "/v1/responses"),
		CorrelationKey:       correlationKey,
		ReceivedHost:         r.Host, ReceivedPath: requestPath, UpstreamHost: upstream.Host, UpstreamPath: upstream.EscapedPath(),
		QueryParameterNames: queryNames(r.URL), Headers: inputHeaders, ContentType: r.Header.Get("Content-Type"), ContentEncoding: r.Header.Get("Content-Encoding")}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	var requestTap *payloadTap
	if recording {
		requestTap = newPayloadTap(body, analyzer, func(partial bool, sample []byte, rawSize int, rawHMAC string) {
			requestInput.Partial = partial
			p.enqueue(analyzer, requestInput, sample, rawSize, rawHMAC)
		})
	}
	requestInput.Partial = false
	outgoing := r.Clone(r.Context())
	outgoing.URL = upstream
	outgoing.Host = upstream.Host
	outgoing.RequestURI = ""
	outgoing.Header = cloneForwardHeaders(r.Header)
	// Stamp the relay origin proof. The cloned header may carry a spoofed
	// client value, so it is stripped first and exactly one Capture-generated
	// value is set when the relay owns a gateway identity.
	outgoing.Header.Del(RelayMarkerHeader)
	outgoing.Header.Del(RequestCorrelationHeader)
	if p.config.InstanceID != "" {
		outgoing.Header.Set(RelayMarkerHeader, p.config.InstanceID)
	}
	outgoing.Header.Set(RequestCorrelationHeader, correlationKey)
	if requestTap != nil {
		outgoing.Body = requestTap
	} else {
		outgoing.Body = body
	}
	client := p.config.HTTPClient
	if client == nil {
		client = &http.Client{Transport: captureTransport()}
	}
	client = cloneCaptureClient(client)
	response, err := client.Do(outgoing)
	if requestTap != nil {
		requestTap.finish(err != nil)
	}
	logCaptureRequestBody(r, requestTap)
	if err != nil {
		http.Error(w, "capture upstream request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	contentType := response.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		atomic.AddUint64(&p.sseCount, 1)
		if !recording {
			_, _ = io.Copy(flushingWriter{ResponseWriter: w}, response.Body)
			return
		}
		relay := newSSERelay(p, analyzer, response.Body, w, r, upstream, response.StatusCode, response.Header, correlationKey)
		_ = relay.copy()
		return
	}
	if !recording {
		_, _ = io.Copy(flushingWriter{ResponseWriter: w}, response.Body)
		return
	}
	responseInput := PayloadInput{Direction: DirectionUpstreamToClient, Transport: TransportHTTP, Method: r.Method, CorrelationKey: correlationKey,
		ReceivedHost: r.Host, ReceivedPath: r.URL.EscapedPath(), UpstreamHost: upstream.Host, UpstreamPath: upstream.EscapedPath(),
		QueryParameterNames: queryNames(r.URL), Headers: response.Header, StatusCode: response.StatusCode,
		ContentType: response.Header.Get("Content-Type"), ContentEncoding: response.Header.Get("Content-Encoding")}
	responseTap := newPayloadTap(response.Body, analyzer, func(partial bool, sample []byte, rawSize int, rawHMAC string) {
		responseInput.Partial = partial
		p.enqueue(analyzer, responseInput, sample, rawSize, rawHMAC)
	})
	_, copyErr := io.Copy(flushingWriter{ResponseWriter: w}, responseTap)
	responseTap.finish(copyErr != nil)
}

func (p *CaptureProxy) serveWebSocket(w http.ResponseWriter, r *http.Request, upstream *url.URL) {
	p.mu.Lock()
	analyzer := p.analyzer
	p.mu.Unlock()
	if analyzer == nil {
		http.Error(w, "capture is not ready", http.StatusServiceUnavailable)
		return
	}
	if upstream.Scheme == "https" {
		upstream.Scheme = "wss"
	} else {
		upstream.Scheme = "ws"
	}
	upstreamHeader := cloneForwardHeaders(r.Header)
	upstreamHeader.Del("Sec-WebSocket-Key")
	upstreamHeader.Del("Sec-WebSocket-Version")
	upstreamHeader.Del("Sec-WebSocket-Extensions")
	upstreamHeader.Del("Sec-WebSocket-Protocol")
	dialer := websocket.Dialer{EnableCompression: false}
	if p.config.WebSocketDialer != nil {
		dialer = *p.config.WebSocketDialer
		dialer.EnableCompression = false
	}
	upstreamConn, _, err := dialer.DialContext(r.Context(), upstream.String(), upstreamHeader)
	if err != nil {
		http.Error(w, "capture websocket upstream handshake failed", http.StatusBadGateway)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: false}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = upstreamConn.Close()
		return
	}
	p.registerConnection(clientConn)
	p.registerConnection(upstreamConn)
	defer p.unregisterConnection(clientConn)
	defer p.unregisterConnection(upstreamConn)
	defer clientConn.Close()
	defer upstreamConn.Close()
	atomic.AddUint64(&p.wsCount, 1)
	atomic.AddUint64(&p.activeWS, 1)
	defer atomic.AddUint64(&p.activeWS, ^uint64(0))

	clientWriter := &wsWriter{conn: clientConn}
	upstreamWriter := &wsWriter{conn: upstreamConn}
	clientConn.SetPingHandler(func(data string) error {
		p.enqueueControl(analyzer, DirectionClientToUpstream, TransportWebSocket, "ping", len(data))
		return upstreamWriter.control(websocket.PingMessage, []byte(data))
	})
	clientConn.SetPongHandler(func(data string) error {
		p.enqueueControl(analyzer, DirectionClientToUpstream, TransportWebSocket, "pong", len(data))
		return upstreamWriter.control(websocket.PongMessage, []byte(data))
	})
	upstreamConn.SetPingHandler(func(data string) error {
		p.enqueueControl(analyzer, DirectionUpstreamToClient, TransportWebSocket, "ping", len(data))
		return clientWriter.control(websocket.PingMessage, []byte(data))
	})
	upstreamConn.SetPongHandler(func(data string) error {
		p.enqueueControl(analyzer, DirectionUpstreamToClient, TransportWebSocket, "pong", len(data))
		return clientWriter.control(websocket.PongMessage, []byte(data))
	})

	result := make(chan error, 2)
	go p.relayWebSocket(result, analyzer, clientConn, upstreamWriter, DirectionClientToUpstream, r, upstream)
	go p.relayWebSocket(result, analyzer, upstreamConn, clientWriter, DirectionUpstreamToClient, r, upstream)
	<-result
	_ = clientWriter.control(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = upstreamWriter.control(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

func (p *CaptureProxy) relayWebSocket(result chan<- error, analyzer *Analyzer, source *websocket.Conn, destination *wsWriter, direction Direction, request *http.Request, upstream *url.URL) {
	for {
		messageType, payload, err := source.ReadMessage()
		if err != nil {
			result <- err
			return
		}
		messageName := "binary"
		if messageType == websocket.TextMessage {
			messageName = "text"
		}
		input := PayloadInput{Direction: direction, Transport: TransportWebSocket, Method: request.Method,
			ReceivedHost: request.Host, ReceivedPath: request.URL.EscapedPath(), UpstreamHost: upstream.Host, UpstreamPath: upstream.EscapedPath(),
			QueryParameterNames: queryNames(request.URL), WebSocketMessageType: messageName, ContentType: "application/json"}
		p.enqueuePayload(analyzer, input, payload, false)
		if err := destination.message(messageType, payload); err != nil {
			result <- err
			return
		}
	}
}

func (p *CaptureProxy) enqueuePayload(analyzer *Analyzer, input PayloadInput, payload []byte, partial bool) {
	if !p.recording.Load() {
		return
	}
	input.Partial = partial
	hashValue := hmacHex(analyzer.key, payload)
	p.enqueue(analyzer, input, boundedSample(payload), len(payload), hashValue)
}

func (p *CaptureProxy) enqueueControl(analyzer *Analyzer, direction Direction, transport Transport, messageType string, size int) {
	if !p.recording.Load() {
		return
	}
	input := PayloadInput{Direction: direction, Transport: transport, WebSocketMessageType: messageType}
	h := hmac.New(sha256.New, analyzer.key)
	_, _ = io.WriteString(h, fmt.Sprintf("control:%s:%d", messageType, size))
	p.enqueue(analyzer, input, nil, size, hex.EncodeToString(h.Sum(nil)))
}

func (p *CaptureProxy) registerConnection(conn *websocket.Conn) {
	p.mu.Lock()
	p.connections[conn] = struct{}{}
	p.mu.Unlock()
}
func (p *CaptureProxy) unregisterConnection(conn *websocket.Conn) {
	p.mu.Lock()
	delete(p.connections, conn)
	p.mu.Unlock()
}
func (p *CaptureProxy) closeConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.connections {
		_ = conn.Close()
	}
}

type payloadTap struct {
	reader   io.Reader
	analyzer *Analyzer
	hash     hash.Hash
	sample   []byte
	size     int
	finished bool
	onFinish func(bool, []byte, int, string)
	mu       sync.Mutex
}

func newPayloadTap(reader io.Reader, analyzer *Analyzer, onFinish func(bool, []byte, int, string)) *payloadTap {
	return &payloadTap{reader: reader, analyzer: analyzer, hash: hmac.New(sha256.New, analyzer.key), sample: make([]byte, 0, MaxRawAnalysisBytes), onFinish: onFinish}
}
func (t *payloadTap) Read(dst []byte) (int, error) {
	n, err := t.reader.Read(dst)
	if n > 0 {
		t.size += n
		_, _ = t.hash.Write(dst[:n])
		if len(t.sample) < MaxRawAnalysisBytes {
			take := MaxRawAnalysisBytes - len(t.sample)
			if take > n {
				take = n
			}
			t.sample = append(t.sample, dst[:take]...)
		}
	}
	return n, err
}
func (t *payloadTap) Close() error {
	if closer, ok := t.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
func (t *payloadTap) finish(partial bool) {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	sample := append([]byte(nil), t.sample...)
	rawSize := t.size
	rawHMAC := hex.EncodeToString(t.hash.Sum(nil))
	callback := t.onFinish
	t.mu.Unlock()
	if callback != nil {
		callback(partial, sample, rawSize, rawHMAC)
	}
}

type flushingWriter struct{ http.ResponseWriter }

func (w flushingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) message(kind int, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteMessage(kind, payload)
}
func (w *wsWriter) control(kind int, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteControl(kind, payload, time.Now().Add(5*time.Second))
}

type sseRelay struct {
	proxy          *CaptureProxy
	analyzer       *Analyzer
	reader         io.Reader
	writer         http.ResponseWriter
	request        *http.Request
	upstream       *url.URL
	status         int
	headers        http.Header
	correlationKey string
	pending        []byte
	event          string
	data           []byte
	hasData        bool
	hmac           hash.Hash
	size           int
}

func newSSERelay(proxy *CaptureProxy, analyzer *Analyzer, reader io.Reader, writer http.ResponseWriter, request *http.Request, upstream *url.URL, status int, headers http.Header, correlationKeys ...string) *sseRelay {
	correlationKey := ""
	if len(correlationKeys) > 0 {
		correlationKey = correlationKeys[0]
	}
	return &sseRelay{proxy: proxy, analyzer: analyzer, reader: reader, writer: writer, request: request, upstream: upstream, status: status, headers: headers, correlationKey: correlationKey, hmac: hmac.New(sha256.New, analyzer.key)}
}
func (s *sseRelay) copy() error {
	buf := make([]byte, 32*1024)
	writer := flushingWriter{ResponseWriter: s.writer}
	for {
		n, err := s.reader.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			s.feed(buf[:n])
		}
		if err != nil {
			if err == io.EOF {
				s.finish(true)
				return nil
			}
			s.finish(true)
			return err
		}
	}
}
func (s *sseRelay) feed(chunk []byte) {
	s.pending = append(s.pending, chunk...)
	for {
		index := bytesIndexLine(s.pending)
		if index < 0 {
			return
		}
		line := append([]byte(nil), s.pending[:index]...)
		consume := index + 1
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		s.pending = s.pending[consume:]
		if len(line) == 0 {
			s.finish(false)
			continue
		}
		switch {
		case strings.HasPrefix(string(line), "event:"):
			s.event = strings.TrimSpace(string(line[len("event:"):]))
		case strings.HasPrefix(string(line), "data:"):
			value := line[len("data:"):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if s.hasData {
				s.data = append(s.data, '\n')
				_, _ = s.hmac.Write([]byte{'\n'})
				s.size++
			}
			s.hasData = true
			s.size += len(value)
			_, _ = s.hmac.Write(value)
			if len(s.data) < MaxRawAnalysisBytes {
				take := MaxRawAnalysisBytes - len(s.data)
				if take > len(value) {
					take = len(value)
				}
				s.data = append(s.data, value[:take]...)
			}
		}
	}
}
func (s *sseRelay) finish(partial bool) {
	if !s.hasData && s.event == "" {
		return
	}
	input := PayloadInput{Direction: DirectionUpstreamToClient, Transport: TransportSSE, Method: s.request.Method, CorrelationKey: s.correlationKey, ReceivedHost: s.request.Host, ReceivedPath: s.request.URL.EscapedPath(), UpstreamHost: s.upstream.Host, UpstreamPath: s.upstream.EscapedPath(), QueryParameterNames: queryNames(s.request.URL), Headers: s.headers, ContentType: "text/event-stream", StatusCode: s.status, SSEEventType: s.event, Partial: partial}
	s.proxy.enqueue(s.analyzer, input, s.data, s.size, hex.EncodeToString(s.hmac.Sum(nil)))
	s.event = ""
	s.data = s.data[:0]
	s.hasData = false
	s.hmac = hmac.New(sha256.New, s.analyzer.key)
	s.size = 0
}

func bytesIndexLine(value []byte) int {
	for index, item := range value {
		if item == '\n' {
			return index
		}
	}
	return -1
}
func boundedSample(value []byte) []byte {
	if len(value) > MaxRawAnalysisBytes {
		return value[:MaxRawAnalysisBytes]
	}
	return value
}

var (
	errAbsoluteRequestTarget       = errors.New("absolute request target")
	errUnsafeRequestPath           = errors.New("unsafe request path")
	errInvalidEscapedPath          = errors.New("invalid escaped path")
	errTraversalPath               = errors.New("traversal path")
	errInvalidUpstreamBase         = errors.New("invalid upstream base")
	errHTTPUpstreamOutsideLoopback = errors.New("http upstream outside loopback")
	errUnsupportedUpstreamScheme   = errors.New("unsupported upstream scheme")
)

func composeUpstreamURL(base string, request *http.Request) (*url.URL, error) {
	if request == nil || request.URL == nil || request.URL.IsAbs() || request.URL.Host != "" {
		return nil, errAbsoluteRequestTarget
	}
	escaped := request.URL.EscapedPath()
	if escaped == "" {
		escaped = "/"
	}
	if !strings.HasPrefix(escaped, "/") || strings.HasPrefix(escaped, "//") || strings.ContainsAny(escaped, "\\\x00") {
		return nil, errUnsafeRequestPath
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, errInvalidEscapedPath
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return nil, errTraversalPath
		}
	}
	upstream, err := url.Parse(base)
	if err != nil || upstream.Host == "" {
		return nil, errInvalidUpstreamBase
	}
	switch upstream.Scheme {
	case "https":
		// The fixed default upstream (chatgpt.com) and any TLS upstream.
	case "http":
		// The desktop flow forwards to the local loopback Gateway only. Allowing
		// http to any host would turn the capture proxy into an open relay.
		if !isLoopbackHost(upstream.Hostname()) {
			return nil, errHTTPUpstreamOutsideLoopback
		}
	default:
		return nil, errUnsupportedUpstreamScheme
	}
	baseEscaped := strings.TrimRight(upstream.EscapedPath(), "/")
	upstream.Path = strings.TrimRight(upstream.Path, "/") + decoded
	upstream.RawPath = baseEscaped + escaped
	upstream.RawQuery = request.URL.RawQuery
	return upstream, nil
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func logCaptureRequestTarget(r *http.Request, upstreamBase string, err error) {
	form := "origin"
	path := ""
	hasQuery := false
	if r.URL != nil {
		if r.URL.IsAbs() || r.URL.Host != "" {
			form = "absolute"
		}
		path = safeCapturePath(r.URL.EscapedPath())
		hasQuery = r.URL.RawQuery != ""
	}
	reason := ""
	if err != nil {
		reason = captureTargetReason(err)
	}
	upstream, parseErr := url.Parse(upstreamBase)
	upstreamScheme := ""
	if parseErr == nil {
		upstreamScheme = upstream.Scheme
	}
	log.Printf("capture request: method=%q request_uri_form=%q path=%q has_query=%t upstream_scheme=%q accepted=%t reason=%q",
		r.Method, form, path, hasQuery, upstreamScheme, err == nil, reason)
}

// safeCapturePath is a log-display sanitizer only, not a forwarding allowlist:
// known diagnostic paths are echoed verbatim, anything else is folded to
// <other>. Arbitrary path forwarding is unaffected.
func safeCapturePath(path string) string {
	switch path {
	case "/responses", "/v1/responses", "/models", "/v1/models", "/socket":
		return path
	default:
		return "<other>"
	}
}

// captureTargetReason maps sentinel errors to safe enum strings. Anything
// unexpected is reported as unknown so raw error text is never logged.
func captureTargetReason(err error) string {
	switch {
	case errors.Is(err, errAbsoluteRequestTarget):
		return "absolute_request_target"
	case errors.Is(err, errUnsafeRequestPath):
		return "unsafe_request_path"
	case errors.Is(err, errInvalidEscapedPath):
		return "invalid_escaped_path"
	case errors.Is(err, errTraversalPath):
		return "traversal_path"
	case errors.Is(err, errInvalidUpstreamBase):
		return "invalid_upstream_base"
	case errors.Is(err, errHTTPUpstreamOutsideLoopback):
		return "http_upstream_outside_loopback"
	case errors.Is(err, errUnsupportedUpstreamScheme):
		return "unsupported_upstream_scheme"
	default:
		return "unknown"
	}
}

// bodyFirstByteClass classifies only the first significant byte of a body for
// secret-free diagnostics. It never exposes the body text itself. "other" means
// the body does not begin with a JSON object or array token.
func bodyFirstByteClass(sample []byte) string {
	for _, b := range sample {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return "object"
		case '[':
			return "array"
		default:
			return "other"
		}
	}
	return "empty"
}

// logCaptureRequestBody is a secret-free forwarding diagnostic. It reports only
// framing metadata and the first-byte JSON class of the forwarded body — never
// the body text, its raw digest, or any header value that could carry secrets.
func logCaptureRequestBody(r *http.Request, tap *payloadTap) {
	chunked := false
	for _, te := range r.TransferEncoding {
		if te == "chunked" {
			chunked = true
		}
	}
	size := 0
	firstByteClass := "empty"
	if tap != nil {
		tap.mu.Lock()
		size = tap.size
		firstByteClass = bodyFirstByteClass(tap.sample)
		tap.mu.Unlock()
	}
	log.Printf("capture request body: path=%q method=%q content_length=%d content_type=%q content_encoding=%q chunked=%t forwarded_bytes=%d first_byte_class=%q",
		r.URL.EscapedPath(), r.Method, r.ContentLength, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"),
		chunked, size, firstByteClass)
}

func cloneForwardHeaders(in http.Header) http.Header {
	out := in.Clone()
	removeHopHeaders(out)
	out.Del("Host")
	return out
}
func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
func removeHopHeaders(headers http.Header) {
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(key)
	}
	for _, value := range headers.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(key))
		}
	}
}
func isHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
func queryNames(value *url.URL) []string {
	if value == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for key := range value.Query() {
		seen[key] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
func parseAfter(value string) uint64 {
	var result uint64
	_, _ = fmt.Sscan(value, &result)
	return result
}
func isLoopbackCaptureAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func upstreamHost(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.Host
}
func captureTransport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	return transport
}
func cloneCaptureClient(in *http.Client) *http.Client {
	out := *in
	out.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if out.Transport == nil {
		out.Transport = captureTransport()
	}
	return &out
}
func safeNetworkError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "network_error"
}
func writeCaptureJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
