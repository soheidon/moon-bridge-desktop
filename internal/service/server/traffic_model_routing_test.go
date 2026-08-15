package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"moonbridge/internal/config"
	deepseekv4 "moonbridge/internal/extension/deepseek_v4"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/format"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/trafficanalysis"
)

type exactTrafficMapping struct {
	target string
	ok     bool
	source []string
	marker []string
}

type routingRoundTripFunc func(*http.Request) (*http.Response, error)

type routingEventSink struct {
	events []trafficanalysis.GatewayEventInput
}

func (s *routingEventSink) RecordGatewayEvent(event trafficanalysis.GatewayEventInput) {
	s.events = append(s.events, event)
}

func (fn routingRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func (m *exactTrafficMapping) ObservedModelFor(sourceModel, relayMarker string) (string, bool) {
	m.source = append(m.source, sourceModel)
	m.marker = append(m.marker, relayMarker)
	return m.target, m.ok
}

func newRoutingTestManager(t *testing.T, routes map[string]provider.ModelRoute, models []string) *provider.ProviderManager {
	t.Helper()
	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
		"openai": {
			BaseURL:    "https://openai.example.test",
			APIKey:     "test-key",
			Protocol:   config.ProtocolOpenAIResponse,
			ModelNames: models,
		},
	}, routes)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	return pm
}

func TestResolveModelOrFallbackUsesExactMappingOnlyAfterNotFound(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"source-route": {Provider: "openai", Name: "source-upstream"},
		"moonbridge":   {Provider: "openai", Name: "mapped-upstream"},
	}, []string{"catalog-model"})

	tests := []struct {
		name       string
		requested  string
		mapping    *exactTrafficMapping
		wantAlias  string
		wantSource string
		wantErr    bool
	}{
		{name: "explicit route wins", requested: "source-route", mapping: &exactTrafficMapping{target: "moonbridge", ok: true}, wantSource: "source-upstream"},
		{name: "catalog model wins", requested: "catalog-model", mapping: &exactTrafficMapping{target: "moonbridge", ok: true}, wantSource: "catalog-model"},
		{name: "exact temporary mapping", requested: "codex-future-model", mapping: &exactTrafficMapping{target: "moonbridge", ok: true}, wantAlias: "moonbridge", wantSource: "mapped-upstream"},
		{name: "source mismatch remains not found", requested: "codex-future-model", mapping: &exactTrafficMapping{target: "moonbridge", ok: false}, wantErr: true},
		{name: "inactive relay remains not found", requested: "codex-future-model", mapping: &exactTrafficMapping{target: "", ok: false}, wantErr: true},
		{name: "broken target fails closed", requested: "codex-future-model", mapping: &exactTrafficMapping{target: "missing-target", ok: true}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := New(Config{ProviderMgr: pm, TrafficRouting: tt.mapping})
			resolved, alias, err := svc.resolveModelOrFallback(tt.requested, "gateway-relay")
			if tt.wantErr {
				if err == nil {
					t.Fatal("resolveModelOrFallback() error = nil, want fail-closed error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveModelOrFallback() error = %v", err)
			}
			preferred, ok := resolved.Preferred()
			if !ok || preferred.UpstreamModel != tt.wantSource {
				t.Fatalf("resolved preferred = %#v/%v, want upstream %q", preferred, ok, tt.wantSource)
			}
			if alias != tt.wantAlias {
				t.Fatalf("routing alias = %q, want %q", alias, tt.wantAlias)
			}
			if tt.wantAlias == "" && len(tt.mapping.source) != 0 {
				t.Fatalf("mapping lookup calls = %v, want none for primary resolution", tt.mapping.source)
			}
		})
	}
}

func TestResolveModelOrFallbackDoesNotUseMappingForBrokenPrimaryRoute(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"broken-route": {Provider: "missing-provider", Name: "source-upstream"},
		"moonbridge":   {Provider: "openai", Name: "mapped-upstream"},
	}, nil)
	mapping := &exactTrafficMapping{target: "moonbridge", ok: true}
	_, alias, err := New(Config{ProviderMgr: pm, TrafficRouting: mapping}).resolveModelOrFallback("broken-route", "gateway-relay")
	if err == nil || alias != "" {
		t.Fatalf("broken primary route = alias %q err %v, want no fallback", alias, err)
	}
	if len(mapping.source) != 0 {
		t.Fatalf("mapping lookup calls = %v, want none for non-not-found error", mapping.source)
	}
}

// newTestCredentialResolver returns a CredentialResolver that resolves
// "DEEPSEEK_API_KEY" to "test-key" and rejects all other env names.
func newTestCredentialResolver() *provider.CredentialResolver {
	return &provider.CredentialResolver{
		LookupEnv: func(name string) (string, bool) {
			if name == "DEEPSEEK_API_KEY" {
				return "test-key", true
			}
			return "", false
		},
		Registry: provider.NewCredentialStatusRegistry(),
	}
}

// newRoutingTestAdapterRegistry creates a format.Registry with OpenAI client
// and Anthropic provider adapters for routing tests.
func newRoutingTestAdapterRegistry(t *testing.T) *format.Registry {
	t.Helper()
	cfg := config.Config{ProviderDefs: map[string]config.ProviderDef{
		"deepseek": {Protocol: config.ProtocolAnthropic, Models: map[string]config.ModelMeta{
			"deepseek-v4-flash": {Extensions: map[string]config.ExtensionSettings{
				deepseekv4.PluginName: {Enabled: boolPtr(true)},
			}},
		}},
	}}
	plugins := plugin.NewRegistry(slog.Default())
	plugins.Register(deepseekv4.NewPlugin(func(model string) bool { return model == "deepseek-v4-flash" }))
	if err := plugins.InitAll(&cfg); err != nil {
		t.Fatalf("InitAll() error = %v", err)
	}
	hooks := plugins.CorePluginHooks()
	adapterReg := format.NewRegistry()
	clientAdapter := openai.NewOpenAIAdapter(hooks)
	if err := adapterReg.RegisterClient(clientAdapter); err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if err := adapterReg.RegisterClientStream(clientAdapter); err != nil {
		t.Fatalf("RegisterClientStream() error = %v", err)
	}
	providerAdapter := anthropic.NewAnthropicProviderAdapter(128, serverNoopCacheManager{}, hooks)
	if err := adapterReg.RegisterProvider(providerAdapter); err != nil {
		t.Fatalf("RegisterProvider() error = %v", err)
	}
	if err := adapterReg.RegisterProviderStream(providerAdapter); err != nil {
		t.Fatalf("RegisterProviderStream() error = %v", err)
	}
	return adapterReg
}

func boolPtr(v bool) *bool { return &v }

func TestLunaPayloadIntegrationFromHTTPEntry(t *testing.T) {
	// Exercises the OpenAI Responses HTTP entrypoint with reasoning: {"effort":"medium"}
	// so inbound reasoning enters the conversion path. Routing profile slots then
	// override (Sol/Terra) or clear (Luna) the reasoning.
	cases := []struct {
		name       string
		model      string
		wantEffort string
	}{
		{name: "sol", model: "gpt-5.6-sol", wantEffort: "max"},
		{name: "terra", model: "gpt-5.6-terra", wantEffort: "high"},
		{name: "luna", model: "gpt-5.6-luna", wantEffort: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var payload anthropic.MessageRequest
			transport := routingRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"msg-luna-test","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)),
				}, nil
			})

			resolver := &stubSlotResolver{slots: map[string]RoutingProfileSlotResult{
				"gpt-5.6-sol":   {ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: "thinking", Reasoning: ptrString("max")},
				"gpt-5.6-terra": {ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: "thinking", Reasoning: ptrString("high")},
				"gpt-5.6-luna":  {ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: "normal", Reasoning: nil},
			}}
			pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
				"deepseek": {
					BaseURL:        "https://deepseek.example.test",
					APIKeyEnv:      "DEEPSEEK_API_KEY",
					ClientOverride: &http.Client{Transport: transport},
				},
			}, nil, newTestCredentialResolver())
			if err != nil {
				t.Fatalf("NewProviderManager() error = %v", err)
			}

			handler := New(Config{
				AdapterRegistry:        newRoutingTestAdapterRegistry(t),
				ProviderMgr:            pm,
				RoutingProfileResolver: resolver,
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"reasoning":{"effort":"medium"},"input":"hello"}`, tc.model)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
			if payload.Model != "deepseek-v4-flash" {
				t.Fatalf("upstream model = %q, want deepseek-v4-flash", payload.Model)
			}
			if tc.wantEffort != "" {
				if payload.OutputConfig == nil || payload.OutputConfig.Effort != tc.wantEffort {
					t.Fatalf("output_config = %+v, want effort %q", payload.OutputConfig, tc.wantEffort)
				}
			} else if payload.OutputConfig != nil {
				t.Fatalf("Luna output_config = %+v, want field absent", payload.OutputConfig)
			}
			if tc.wantEffort == "" {
				if payload.Thinking == nil || payload.Thinking.Type != "disabled" {
					t.Fatalf("Luna thinking = %+v, want disabled", payload.Thinking)
				}
			} else if payload.Thinking == nil || payload.Thinking.Type != "enabled" {
				t.Fatalf("thinking = %+v, want enabled", payload.Thinking)
			}
		})
	}
}

// TestLunaPayloadControlNoOverride is a negative control: input reasoning
// {"effort":"medium"} passes through the Anthropic adapter path WITHOUT any
// routing profile override. Since OpenAI ToCoreRequest stores reasoning only
// in Extensions (not in coreReq.Output/Thinking), the effort should NOT reach
// the Anthropic upstream. This confirms that Luna's thinking=nil in the main
// test is due to mode=normal clearing, not due to the reasoning never entering
// the conversion path.
func TestLunaPayloadControlNoOverride(t *testing.T) {
	var calls int
	var payload anthropic.MessageRequest
	transport := routingRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg-control","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)),
		}, nil
	})

	// No routing profile resolver — input reasoning passes through unchanged.
	// Use a route alias so the model can be resolved by the ProviderManager.
	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
		"deepseek": {
			BaseURL:        "https://deepseek.example.test",
			APIKeyEnv:      "DEEPSEEK_API_KEY",
			ClientOverride: &http.Client{Transport: transport},
		},
	}, map[string]provider.ModelRoute{
		"deepseek-v4-flash": {Provider: "deepseek", Name: "deepseek-v4-flash"},
	}, newTestCredentialResolver())
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}

	handler := New(Config{
		AdapterRegistry: newRoutingTestAdapterRegistry(t),
		ProviderMgr:     pm,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-flash","reasoning":{"effort":"medium"},"input":"hello"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	// Control: without routing profile, the production DeepSeek plugin applies
	// its documented default enabled thinking, but no invented token budget.
	if payload.OutputConfig != nil {
		t.Fatalf("control: output_config = %+v, want nil (no routing override)", payload.OutputConfig)
	}
	if payload.Thinking == nil || payload.Thinking.Type != "enabled" || payload.Thinking.BudgetTokens != 0 {
		t.Fatalf("control: thinking = %+v, want enabled without budget", payload.Thinking)
	}
}

func TestRoutingObservationEmitsCorrelatedSafeEvents(t *testing.T) {
	var calls int
	transport := routingRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg-observe","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
	})
	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{"deepseek": {BaseURL: "https://deepseek.example.test", APIKeyEnv: "DEEPSEEK_API_KEY", ClientOverride: &http.Client{Transport: transport}}}, nil, newTestCredentialResolver())
	if err != nil {
		t.Fatal(err)
	}
	sink := &routingEventSink{}
	mapping := &exactTrafficMapping{target: "deepseek-v4-flash", ok: true}
	resolver := &stubSlotResolver{slots: map[string]RoutingProfileSlotResult{
		"gpt-5.6-sol":   {SlotID: "sol", ActiveProfileID: "deepseek", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: "thinking", Reasoning: ptrString("max")},
		"gpt-5.6-terra": {SlotID: "terra", ActiveProfileID: "deepseek", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: "thinking", Reasoning: ptrString("high")},
		"gpt-5.6-luna":  {SlotID: "luna", ActiveProfileID: "deepseek", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: "normal"},
	}}
	handler := New(Config{AdapterRegistry: newRoutingTestAdapterRegistry(t), ProviderMgr: pm, TrafficRouting: mapping, RoutingObservationSink: sink, RoutingProfileResolver: resolver})
	models := []struct {
		requested string
		slot      string
		mode      string
	}{
		{requested: "gpt-5.6-sol", slot: "sol", mode: "thinking"},
		{requested: "gpt-5.6-terra", slot: "terra", mode: "thinking"},
		{requested: "gpt-5.6-luna", slot: "luna", mode: "normal"},
		{requested: "gpt-5.6-sol", slot: "sol", mode: "thinking"},
	}
	for _, model := range models {
		response := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":%q,"reasoning":{"effort":"medium"},"input":"hello"}`, model.requested)
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("model=%s status=%d calls=%d body=%s", model.requested, response.Code, calls, response.Body.String())
		}
	}
	if calls != len(models) {
		t.Fatalf("provider calls=%d, want %d", calls, len(models))
	}
	if len(sink.events) != len(models)*6 {
		t.Fatalf("events=%d, want %d events: %#v", len(sink.events), len(models)*6, sink.events)
	}
	if len(mapping.source) != 0 {
		t.Fatalf("traffic fallback was consulted before exact routing slot: %#v", mapping.source)
	}
	seenCorrelations := map[string]bool{}
	for i, model := range models {
		group := sink.events[i*6 : (i+1)*6]
		if group[0].Kind != trafficanalysis.ObservationRoutingResolutionDiagnosed || group[1].Kind != trafficanalysis.ObservationRoutingResolved || group[2].Kind != trafficanalysis.ObservationProviderRequestPrepared || group[3].Kind != trafficanalysis.ObservationProviderRequestDispatched || group[4].Kind != trafficanalysis.ObservationProviderResponseReceived || group[5].Kind != trafficanalysis.ObservationProviderResponseForwarded {
			t.Fatalf("model=%s events=%#v, want routing/prepared/dispatch/received/forwarded sequence", model.requested, group)
		}
		if group[0].Resolver == nil || group[0].Resolver.RequestedModel != model.requested {
			t.Fatalf("model=%s diagnostic=%#v", model.requested, group[0].Resolver)
		}
		correlation := group[1].CorrelationKey
		if correlation == "" || seenCorrelations[correlation] {
			t.Fatalf("model=%s correlation=%q already seen=%v", model.requested, correlation, seenCorrelations)
		}
		seenCorrelations[correlation] = true
		for _, event := range group[1:] {
			if event.CorrelationKey != correlation || event.RequestedModel != model.requested || event.RoutingSlot != model.slot || event.Mode != model.mode || event.Provider != "deepseek" || event.UpstreamModel != "deepseek-v4-flash" {
				t.Fatalf("model=%s metadata drifted: %#v", model.requested, group)
			}
		}
		if group[3].ExchangeIndex != 1 || group[4].ExchangeIndex != 1 || group[5].ExchangeIndex != 1 {
			t.Fatalf("model=%s egress exchange index mismatch: %#v", model.requested, group)
		}
	}
}

func TestReasoningModePropagatesToCandidate(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, []string{"deepseek-v4-flash"})
	resolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol":  {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Mode: "thinking", Reasoning: ptrString("max")},
			"gpt-5.6-luna": {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Mode: "normal", Reasoning: nil},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})

	cases := []struct {
		model    string
		wantMode string
		wantRO   *string
	}{
		{model: "gpt-5.6-sol", wantMode: "thinking", wantRO: ptrString("max")},
		{model: "gpt-5.6-luna", wantMode: "normal", wantRO: nil},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			resolved, _, err := svc.resolveModelOrFallback(tc.model, "")
			if err != nil {
				t.Fatalf("resolveModelOrFallback(%q) error = %v", tc.model, err)
			}
			preferred, ok := resolved.Preferred()
			if !ok {
				t.Fatal("no preferred candidate")
			}
			if preferred.ReasoningMode != tc.wantMode {
				t.Fatalf("ReasoningMode = %q, want %q", preferred.ReasoningMode, tc.wantMode)
			}
			if tc.wantRO == nil && preferred.ReasoningOverride != nil {
				t.Fatalf("ReasoningOverride = %v, want nil", *preferred.ReasoningOverride)
			}
			if tc.wantRO != nil && (preferred.ReasoningOverride == nil || *preferred.ReasoningOverride != *tc.wantRO) {
				t.Fatalf("ReasoningOverride = %v, want %v", preferred.ReasoningOverride, tc.wantRO)
			}
		})
	}
}

type serverNoopCacheManager struct{}

func (serverNoopCacheManager) PlanAndInject(context.Context, *anthropic.MessageRequest, *format.CoreRequest) (string, string) {
	return "", ""
}

func (serverNoopCacheManager) UpdateRegistry(context.Context, string, string, anthropic.Usage) {}

func ptrString(value string) *string { return &value }

func TestResolvedMappingUsesRoutingAliasForOpenAIResponseSettings(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "mapped-upstream"},
	}, nil)
	pm.SetResolvedWebSearch("model:moonbridge", "enabled")
	mapping := &exactTrafficMapping{target: "moonbridge", ok: true}
	var upstreamModel string
	var sawWebSearch bool
	httpClient := &http.Client{Transport: routingRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var upstream openai.ResponsesRequest
		if err := json.Unmarshal(body, &upstream); err != nil {
			return nil, err
		}
		upstreamModel = upstream.Model
		for _, tool := range upstream.Tools {
			if tool.Type == "web_search" {
				sawWebSearch = true
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp-routing","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
		}, nil
	})}
	handler := New(Config{ProviderMgr: pm, TrafficRouting: mapping, OpenAIHTTPClient: httpClient})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"codex-future-model","input":"hello"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if upstreamModel != "mapped-upstream" || !sawWebSearch {
		t.Fatalf("upstream routing = model %q web_search %v", upstreamModel, sawWebSearch)
	}
}

func TestResolveModelOrFallbackLogsSecretFreeResolutionDiagnostic(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "mapped-upstream"},
	}, nil)

	tests := []struct {
		name          string
		mapping       *exactTrafficMapping
		wantHit       bool
		wantAttempted bool
		wantSuccess   bool
	}{
		{name: "mapping miss", mapping: &exactTrafficMapping{target: "", ok: false}, wantHit: false, wantAttempted: false, wantSuccess: false},
		{name: "target resolve fails", mapping: &exactTrafficMapping{target: "missing-target", ok: true}, wantHit: true, wantAttempted: true, wantSuccess: false},
		{name: "target resolve success", mapping: &exactTrafficMapping{target: "moonbridge", ok: true}, wantHit: true, wantAttempted: true, wantSuccess: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := resetLoggerForTest(&buf)
			defer restore()
			svc := New(Config{ProviderMgr: pm, TrafficRouting: tt.mapping})
			_, _, err := svc.resolveModelOrFallback("codex-future-model", "gateway-relay")
			if tt.wantSuccess && err != nil {
				t.Fatalf("resolveModelOrFallback() error = %v, want success", err)
			}
			if !tt.wantSuccess && err == nil {
				t.Fatal("resolveModelOrFallback() error = nil, want fail-closed error")
			}
			entry := findSlogEntry(&buf, "traffic model routing resolve")
			if entry == nil {
				t.Fatalf("resolve diagnostic entry missing; buffer = %s", buf.String())
			}
			assertField(t, entry, "primary_not_found", true)
			assertField(t, entry, "mapping_lookup_hit", tt.wantHit)
			assertField(t, entry, "target_resolve_attempted", tt.wantAttempted)
			assertField(t, entry, "target_resolve_success", tt.wantSuccess)
			raw := buf.String()
			for _, secret := range []string{"codex-future-model", "moonbridge", "missing-target", "mapped-upstream"} {
				if strings.Contains(raw, secret) {
					t.Fatalf("resolve diagnostic leaked %q: %s", secret, raw)
				}
			}
		})
	}
}

func TestResolveModelOrFallbackSkipsDiagnosticWhenNoTrafficRouting(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "mapped-upstream"},
	}, nil)
	var buf bytes.Buffer
	restore := resetLoggerForTest(&buf)
	defer restore()
	svc := New(Config{ProviderMgr: pm})
	_, _, err := svc.resolveModelOrFallback("codex-future-model", "")
	if err == nil {
		t.Fatal("resolveModelOrFallback() error = nil, want fail-closed error")
	}
	if entry := findSlogEntry(&buf, "traffic model routing resolve"); entry != nil {
		t.Fatalf("resolve diagnostic emitted without TrafficRouting: %v", entry)
	}
}

func TestResolveModelOrFallbackForwardsRelayMarkerToMapping(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "mapped-upstream"},
	}, nil)
	mapping := &exactTrafficMapping{target: "moonbridge", ok: true}
	svc := New(Config{ProviderMgr: pm, TrafficRouting: mapping})
	resolved, alias, err := svc.resolveModelOrFallback("codex-future-model", "gateway-1")
	if err != nil {
		t.Fatalf("resolveModelOrFallback() error = %v", err)
	}
	if alias != "moonbridge" || len(mapping.source) != 1 || mapping.source[0] != "codex-future-model" {
		t.Fatalf("mapping source = %v, alias %q", mapping.source, alias)
	}
	if len(mapping.marker) != 1 || mapping.marker[0] != "gateway-1" {
		t.Fatalf("mapping relayMarker = %v, want [gateway-1]", mapping.marker)
	}
	preferred, ok := resolved.Preferred()
	if !ok || preferred.UpstreamModel != "mapped-upstream" {
		t.Fatalf("resolved preferred = %#v/%v", preferred, ok)
	}
}

// --- routing profile slot resolver tests ---

type stubSlotResolver struct {
	slots map[string]RoutingProfileSlotResult
}

func (r *stubSlotResolver) ResolveSlot(requestModel string) (RoutingProfileSlotResult, bool) {
	slot, ok := r.slots[requestModel]
	return slot, ok
}

func TestResolveModelOrFallbackUsesRoutingProfileSlot(t *testing.T) {
	maxReasoning := "max"
	highReasoning := "high"
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, []string{"deepseek-v4-flash"})
	resolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol":   {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Reasoning: &maxReasoning},
			"gpt-5.6-terra": {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Reasoning: &highReasoning},
			"gpt-5.6-luna":  {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Reasoning: nil},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})

	cases := []struct {
		model         string
		wantUpstream  string
		wantReasoning *string
	}{
		{model: "gpt-5.6-sol", wantUpstream: "deepseek-v4-flash", wantReasoning: &maxReasoning},
		{model: "gpt-5.6-terra", wantUpstream: "deepseek-v4-flash", wantReasoning: &highReasoning},
		{model: "gpt-5.6-luna", wantUpstream: "deepseek-v4-flash", wantReasoning: nil},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			resolved, alias, err := svc.resolveModelOrFallback(tc.model, "")
			if err != nil {
				t.Fatalf("resolveModelOrFallback(%q) error = %v", tc.model, err)
			}
			if alias != tc.wantUpstream {
				t.Fatalf("alias = %q, want %q", alias, tc.wantUpstream)
			}
			preferred, ok := resolved.Preferred()
			if !ok {
				t.Fatal("no preferred candidate")
			}
			if preferred.UpstreamModel != tc.wantUpstream {
				t.Fatalf("UpstreamModel = %q, want %q", preferred.UpstreamModel, tc.wantUpstream)
			}
			if tc.wantReasoning == nil && preferred.ReasoningOverride != nil {
				t.Fatalf("ReasoningOverride = %v, want nil", *preferred.ReasoningOverride)
			}
			if tc.wantReasoning != nil && (preferred.ReasoningOverride == nil || *preferred.ReasoningOverride != *tc.wantReasoning) {
				t.Fatalf("ReasoningOverride = %v, want %v", preferred.ReasoningOverride, tc.wantReasoning)
			}
		})
	}
}

func TestResolveModelOrFallbackRoutingProfileFailClosed(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, nil)
	resolver := &stubSlotResolver{slots: map[string]RoutingProfileSlotResult{}}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})

	_, _, err := svc.resolveModelOrFallback("gpt-5.6-unknown", "")
	if err == nil {
		t.Fatal("resolveModelOrFallback for unknown model should return error")
	}
}

func TestResolveModelOrFallbackRoutingProfileOverridesReasoning(t *testing.T) {
	maxReasoning := "max"
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, []string{"deepseek-v4-flash"})
	resolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol": {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Reasoning: &maxReasoning},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})
	resolved, _, err := svc.resolveModelOrFallback("gpt-5.6-sol", "")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	preferred, ok := resolved.Preferred()
	if !ok || preferred.ReasoningOverride == nil || *preferred.ReasoningOverride != "max" {
		t.Fatalf("ReasoningOverride = %v, want max", preferred.ReasoningOverride)
	}
}

func TestResolveModelOrFallbackRoutingProfileHonorsProviderKey(t *testing.T) {
	// Two providers both have "shared-model", but the slot targets "other".
	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
		"openai": {
			BaseURL:    "https://openai.example.test",
			APIKey:     "test-key",
			Protocol:   config.ProtocolOpenAIResponse,
			ModelNames: []string{"shared-model"},
		},
		"other": {
			BaseURL:    "https://other.example.test",
			APIKey:     "test-key-2",
			Protocol:   config.ProtocolOpenAIResponse,
			ModelNames: []string{"shared-model"},
		},
	}, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	highReasoning := "high"
	resolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-terra": {ProviderKey: "other", UpstreamModel: "shared-model", Reasoning: &highReasoning},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})
	resolved, alias, err := svc.resolveModelOrFallback("gpt-5.6-terra", "")
	if err != nil {
		t.Fatalf("resolveModelOrFallback() error = %v", err)
	}
	if alias != "shared-model" {
		t.Fatalf("alias = %q, want %q", alias, "shared-model")
	}
	preferred, ok := resolved.Preferred()
	if !ok {
		t.Fatal("no preferred candidate")
	}
	// The slot specifies "other" provider, not "openai".
	if preferred.ProviderKey != "other" {
		t.Fatalf("ProviderKey = %q, want %q (slot.ProviderKey should be honored)", preferred.ProviderKey, "other")
	}
	if preferred.UpstreamModel != "shared-model" {
		t.Fatalf("UpstreamModel = %q, want %q", preferred.UpstreamModel, "shared-model")
	}
}

func TestResolveModelOrFallbackRoutingProfileUnknownProviderFailClosed(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, []string{"deepseek-v4-flash"})
	resolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol": {ProviderKey: "nonexistent-provider", UpstreamModel: "deepseek-v4-flash"},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})
	// Unknown provider in the slot → pm.ResolveModel("nonexistent-provider/deepseek-v4-flash")
	// should fail, and the overall resolution should fail closed.
	_, _, err := svc.resolveModelOrFallback("gpt-5.6-sol", "")
	if err == nil {
		t.Fatal("resolveModelOrFallback with unknown provider should return error")
	}
}

func TestSwapRoutingProfileResolver(t *testing.T) {
	maxReasoning := "max"
	highReasoning := "high"
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, []string{"deepseek-v4-flash"})

	oldResolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol": {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Reasoning: &maxReasoning},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: oldResolver})

	// Verify old resolver works.
	resolved, alias, err := svc.resolveModelOrFallback("gpt-5.6-sol", "")
	if err != nil {
		t.Fatalf("old resolver: error = %v", err)
	}
	preferred, _ := resolved.Preferred()
	if preferred.ReasoningOverride == nil || *preferred.ReasoningOverride != "max" {
		t.Fatalf("old resolver: reasoning = %v, want max", preferred.ReasoningOverride)
	}
	_ = alias

	// Swap to a new resolver with different reasoning.
	newResolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol": {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash", Reasoning: &highReasoning},
		},
	}
	svc.SwapRoutingProfileResolver(newResolver)

	// Verify new resolver is active.
	resolved, _, err = svc.resolveModelOrFallback("gpt-5.6-sol", "")
	if err != nil {
		t.Fatalf("new resolver: error = %v", err)
	}
	preferred, _ = resolved.Preferred()
	if preferred.ReasoningOverride == nil || *preferred.ReasoningOverride != "high" {
		t.Fatalf("new resolver: reasoning = %v, want high", preferred.ReasoningOverride)
	}
}

func TestSwapRoutingProfileResolverRemovesSlot(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, []string{"deepseek-v4-flash"})
	resolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol": {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash"},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})

	// Verify works.
	if _, _, err := svc.resolveModelOrFallback("gpt-5.6-sol", ""); err != nil {
		t.Fatalf("before swap: error = %v", err)
	}

	// Swap to a resolver with no slots (simulates profile with no sol slot).
	svc.SwapRoutingProfileResolver(&stubSlotResolver{slots: map[string]RoutingProfileSlotResult{}})

	// gpt-5.6-sol should now fail (slot removed).
	if _, _, err := svc.resolveModelOrFallback("gpt-5.6-sol", ""); err == nil {
		t.Fatal("after swap: expected error for gpt-5.6-sol")
	}
}

func TestSwapRoutingProfileResolverNilClear(t *testing.T) {
	pm := newRoutingTestManager(t, map[string]provider.ModelRoute{
		"moonbridge": {Provider: "openai", Name: "moonbridge-model"},
	}, []string{"deepseek-v4-flash"})
	resolver := &stubSlotResolver{
		slots: map[string]RoutingProfileSlotResult{
			"gpt-5.6-sol": {ProviderKey: "openai", UpstreamModel: "deepseek-v4-flash"},
		},
	}
	svc := New(Config{ProviderMgr: pm, RoutingProfileResolver: resolver})

	if _, _, err := svc.resolveModelOrFallback("gpt-5.6-sol", ""); err != nil {
		t.Fatalf("before clear: error = %v", err)
	}

	// Nil clear — atomic.Pointer allows nil.
	svc.SwapRoutingProfileResolver(nil)

	if _, _, err := svc.resolveModelOrFallback("gpt-5.6-sol", ""); err == nil {
		t.Fatal("after nil clear: expected error for gpt-5.6-sol")
	}
}
