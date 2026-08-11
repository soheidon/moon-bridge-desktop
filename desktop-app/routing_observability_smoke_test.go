package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"moonbridge/internal/config"
	deepseekv4 "moonbridge/internal/extension/deepseek_v4"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/format"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/routingprofile"
	"moonbridge/internal/service/server"
	"moonbridge/internal/service/trafficanalysis"
)

type smokeSlotResolver struct {
	slots map[string]server.RoutingProfileSlotResult
}

func (r *smokeSlotResolver) ResolveSlot(model string) (server.RoutingProfileSlotResult, bool) {
	slot, ok := r.slots[model]
	return slot, ok
}

type smokeCacheManager struct{}

func (smokeCacheManager) PlanAndInject(context.Context, *anthropic.MessageRequest, *format.CoreRequest) (string, string) {
	return "", ""
}
func (smokeCacheManager) UpdateRegistry(context.Context, string, string, anthropic.Usage) {}

func TestRoutingObservabilitySmoke(t *testing.T) {
	var mu sync.Mutex
	var upstreamBodies []map[string]any
	upstream := routingSmokeTransport(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, err
		}
		mu.Lock()
		upstreamBodies = append(upstreamBodies, decoded)
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"smoke-message","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
	})

	cfg := config.Config{ProviderDefs: map[string]config.ProviderDef{
		"deepseek": {Protocol: config.ProtocolAnthropic, Models: map[string]config.ModelMeta{"deepseek-v4-flash": {Extensions: map[string]config.ExtensionSettings{deepseekv4.PluginName: {Enabled: smokeBool(true)}}}, "deepseek-v4-pro": {Extensions: map[string]config.ExtensionSettings{deepseekv4.PluginName: {Enabled: smokeBool(true)}}}}},
	}}
	plugins := plugin.NewRegistry(slog.Default())
	plugins.Register(deepseekv4.NewPlugin(func(model string) bool { return model == "deepseek-v4-flash" || model == "deepseek-v4-pro" }))
	if err := plugins.InitAll(&cfg); err != nil {
		t.Fatal(err)
	}
	hooks := plugins.CorePluginHooks()
	registry := format.NewRegistry()
	clientAdapter := openai.NewOpenAIAdapter(hooks)
	if err := registry.RegisterClient(clientAdapter); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterClientStream(clientAdapter); err != nil {
		t.Fatal(err)
	}
	providerAdapter := anthropic.NewAnthropicProviderAdapter(128, smokeCacheManager{}, hooks)
	if err := registry.RegisterProvider(providerAdapter); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterProviderStream(providerAdapter); err != nil {
		t.Fatal(err)
	}
	pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{"deepseek": {BaseURL: "http://mock.invalid", Protocol: config.ProtocolAnthropic, APIKeyEnv: "SMOKE_ONLY", ClientOverride: &http.Client{Transport: upstream}}}, nil, &provider.CredentialResolver{LookupEnv: func(string) (string, bool) { return "dummy", true }, Registry: provider.NewCredentialStatusRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &smokeSlotResolver{slots: map[string]server.RoutingProfileSlotResult{
		"gpt-5.6-sol":   {SlotID: routingprofile.SlotSol, ActiveProfileID: "deepseek", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: routingprofile.ModeThinking, Reasoning: smokeString("max")},
		"gpt-5.6-terra": {SlotID: routingprofile.SlotTerra, ActiveProfileID: "deepseek", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: routingprofile.ModeThinking, Reasoning: smokeString("high")},
		"gpt-5.6-luna":  {SlotID: routingprofile.SlotLuna, ActiveProfileID: "deepseek", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: routingprofile.ModeNormal},
	}}
	traffic := trafficanalysis.NewService()
	gatewayHandler := server.New(server.Config{AdapterRegistry: registry, ProviderMgr: pm, RoutingProfileResolver: resolver, PluginRegistry: plugins, RoutingObservationSink: traffic})
	gateway := httptest.NewServer(gatewayHandler)
	defer gateway.Close()
	if _, err := traffic.BindGatewayRun("smoke-gateway", gateway.URL); err != nil {
		t.Fatal(err)
	}
	state, err := traffic.StartCapture(trafficanalysis.StartOptions{UpstreamBase: gateway.URL, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer traffic.CloseCapture(context.Background())
	logWriter, err := newTrafficLogWriter(t.TempDir(), traffic.Observations)
	if err != nil {
		t.Fatal(err)
	}
	defer logWriter.Close(false)

	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		smokePost(t, state.ListeningAddress, model)
	}
	waitForSmokeEvents(t, traffic, 6)
	mu.Lock()
	if len(upstreamBodies) != 3 {
		t.Fatalf("upstream calls = %d, want 3", len(upstreamBodies))
	}
	for i, want := range []struct{ model, thinking, effort string }{{"deepseek-v4-flash", "enabled", "max"}, {"deepseek-v4-flash", "enabled", "high"}, {"deepseek-v4-flash", "disabled", ""}} {
		body := upstreamBodies[i]
		if body["model"] != want.model {
			t.Fatalf("body %d model = %v, want %s", i, body["model"], want.model)
		}
		thinking, _ := body["thinking"].(map[string]any)
		if thinking["type"] != want.thinking {
			t.Fatalf("body %d thinking = %#v, want %s", i, thinking, want.thinking)
		}
		output, hasOutput := body["output_config"].(map[string]any)
		if want.effort == "" && hasOutput {
			t.Fatalf("body %d output_config = %#v, want omitted", i, output)
		}
		if want.effort != "" && (!hasOutput || output["effort"] != want.effort) {
			t.Fatalf("body %d output_config = %#v, want %s", i, output, want.effort)
		}
	}
	mu.Unlock()

	resolverPro := &smokeSlotResolver{slots: map[string]server.RoutingProfileSlotResult{"gpt-5.6-luna": {SlotID: routingprofile.SlotLuna, ActiveProfileID: "deepseek", ProviderKey: "deepseek", UpstreamModel: "deepseek-v4-pro", Mode: routingprofile.ModeNormal}}}
	// This is the same atomic refresh seam used by profile mutation callbacks.
	gatewayHandler.SwapRoutingProfileResolver(resolverPro)
	smokePost(t, state.ListeningAddress, "gpt-5.6-luna")
	waitForSmokeEvents(t, traffic, 8)
	mu.Lock()
	if upstreamBodies[3]["model"] != "deepseek-v4-pro" {
		t.Fatalf("refreshed body model = %v, want deepseek-v4-pro", upstreamBodies[3]["model"])
	}
	mu.Unlock()
	logWriter.Close(true)
	data, err := os.ReadFile(logWriter.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dummy") || strings.Contains(string(data), "Authorization") || !strings.Contains(string(data), "routing_resolved") || !strings.Contains(string(data), "provider_request_prepared") {
		t.Fatalf("autosave security/event assertion failed")
	}
}

// TestRoutingObservabilityProductionEquivalentSmoke exercises the real
// Desktop App -> Gateway -> Capture -> provider transport -> autosave path
// with an isolated config/home and a local mock provider. It deliberately
// starts with Luna so the first routed request is the one whose pair must be
// observable. No real provider, credential, prompt, or raw body is used.
func TestRoutingObservabilityProductionEquivalentSmoke(t *testing.T) {
	type providerObservation struct {
		Model    string
		Thinking string
		Effort   string
	}
	var providerMu sync.Mutex
	var providerObservations []providerObservation
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(trafficanalysis.RequestCorrelationHeader) != "" || r.Header.Get(server.RelayMarkerHeader) != "" {
			t.Fatalf("internal correlation headers reached provider transport")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("provider mock read failed")
		}
		var request struct {
			Model    string `json:"model"`
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			OutputConfig struct {
				Effort string `json:"effort"`
			} `json:"output_config"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("provider mock request was not JSON")
		}
		providerMu.Lock()
		providerObservations = append(providerObservations, providerObservation{Model: request.Model, Thinking: request.Thinking.Type, Effort: request.OutputConfig.Effort})
		providerMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"production-smoke","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	backupDir := filepath.Join(root, "backups")
	recoveryDir := filepath.Join(root, "recovery")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-5.6-luna\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gatewayPath := filepath.Join(root, "gateway.yml")
	gatewayYAML := fmt.Sprintf(`mode: Transform
server:
  addr: 127.0.0.1:0
  auth_token: desktop-smoke-token
models:
  deepseek-v4-flash:
    extensions:
      deepseek_v4:
        enabled: true
providers:
  deepseek:
    protocol: anthropic
    base_url: %s
    api_key: test-only
    version: 2023-06-01
    offers:
      - model: deepseek-v4-flash
routes:
  moonbridge:
    provider: deepseek
    model: deepseek-v4-flash
persistence:
  active_provider: db_sqlite
extensions:
  db_sqlite:
    enabled: true
    config:
      path: "%s"
      wal: true
      busy_timeout_ms: 5000
  routing_profiles:
    enabled: true
    config:
      active_profile: deepseek
      profiles:
        deepseek:
          display_name: DeepSeek
          slots:
            sol:
              provider: deepseek
              upstream_model: deepseek-v4-flash
              mode: thinking
              reasoning: max
            terra:
              provider: deepseek
              upstream_model: deepseek-v4-flash
              mode: thinking
              reasoning: high
            luna:
              provider: deepseek
              upstream_model: deepseek-v4-flash
              mode: normal
              reasoning: null
`, upstream.URL, filepath.ToSlash(filepath.Join(dataDir, "moonbridge.db")))
	if err := os.WriteFile(gatewayPath, []byte(gatewayYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   recoveryDir,
		CodexHome:     codexHome,
		BackupDir:     backupDir,
		TrafficLogDir: filepath.Join(root, "logs"),
		AppDataRoot:   filepath.Join(root, "appdata"),
	}, filepath.Join(recoveryDir, "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(AppOptions{
		Service:      gateway.NewService(gateway.ServiceOptions{Errors: io.Discard}),
		ConfigPath:   gatewayPath,
		NewIdentity:  func() (string, string) { return "desktop-smoke-instance", "desktop-smoke-token" },
		CodexConfig:  codexconfig.New(codexconfig.Options{Home: codexHome, BackupDir: backupDir}),
		Recovery:     store,
		RecoveryHome: codexHome,
		BackupDir:    backupDir,
		EmitEvents:   noopEmit,
	})
	app.trafficLogDir = filepath.Join(root, "logs")
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	started := app.StartGateway(StartGatewayRequest{})
	if !started.OK || started.Value == nil || started.Value.State != "running" {
		t.Fatalf("StartGateway() = %#v, want running", started)
	}
	if _, err := app.ensureTrafficTransaction(); err != nil {
		t.Fatalf("ensureTrafficTransaction() error = %v", err)
	}
	trafficStarted := app.StartTrafficAnalysis()
	if !trafficStarted.OK || trafficStarted.Value == nil || trafficStarted.Value.TrafficAnalysis == nil {
		t.Fatalf("StartTrafficAnalysis() = %#v, want success", trafficStarted)
	}
	listen := app.traffic.Status().ListeningAddress
	if listen == "" {
		t.Fatal("StartTrafficAnalysis() returned empty Capture listener")
	}

	// Luna is intentionally the first request after Capture readiness.
	for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"} {
		productionSmokePost(t, listen, app.session.ServerToken, model)
	}
	providerMu.Lock()
	providerSnapshot := append([]providerObservation(nil), providerObservations...)
	providerMu.Unlock()
	if len(providerSnapshot) < 3 {
		t.Fatalf("provider request count = %d, want at least 3", len(providerSnapshot))
	}
	wantProvider := []providerObservation{{Model: "deepseek-v4-flash", Thinking: "disabled", Effort: ""}, {Model: "deepseek-v4-flash", Thinking: "enabled", Effort: "max"}, {Model: "deepseek-v4-flash", Thinking: "enabled", Effort: "high"}}
	for i, want := range wantProvider {
		if providerSnapshot[len(providerSnapshot)-len(wantProvider)+i] != want {
			t.Fatalf("provider requests safe fields = %#v; request %d = %#v, want %#v", providerSnapshot, i+1, providerSnapshot[len(providerSnapshot)-len(wantProvider)+i], want)
		}
	}

	items := waitForProductionSmokeObservations(t, app.traffic, []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"})
	byModel := make(map[string]string)
	for _, item := range items {
		if item.Direction == trafficanalysis.DirectionClientToUpstream && item.PayloadShape != nil && item.PayloadShape.RequestModel != "" && item.RequestID != "" {
			byModel[item.PayloadShape.RequestModel] = item.RequestID
		}
	}
	wantEffort := map[string]string{"gpt-5.6-luna": "none", "gpt-5.6-sol": "max", "gpt-5.6-terra": "high"}
	wantThinking := map[string]string{"gpt-5.6-luna": "disabled", "gpt-5.6-sol": "enabled", "gpt-5.6-terra": "enabled"}
	for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"} {
		alias := byModel[model]
		if alias == "" {
			t.Fatalf("no Capture request observation for %s", model)
		}
		var events []trafficanalysis.Observation
		for _, item := range items {
			if item.GatewayEvent != nil && item.GatewayEvent.RequestAlias == alias {
				events = append(events, item)
			}
		}
		if len(events) != 2 || events[0].Kind != trafficanalysis.ObservationRoutingResolved || events[1].Kind != trafficanalysis.ObservationProviderRequestPrepared {
			var safeEvents []string
			for _, item := range events {
				if item.GatewayEvent != nil {
					safeEvents = append(safeEvents, fmt.Sprintf("%s/%s/%s/%s/%s", item.Kind, item.GatewayEvent.RequestedModel, item.GatewayEvent.RoutingSlot, item.GatewayEvent.Mode, item.GatewayEvent.Thinking))
				}
			}
			t.Fatalf("events for %s alias %s = %v, want ordered routing/prepared pair", model, alias, safeEvents)
		}
		resolved := events[0].GatewayEvent
		prepared := events[1].GatewayEvent
		if resolved.RequestedModel != model || resolved.RoutingSlot != strings.TrimPrefix(strings.TrimPrefix(model, "gpt-5.6-"), "") || resolved.Mode != map[string]string{"gpt-5.6-luna": "normal", "gpt-5.6-sol": "thinking", "gpt-5.6-terra": "thinking"}[model] {
			t.Fatalf("resolved %s = %#v", model, resolved)
		}
		if prepared.RequestedModel != model || prepared.RoutingSlot != resolved.RoutingSlot || prepared.Mode != resolved.Mode || prepared.Thinking != wantThinking[model] || prepared.EffectiveEffort != wantEffort[model] {
			t.Fatalf("prepared %s = %#v", model, prepared)
		}
		responseObserved := false
		for _, item := range items {
			if item.Direction == trafficanalysis.DirectionUpstreamToClient && item.RequestID == alias && item.StatusCode == http.StatusOK {
				responseObserved = true
				break
			}
		}
		if !responseObserved {
			t.Fatalf("response for %s alias %s was not observed with status 200", model, alias)
		}
	}

	stopped := app.StopTrafficAnalysis()
	if !stopped.OK {
		t.Fatalf("StopTrafficAnalysis() = %#v, want success", stopped)
	}
	logs := collectTrafficLogs(app.trafficLogDir)
	if len(logs) == 0 {
		t.Fatal("production smoke did not create an autosave log")
	}
	logData, err := os.ReadFile(logs[len(logs)-1])
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, required := range []string{"request_alias: req#1", "requested_model: gpt-5.6-luna", "routing_slot: luna", "thinking: disabled", "status_code: 200"} {
		if !strings.Contains(logText, required) {
			t.Fatalf("autosave missing safe field %q", required)
		}
	}
	for _, forbidden := range []string{"X-Moonbridge-Request", "desktop-smoke-token", "test-only", "smoke"} {
		if strings.Contains(logText, forbidden) {
			t.Fatalf("autosave contains forbidden value %q", forbidden)
		}
	}
}

func productionSmokePost(t *testing.T, address, token, model string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+address+"/responses", strings.NewReader(`{"model":"`+model+`","reasoning":{"effort":"medium"},"input":"smoke"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("production smoke %s status = %d body_len=%d", model, resp.StatusCode, len(body))
	}
}

func waitForProductionSmokeObservations(t *testing.T, traffic *trafficanalysis.Service, models []string) []trafficanalysis.Observation {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		items, _ := traffic.Observations(0)
		seen := make(map[string]bool)
		requestCount := 0
		responseCount := 0
		gatewayCount := 0
		for _, item := range items {
			if item.Direction == trafficanalysis.DirectionClientToUpstream && item.PayloadShape != nil {
				seen[item.PayloadShape.RequestModel] = true
			}
			if item.Direction == trafficanalysis.DirectionClientToUpstream && item.PayloadShape != nil && item.PayloadShape.RequestModel != "" {
				requestCount++
			}
			if item.Direction == trafficanalysis.DirectionUpstreamToClient && item.StatusCode == http.StatusOK {
				responseCount++
			}
			if item.GatewayEvent != nil {
				gatewayCount++
			}
		}
		complete := true
		for _, model := range models {
			if !seen[model] {
				complete = false
				break
			}
		}
		if complete && requestCount >= len(models) && responseCount >= len(models) && gatewayCount >= len(models)*2 {
			return items
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("production smoke observations did not reach all requested models; seen=%v", seen)
		}
	}
}

type routingSmokeTransport func(*http.Request) (*http.Response, error)

func (f routingSmokeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func smokeBool(v bool) *bool                                                        { return &v }
func smokeString(v string) *string                                                  { return &v }

func smokePost(t *testing.T, address, model string) {
	t.Helper()
	resp, err := http.Post("http://"+address+"/responses", "application/json", strings.NewReader(`{"model":"`+model+`","reasoning":{"effort":"medium"},"input":"smoke"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke %s status = %d", model, resp.StatusCode)
	}
}

func waitForSmokeEvents(t *testing.T, traffic *trafficanalysis.Service, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := traffic.Observations(0)
		count := 0
		for _, item := range items {
			if item.GatewayEvent != nil {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("gateway events did not reach %d", want)
}
