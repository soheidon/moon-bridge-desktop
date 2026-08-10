package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/trace"
	"moonbridge/internal/service/trafficanalysis"
)

// TestDispatchRelayMarkerFirstRequestBindsAndForwardsWithoutMarker wires a real
// trafficanalysis.Service as the TrafficRouting. It verifies the origin-proof
// contract end to end over zstd POST /responses:
//  1. the first marker-carrying request lazily binds its model and resolves in
//     the same request (no 404-then-success);
//  2. the same model without a marker still exact-matches once bound;
//  3. a different model is rejected with 404 and never rebounds;
//  4. the marker never reaches the upstream or the trace record.
func TestDispatchRelayMarkerFirstRequestBindsAndForwardsWithoutMarker(t *testing.T) {
	trafficSvc := trafficanalysis.NewService()
	trafficSvc.BindGatewayRun("gateway-1", "127.0.0.1:38440")
	st, err := trafficSvc.StartCapture(trafficanalysis.StartOptions{
		ListenAddr:   "127.0.0.1:0",
		UpstreamBase: "https://chatgpt.com/backend-api/codex",
	})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	defer trafficSvc.CloseCapture(context.Background())
	if _, err := trafficSvc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1"); err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	if err := trafficSvc.SetDesktopModelMappingExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1", "codex-observed", "moonbridge"); err != nil {
		t.Fatalf("SetDesktopModelMappingExpected() error = %v", err)
	}

	var upstreamMarkers []string
	httpClient := &http.Client{Transport: routingRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamMarkers = append(upstreamMarkers, req.Header.Get(RelayMarkerHeader))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp-relay","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
		}, nil
	})}

	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
		"openai": {
			BaseURL:    "https://openai.example.test",
			APIKey:     "test-key",
			Protocol:   config.ProtocolOpenAIResponse,
			ModelNames: []string{"mapped-upstream"},
		},
	}, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "mapped-upstream"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}

	traceRoot := t.TempDir()
	handler := New(Config{
		ProviderMgr:      pm,
		TrafficRouting:   trafficSvc,
		OpenAIHTTPClient: httpClient,
		Tracer:           trace.New(trace.Config{Enabled: true, Root: traceRoot, SessionID: "session-relay"}),
	})

	do := func(model, marker string) int {
		req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(zstdEncode(t, []byte(`{"model":"`+model+`","input":"hello"}`)))))
		req.Header.Set("Content-Encoding", "zstd")
		if marker != "" {
			req.Header.Set(RelayMarkerHeader, marker)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}

	// First marker-carrying request binds and resolves in the same call.
	if code := do("codex-observed", "gateway-1"); code != http.StatusOK {
		t.Fatalf("first marker request status = %d, want 200", code)
	}
	// Bound exact match works without a marker.
	if code := do("codex-observed", ""); code != http.StatusOK {
		t.Fatalf("bound no-marker request status = %d, want 200", code)
	}
	// A different model fails closed and never rebounds.
	if code := do("other-model", "gateway-1"); code != http.StatusNotFound {
		t.Fatalf("different-model status = %d, want 404", code)
	}

	// The marker never reaches the upstream.
	if len(upstreamMarkers) != 2 {
		t.Fatalf("upstream marker count = %d, want 2 (one per 200 request)", len(upstreamMarkers))
	}
	for i, m := range upstreamMarkers {
		if m != "" {
			t.Fatalf("upstream request %d carried marker %q", i, m)
		}
	}

	// The marker never appears in any written trace.
	var traceFiles []string
	_ = filepath.Walk(traceRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			traceFiles = append(traceFiles, path)
		}
		return nil
	})
	for _, f := range traceFiles {
		content, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", f, err)
		}
		if strings.Contains(string(content), RelayMarkerHeader) || strings.Contains(string(content), "gateway-1") {
			t.Fatalf("trace %s leaked relay marker: %s", f, content)
		}
	}
}

// TestDispatchRelayMarkerWithoutProofFailsClosedOnPendingMapping verifies that a
// request arriving before any bind cannot bind without the relay marker: the
// resolver falls back, the Service refuses to bind, and dispatch returns 404.
func TestDispatchRelayMarkerWithoutProofFailsClosedOnPendingMapping(t *testing.T) {
	trafficSvc := trafficanalysis.NewService()
	trafficSvc.BindGatewayRun("gateway-1", "127.0.0.1:38440")
	st, err := trafficSvc.StartCapture(trafficanalysis.StartOptions{
		ListenAddr:   "127.0.0.1:0",
		UpstreamBase: "https://chatgpt.com/backend-api/codex",
	})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	defer trafficSvc.CloseCapture(context.Background())
	if _, err := trafficSvc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1"); err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	if err := trafficSvc.SetDesktopModelMappingExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1", "codex-observed", "moonbridge"); err != nil {
		t.Fatalf("SetDesktopModelMappingExpected() error = %v", err)
	}

	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
		"openai": {
			BaseURL:    "https://openai.example.test",
			APIKey:     "test-key",
			Protocol:   config.ProtocolOpenAIResponse,
			ModelNames: []string{"mapped-upstream"},
		},
	}, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "mapped-upstream"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	handler := New(Config{ProviderMgr: pm, TrafficRouting: trafficSvc})

	req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(zstdEncode(t, []byte(`{"model":"codex-observed","input":"hello"}`)))))
	req.Header.Set("Content-Encoding", "zstd")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no relay proof on pending mapping)", recorder.Code)
	}

	// The pending mapping was never bound by the unproven request.
	if _, ok := trafficSvc.ModelMappingFor("codex-observed"); ok {
		t.Fatal("unproven request bound the mapping")
	}
}
