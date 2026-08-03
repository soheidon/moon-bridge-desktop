package trafficanalysis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCaptureHTTPForwardsPayloadAndSanitizesObservation(t *testing.T) {
	var upstreamBody string
	var upstreamAuth, upstreamCookie string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		upstreamAuth = r.Header.Get("Authorization")
		upstreamCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()

	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, HTTPClient: upstream.Client(), QueueSize: 16})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	address := proxy.Status().CaptureAddress
	body := `{"model":"gpt-5.6-luna","input":"SENTINEL_PROMPT_SECRET"}`
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/unknown/codex/path?token=SENTINEL_QUERY_SECRET", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer SENTINEL_AUTH_SECRET")
	request.Header.Set("Cookie", "session=SENTINEL_COOKIE_SECRET")
	request.Header.Set("Connection", "X-Capture-Test")
	request.Header.Set("X-Capture-Test", "SENTINEL_HOP_SECRET")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(responseBody) != `{"status":"ok"}` {
		t.Fatalf("response = %d %q", response.StatusCode, responseBody)
	}
	if upstreamBody != body || upstreamAuth != "Bearer SENTINEL_AUTH_SECRET" || upstreamCookie != "session=SENTINEL_COOKIE_SECRET" {
		t.Fatalf("upstream forwarding mismatch: body=%q auth=%q cookie=%q", upstreamBody, upstreamAuth, upstreamCookie)
	}

	observations := waitForObservations(t, proxy, 2)
	encoded, err := json.Marshal(observations)
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded)
	for _, sentinel := range []string{"SENTINEL_AUTH_SECRET", "SENTINEL_COOKIE_SECRET", "SENTINEL_PROMPT_SECRET", "SENTINEL_QUERY_SECRET", "SENTINEL_HOP_SECRET"} {
		if strings.Contains(output, sentinel) {
			t.Fatalf("observation contains sentinel %q: %s", sentinel, output)
		}
	}
	var sawRequest, sawResponse bool
	for _, observation := range observations {
		if observation.Direction == DirectionClientToUpstream {
			sawRequest = true
			if observation.StatusCode != 0 {
				t.Fatalf("request unexpectedly has status code: %+v", observation)
			}
		}
		if observation.Direction == DirectionUpstreamToClient {
			sawResponse = true
			if observation.StatusCode != http.StatusOK {
				t.Fatalf("response status code = %d", observation.StatusCode)
			}
		}
	}
	if !sawRequest || !sawResponse {
		t.Fatalf("missing request/response observations: %+v", observations)
	}
}

func TestCaptureRejectsUnsafeTargetsAndDoesNotDialUpstream(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/%2e%2e/secret", nil)
	if _, err := composeUpstreamURL(DefaultUpstreamBase, request); err == nil {
		t.Fatal("encoded traversal target was accepted")
	}
	absolute := httptest.NewRequest(http.MethodGet, "https://chatgpt.com/responses", nil)
	if _, err := composeUpstreamURL(DefaultUpstreamBase, absolute); err == nil {
		t.Fatal("absolute-form target was accepted")
	}
	valid := httptest.NewRequest(http.MethodGet, "/v1/responses?stream=true", nil)
	upstream, err := composeUpstreamURL(DefaultUpstreamBase, valid)
	if err != nil {
		t.Fatal(err)
	}
	if upstream.String() != "https://chatgpt.com/backend-api/codex/v1/responses?stream=true" {
		t.Fatalf("upstream URL = %q", upstream.String())
	}
}

func TestSSERelayPreservesBytesAcrossChunkBoundaries(t *testing.T) {
	proxy := NewCaptureProxy(CaptureConfig{QueueSize: 16})
	defer proxy.Close()
	proxy.mu.Lock()
	analyzer := proxy.analyzer
	proxy.mu.Unlock()
	request := httptest.NewRequest(http.MethodGet, "/responses", nil)
	upstream, _ := composeUpstreamURL(DefaultUpstreamBase, request)
	input := "event: response.created\ndata: {\"object\":\"response\"}\n\nevent: response.completed\ndata: {\"status\":\"completed\"}\n"
	recorder := httptest.NewRecorder()
	relay := newSSERelay(proxy, analyzer, &chunkReader{chunks: []string{"event: res", "ponse.created\nda", "ta: {\"object\":\"response\"}\n\n", "event: response.completed\ndata: {\"status\":\"completed\"}\n"}}, recorder, request, upstream, http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}})
	if err := relay.copy(); err != nil {
		t.Fatal(err)
	}
	observations := waitForObservations(t, proxy, 2)
	if len(observations) < 2 || observations[0].Transport != TransportSSE || observations[0].SSEEventType != "response.created" || observations[1].SSEEventType != "response.completed" {
		t.Fatalf("SSE observations = %+v", observations)
	}
	if recorder.Body.String() != input {
		t.Fatalf("SSE body changed: %q", recorder.Body.String())
	}
}

func TestCaptureWebSocketRoundTrip(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			kind, payload, err := connection.ReadMessage()
			if err != nil {
				return
			}
			_ = connection.WriteMessage(kind, payload)
		}
	}))
	defer upstream.Close()
	tlsConfig := upstream.Client().Transport.(*http.Transport).TLSClientConfig
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, WebSocketDialer: &websocket.Dialer{TLSClientConfig: tlsConfig}})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	client, _, err := (&websocket.Dialer{}).Dial("ws://"+proxy.Status().CaptureAddress+"/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := []byte(`{"model":"gpt-5.6-luna"}`)
	if err := client.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatal(err)
	}
	kind, echoed, err := client.ReadMessage()
	if err != nil || kind != websocket.TextMessage || string(echoed) != string(payload) {
		t.Fatalf("websocket echo = %d %q %v", kind, echoed, err)
	}
	observations := waitForObservations(t, proxy, 2)
	if len(observations) < 2 || observations[0].Transport != TransportWebSocket || observations[1].Transport != TransportWebSocket {
		t.Fatalf("websocket observations = %+v", observations)
	}
}

func TestCapturePauseKeepsRelayButStopsObservations(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append([]byte(`{"echo":`), append(body, '}')...))
	}))
	defer upstream.Close()
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, HTTPClient: upstream.Client(), QueueSize: 16})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	address := proxy.Status().CaptureAddress

	request := func(body string) *http.Response {
		t.Helper()
		response, err := http.Post("http://"+address+"/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := request(`{"model":"before-pause"}`)
	_, _ = io.ReadAll(first.Body)
	_ = first.Body.Close()
	waitForObservations(t, proxy, 2)

	if err := proxy.Pause(); err != nil {
		t.Fatal(err)
	}
	if status := proxy.Status(); status.State != "passthrough" {
		t.Fatalf("pause state = %q, want passthrough", status.State)
	}
	if err := proxy.Pause(); err != nil {
		t.Fatalf("second pause = %v", err)
	}
	if err := proxy.Start(); err == nil || !strings.Contains(err.Error(), "relay is still active") {
		t.Fatalf("start during passthrough error = %v", err)
	}

	second := request(`{"model":"after-pause"}`)
	responseBody, _ := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if second.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "after-pause") {
		t.Fatalf("passthrough response = %d %q", second.StatusCode, responseBody)
	}
	time.Sleep(100 * time.Millisecond)
	observations, _ := proxy.Observations(0)
	if len(observations) != 2 {
		t.Fatalf("observations after pause = %d, want 2", len(observations))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if status := proxy.Status(); status.State != "stopped" {
		t.Fatalf("final stop state = %q, want stopped", status.State)
	}
	if _, err := http.Get("http://" + address + "/responses"); err == nil {
		t.Fatal("request succeeded after final capture stop")
	}
}

func TestCapturePauseWithoutConnectionsTransitionsToPassthrough(t *testing.T) {
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", QueueSize: 4})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}

	if err := proxy.Pause(); err != nil {
		t.Fatalf("pause without connections = %v", err)
	}
	if status := proxy.Status(); status.State != "passthrough" {
		t.Fatalf("pause state = %q, want passthrough", status.State)
	}
	if status := proxy.Status(); status.InstanceID != "" {
		t.Fatalf("unexpected instance id = %q", status.InstanceID)
	}
}

func TestCapturePauseTimeoutRestoresObservationGateAndState(t *testing.T) {
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", QueueSize: 4})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	proxy.setPauseBarrierDelay(6 * time.Second)

	started := time.Now()
	err := proxy.Pause()
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("pause error = %v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed < 4*time.Second || elapsed > 6*time.Second {
		t.Fatalf("pause elapsed = %s, want approximately 5 seconds", elapsed)
	}
	if status := proxy.Status(); status.State != "capturing" {
		t.Fatalf("timeout state = %q, want capturing", status.State)
	}

	proxy.setPauseBarrierDelay(0)
}

func TestCapturePauseWhileDrainingSharesCompletion(t *testing.T) {
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", QueueSize: 4})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	proxy.setPauseBarrierDelay(200 * time.Millisecond)

	results := make(chan error, 2)
	go func() { results <- proxy.Pause() }()
	time.Sleep(20 * time.Millisecond)
	go func() { results <- proxy.Pause() }()
	first := <-results
	second := <-results
	if first != nil || second != nil {
		t.Fatalf("single-flight pause results = %v, %v", first, second)
	}
	if status := proxy.Status(); status.State != "passthrough" {
		t.Fatalf("single-flight state = %q, want passthrough", status.State)
	}
}

func waitForObservations(t *testing.T, proxy *CaptureProxy, count int) []Observation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		observations, _ := proxy.Observations(0)
		if len(observations) >= count {
			return observations
		}
		time.Sleep(10 * time.Millisecond)
	}
	observations, _ := proxy.Observations(0)
	t.Fatalf("timed out waiting for %d observations; got %d", count, len(observations))
	return nil
}

type chunkReader struct {
	mu     sync.Mutex
	chunks []string
}

func (r *chunkReader) Read(dst []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	copy(dst, chunk)
	return len(chunk), nil
}
