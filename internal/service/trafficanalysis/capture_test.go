package trafficanalysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestCaptureStampsSingleRelayMarkerAndRejectsClientSpoof(t *testing.T) {
	var markerValues []string
	var correlationValues []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markerValues = r.Header.Values(RelayMarkerHeader)
		correlationValues = r.Header.Values(RequestCorrelationHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	// Capture owns gateway-1: the upstream must see exactly one Capture-generated
	// marker value, never the client-spoofed one.
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, InstanceID: "gateway-1", HTTPClient: upstream.Client(), QueueSize: 16})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	address := proxy.Status().CaptureAddress
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/responses", strings.NewReader(`{"input":"ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(RelayMarkerHeader, "client-spoofed-value")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if len(markerValues) != 1 || markerValues[0] != "gateway-1" {
		t.Fatalf("upstream marker = %v, want [gateway-1]", markerValues)
	}
	if len(correlationValues) != 1 || !strings.HasPrefix(correlationValues[0], "capture-request-") {
		t.Fatalf("upstream request correlation = %v, want one Capture-generated local key", correlationValues)
	}

	// Without an InstanceID the Capture strips any client marker and adds none.
	markerValues = nil
	proxy2 := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, HTTPClient: upstream.Client(), QueueSize: 16})
	defer proxy2.Close()
	if err := proxy2.Start(); err != nil {
		t.Fatal(err)
	}
	request2, err := http.NewRequest(http.MethodPost, "http://"+proxy2.Status().CaptureAddress+"/responses", strings.NewReader(`{"input":"ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	request2.Header.Set(RelayMarkerHeader, "client-spoofed-value")
	response2, err := http.DefaultClient.Do(request2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response2.Body)
	_ = response2.Body.Close()
	if response2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response2.StatusCode)
	}
	if len(markerValues) != 0 {
		t.Fatalf("upstream marker without InstanceID = %v, want none", markerValues)
	}
	if len(correlationValues) != 1 || !strings.HasPrefix(correlationValues[0], "capture-request-") {
		t.Fatalf("upstream request correlation without InstanceID = %v, want one Capture-generated local key", correlationValues)
	}
}

func TestCapturePayloadAndGatewayCorrelationUsesOneRequestAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, QueueSize: 16})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+proxy.Status().CaptureAddress+"/responses", strings.NewReader(`{"model":"gpt-5.6-luna","input":"smoke"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	items := waitForObservations(t, proxy, 2)
	var requestAlias string
	for _, item := range items {
		if item.Direction == DirectionClientToUpstream {
			requestAlias = item.RequestID
		}
	}
	if requestAlias == "" || !strings.HasPrefix(requestAlias, "req#") {
		t.Fatalf("capture request alias = %q, want req#N", requestAlias)
	}
	for _, item := range items {
		if item.RequestID != requestAlias {
			t.Fatalf("observation aliases = %q and %q, want one request alias", requestAlias, item.RequestID)
		}
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

// Tests 1, 2, 9, 10: POST /responses and /v1/responses are accepted over an
// http loopback upstream, forwarded verbatim, and the client-supplied Host
// header never changes the forwarding target (fixed to the configured base).
func TestCaptureHTTPForwardsResponsesOverHTTPLoopback(t *testing.T) {
	var sawPath string
	var sawHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, HTTPClient: upstream.Client(), QueueSize: 16})
	defer proxy.Close()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	address := proxy.Status().CaptureAddress

	for _, path := range []string{"/responses", "/v1/responses"} {
		request, err := http.NewRequest(http.MethodPost, "http://"+address+path, strings.NewReader(`{"input":"ok"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Host = "evil.example.com"
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || string(responseBody) != `{"ok":true}` {
			t.Fatalf("%s response = %d %q", path, response.StatusCode, responseBody)
		}
		if sawPath != path {
			t.Fatalf("upstream path = %q, want %q", sawPath, path)
		}
		wantHost := upstream.URL[len("http://"):]
		if sawHost != wantHost {
			t.Fatalf("upstream Host = %q, want the configured base %q", sawHost, wantHost)
		}
	}
}

// Tests 3, 4: GET /models and /v1/models are accepted over an http loopback
// base and the raw query is preserved.
func TestComposeUpstreamURLAcceptsHTTPLoopbackPaths(t *testing.T) {
	base := "http://127.0.0.1:38440"
	for _, target := range []string{"/responses", "/v1/responses", "/models", "/v1/models"} {
		if _, err := composeUpstreamURL(base, httptest.NewRequest(http.MethodPost, target, nil)); err != nil {
			t.Fatalf("%s rejected over http loopback: %v", target, err)
		}
	}
	query := httptest.NewRequest(http.MethodPost, "/v1/responses?stream=true", nil)
	upstream, err := composeUpstreamURL(base, query)
	if err != nil {
		t.Fatalf("query target rejected: %v", err)
	}
	if upstream.RawQuery != "stream=true" {
		t.Fatalf("raw query = %q, want stream=true", upstream.RawQuery)
	}
}

// Tests 5, 6, 7: dot-segment traversal, percent-encoded traversal, absolute-form
// and authority-form request targets are all rejected even over an http loopback
// base.
func TestComposeUpstreamURLRejectsUnsafeTargetsOverHTTPLoopback(t *testing.T) {
	base := "http://127.0.0.1:38440"
	for name, target := range map[string]string{
		"traversal":         "/v1/../secret",
		"encoded traversal": "/v1/%2e%2e/secret",
		"absolute form":     "http://127.0.0.1:38440/v1/responses",
	} {
		if _, err := composeUpstreamURL(base, httptest.NewRequest(http.MethodGet, target, nil)); err == nil {
			t.Fatalf("%s accepted: %q", name, target)
		}
	}
	authority := httptest.NewRequest(http.MethodGet, "/responses", nil)
	authority.URL = &url.URL{Host: "127.0.0.1:38440", Path: "/responses"}
	if _, err := composeUpstreamURL(base, authority); err == nil {
		t.Fatal("authority-form target accepted")
	}
}

// bodyForwardingFixture returns a running loopback capture proxy whose upstream
// records the exact request body bytes it received, so tests can assert the
// byte-for-byte forwarding contract.
func bodyForwardingFixture(t *testing.T) (*CaptureProxy, *[]byte) {
	t.Helper()
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got, _ := io.ReadAll(r.Body)
			received = append(received, got...)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, HTTPClient: upstream.Client(), QueueSize: 16})
	t.Cleanup(func() { _ = proxy.Close() })
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	return proxy, &received
}

func findClientObservation(t *testing.T, proxy *CaptureProxy) Observation {
	t.Helper()
	obs := waitForObservations(t, proxy, 1)
	for _, o := range obs {
		if o.Direction == DirectionClientToUpstream {
			return o
		}
	}
	t.Fatal("no client-to-upstream observation recorded")
	return Observation{}
}

// L1: a POST body with ContentLength == -1 (unknown, framed as chunked by Go)
// must be forwarded to the upstream byte-for-byte, and the observation's raw
// size/HMAC must match the actually-forwarded bytes.
func TestCaptureForwardsChunkedBodyByteForByte(t *testing.T) {
	proxy, received := bodyForwardingFixture(t)
	payload := []byte(`{"model":"gpt-5.6-luna","input":"chunked-request"}`)
	request, err := http.NewRequest(http.MethodPost, "http://"+proxy.Status().CaptureAddress+"/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = -1 // force unknown length → chunked framing
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(responseBody) != `{}` {
		t.Fatalf("response = %d %q", response.StatusCode, responseBody)
	}
	if !bytes.Equal(*received, payload) {
		t.Fatalf("upstream received %q, want byte-for-byte %q", *received, payload)
	}
	obs := findClientObservation(t, proxy)
	if obs.RawPayloadSize != len(payload) {
		t.Fatalf("observation rawPayloadSize = %d, want %d", obs.RawPayloadSize, len(payload))
	}
	proxy.mu.Lock()
	analyzer := proxy.analyzer
	proxy.mu.Unlock()
	if analyzer == nil {
		t.Fatal("analyzer is nil")
	}
	if want := hmacHex(analyzer.key, payload); obs.RawPayloadHMAC != want {
		t.Fatalf("observation rawPayloadHmac = %q, want the forwarded bytes' HMAC %q", obs.RawPayloadHMAC, want)
	}
}

// L2: a body whose first byte is `(` (not JSON) must still be forwarded
// byte-for-byte. This is the back-to-back guarantee that a `(` observed at the
// Gateway is not introduced by the Capture Proxy.
func TestCaptureForwardsLeadingParenBodyByteForByte(t *testing.T) {
	proxy, received := bodyForwardingFixture(t)
	payload := []byte(`(not-json)`)
	request, err := http.NewRequest(http.MethodPost, "http://"+proxy.Status().CaptureAddress+"/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !bytes.Equal(*received, payload) {
		t.Fatalf("leading-paren body forwarded %q, want %q", *received, payload)
	}
	if got := bodyFirstByteClass(payload); got != "other" {
		t.Fatalf("bodyFirstByteClass(%q) = %q, want other", payload, got)
	}
}

// L3: a clean JSON object body is forwarded byte-for-byte (regression) and is
// classified as an object.
func TestCaptureForwardsCleanJSONByteForByte(t *testing.T) {
	proxy, received := bodyForwardingFixture(t)
	payload := []byte(`{"model":"gpt-5.6-luna","input":"ok"}`)
	request, err := http.NewRequest(http.MethodPost, "http://"+proxy.Status().CaptureAddress+"/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !bytes.Equal(*received, payload) {
		t.Fatalf("clean JSON body forwarded %q, want %q", *received, payload)
	}
	if got := bodyFirstByteClass(payload); got != "object" {
		t.Fatalf("bodyFirstByteClass(%q) = %q, want object", payload, got)
	}
}

// L4: a Codex-compatible normal Responses request shape (POST /responses,
// application/json, object body with a model field) is forwarded byte-for-byte
// and the observation digest matches. This anchors the claim that a leading `(`
// seen at the Gateway is not attributable to a normal JSON request body.
func TestCaptureForwardsCodexCompatibleRequestByteForByte(t *testing.T) {
	proxy, received := bodyForwardingFixture(t)
	payload := []byte(`{"model":"gpt-5.6-luna","input":"Return exactly: OK","stream":true}`)
	request, err := http.NewRequest(http.MethodPost, "http://"+proxy.Status().CaptureAddress+"/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !bytes.Equal(*received, payload) {
		t.Fatalf("Codex-compatible body forwarded %q, want %q", *received, payload)
	}
	obs := findClientObservation(t, proxy)
	if obs.RawPayloadSize != len(payload) {
		t.Fatalf("observation rawPayloadSize = %d, want %d", obs.RawPayloadSize, len(payload))
	}
	proxy.mu.Lock()
	analyzer := proxy.analyzer
	proxy.mu.Unlock()
	if analyzer == nil {
		t.Fatal("analyzer is nil")
	}
	if want := hmacHex(analyzer.key, payload); obs.RawPayloadHMAC != want {
		t.Fatalf("Codex-compatible observation rawPayloadHmac = %q, want the forwarded bytes' HMAC %q", obs.RawPayloadHMAC, want)
	}
	if got := bodyFirstByteClass(payload); got != "object" {
		t.Fatalf("bodyFirstByteClass(%q) = %q, want object", payload, got)
	}
}

// L5: bodyFirstByteClass classifies only the first significant byte.
func TestBodyFirstByteClass(t *testing.T) {
	cases := map[string]string{
		`{`:       "object",
		` {`:      "object",
		"\n{":     "object",
		`[`:       "array",
		`(`:       "other",
		`(:`:      "other",
		`":":`:    "other",
		``:        "empty",
		"   \r\n": "empty",
	}
	for input, want := range cases {
		if got := bodyFirstByteClass([]byte(input)); got != want {
			t.Fatalf("bodyFirstByteClass(%q) = %q, want %q", input, got, want)
		}
	}
}

// Test 8: an http upstream base that is not a loopback host is rejected
// (never forwarded to), while loopback http bases and the fixed https default
// remain accepted.
func TestComposeUpstreamURLRejectsExternalHTTPUpstream(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/responses", nil)
	for _, base := range []string{
		"http://example.com:38440",
		"http://192.168.1.1:38440",
		"http://10.0.0.1:38440",
		"http://0.0.0.0:38440",
		"http://169.254.1.1:38440",
	} {
		if _, err := composeUpstreamURL(base, request); err == nil {
			t.Fatalf("external http upstream accepted: %q", base)
		}
	}
	if _, err := composeUpstreamURL("ftp://127.0.0.1:21", request); err == nil {
		t.Fatal("unsupported scheme accepted")
	}
	for _, base := range []string{"http://127.0.0.1:38440", "http://localhost:38440", "http://[::1]:38440"} {
		if _, err := composeUpstreamURL(base, request); err != nil {
			t.Fatalf("loopback http upstream rejected: %q: %v", base, err)
		}
	}
}

// Test 11: an http upstream is dialed as ws:// (not wss) — proves the scheme is
// derived from the upstream base rather than hardcoded.
func TestCaptureWebSocketRoundTripOverHTTPUpstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	proxy := NewCaptureProxy(CaptureConfig{ListenAddr: "127.0.0.1:0", UpstreamBase: upstream.URL, QueueSize: 16})
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
}

// Test 12: the loopback determination contract, including the full 127.0.0.0/8
// range and IPv4-mapped loopback via net.IP.IsLoopback.
func TestIsLoopbackHost(t *testing.T) {
	allowed := []string{"localhost", "127.0.0.1", "127.0.0.2", "127.8.8.8", "::1", "::ffff:127.0.0.1"}
	for _, host := range allowed {
		if !isLoopbackHost(host) {
			t.Fatalf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	rejected := []string{"example.com", "0.0.0.0", "192.168.1.1", "10.0.0.1", "169.254.1.1", ""}
	for _, host := range rejected {
		if isLoopbackHost(host) {
			t.Fatalf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

// Test 13: sentinel errors map to safe enum strings; anything else (including
// nil) maps to unknown so raw error text is never logged.
func TestCaptureTargetReason(t *testing.T) {
	cases := map[error]string{
		errAbsoluteRequestTarget:       "absolute_request_target",
		errUnsafeRequestPath:           "unsafe_request_path",
		errInvalidEscapedPath:          "invalid_escaped_path",
		errTraversalPath:               "traversal_path",
		errInvalidUpstreamBase:         "invalid_upstream_base",
		errHTTPUpstreamOutsideLoopback: "http_upstream_outside_loopback",
		errUnsupportedUpstreamScheme:   "unsupported_upstream_scheme",
	}
	for sentinel, want := range cases {
		if got := captureTargetReason(sentinel); got != want {
			t.Fatalf("captureTargetReason(%v) = %q, want %q", sentinel, got, want)
		}
	}
	if got := captureTargetReason(errors.New("boom")); got != "unknown" {
		t.Fatalf("captureTargetReason(unknown) = %q, want unknown", got)
	}
	if got := captureTargetReason(nil); got != "unknown" {
		t.Fatalf("captureTargetReason(nil) = %q, want unknown", got)
	}
}

// Test 14: safeCapturePath echoes only known diagnostic paths and folds the
// rest to <other> — log sanitization only, not a forwarding allowlist.
func TestSafeCapturePath(t *testing.T) {
	for _, path := range []string{"/responses", "/v1/responses", "/models", "/v1/models", "/socket"} {
		if got := safeCapturePath(path); got != path {
			t.Fatalf("safeCapturePath(%q) = %q, want verbatim", path, got)
		}
	}
	for _, path := range []string{"/", "/unknown/codex/path", "/v1/responses/../x", "/%2e%2e/secret"} {
		if got := safeCapturePath(path); got != "<other>" {
			t.Fatalf("safeCapturePath(%q) = %q, want <other>", path, got)
		}
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
	if err := proxy.Start(); err == nil || captureStartStage(err) != "relay_active" {
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
