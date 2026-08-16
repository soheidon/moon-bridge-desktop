package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/gateway"
)

// integrationConfig writes a CaptureAnthropic config that points the SQLite
// config store (the source of truth for configgraph) at a temp DB, so the test
// never touches a real user store. serverToken controls the gateway auth token
// passed to codex auth.json generation.
func integrationConfig(t *testing.T, serverToken string) (configPath, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "moonbridge.db")
	content := "mode: Transform\n" +
		"server:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  auth_token: " + strconv.Quote(serverToken) + "\n" +
		"defaults:\n" +
		"  model: moonbridge\n" +
		"  max_tokens: 1024\n" +
		"models:\n" +
		"  local-test-model:\n" +
		"    context_window: 128000\n" +
		"    max_output_tokens: 4096\n" +
		"providers:\n" +
		"  local:\n" +
		"    base_url: https://api.example.invalid\n" +
		"    api_key: test-key\n" +
		"    protocol: openai-chat\n" +
		"    offers:\n" +
		"      - model: local-test-model\n" +
		"        upstream_name: local-test-model\n" +
		"routes:\n" +
		"  moonbridge:\n" +
		"    provider: local\n" +
		"    model: local-test-model\n" +
		"persistence:\n" +
		"  active_provider: db_sqlite\n" +
		"extensions:\n" +
		"  db_sqlite:\n" +
		"    enabled: true\n" +
		"    config:\n" +
		"      path: " + sqlitePathYAML(dbPath) + "\n" +
		"      wal: false\n"
	configPath = filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath, dbPath
}

// sqlitePathYAML emits the db path with backslashes escaped and quotes so the
// YAML parses on Windows.
func sqlitePathYAML(p string) string {
	return strconv.Quote(strings.ReplaceAll(p, `\`, `\\`))
}

func TestRealGatewayDeepSeekSavePersistsToSQLite(t *testing.T) {
	configPath, dbPath := integrationConfig(t, "server-tok")
	svc := gateway.NewService(gateway.ServiceOptions{Errors: os.Stderr})
	newIdentity, _ := sequenceIdentity()
	app := NewApp(scopedGatewayIntegration(t, AppOptions{
		Service:     svc,
		NewIdentity: newIdentity,
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	}))
	defer app.shutdown(context.Background())

	start := app.StartGateway(StartGatewayRequest{})
	if !start.OK || start.Value == nil {
		t.Fatalf("StartGateway() = %#v, want ok", start)
	}
	if app.session == nil {
		t.Fatal("session not built after real gateway start")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("sqlite store db not created: %v", err)
	}

	// Save through the REAL deepseek controller (HTTP against the live session).
	res := app.SaveDeepSeekSettings(deepseek.Input{
		APIKey: "sk-integration-12345678", DefaultModel: "flash", ProReasoning: "max", FlashReasoning: "low",
	})
	if !res.OK {
		t.Fatalf("SaveDeepSeekSettings() = %#v, want ok (no final_state_mismatch)", res)
	}
	if res.Value == nil || res.Value.DeepSeek == nil || !res.Value.DeepSeek.Configured {
		t.Fatalf("snapshot = %#v, want configured=true", res.Value)
	}
	if !res.Value.DeepSeek.APIKeySet || !res.Value.DeepSeek.Active {
		t.Fatalf("snapshot = %#v, want api key present and active", res.Value.DeepSeek)
	}

	// The SQLite store is the source of truth: config.yml must NOT have gained the
	// deepseek provider (YAML is a one-time seed, never rewritten).
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "deepseek") {
		t.Fatal("config.yml was rewritten with the deepseek provider — YAML must not be the source of truth")
	}

	// The persisted graph (via the management API with the real control token)
	// reflects the save: the starter resources remain, but the active
	// moonbridge route target is switched to DeepSeek Flash.
	graph := fetchGraph(t, app.session.Address, app.session.ControlToken)
	if got := graphResourceString(graph, configgraph.ResourceRoute, deepseek.RouteID, "provider"); got != deepseek.ProviderID {
		t.Fatalf("route.moonbridge.provider = %q, want %q", got, deepseek.ProviderID)
	}
	if got := graphResourceString(graph, configgraph.ResourceRoute, deepseek.RouteID, "model"); got != deepseek.ModelFlash {
		t.Fatalf("route.moonbridge.model = %q, want %q", got, deepseek.ModelFlash)
	}
	if got := graphResourceString(graph, configgraph.ResourceDefaults, "main", "model"); got != deepseek.RouteID {
		t.Fatalf("defaults.model = %q, want %q", got, deepseek.RouteID)
	}
	if !graphResourceExists(graph, configgraph.ResourceProvider, "local") {
		t.Fatal("starter provider local was removed; resources should remain while active route changes")
	}
	if got := graphResourceString(graph, configgraph.ResourceModel, deepseek.ModelPro, "default_reasoning_level"); got != "max" {
		t.Fatalf("pro model reasoning = %q, want max", got)
	}
	if got := graphResourceString(graph, configgraph.ResourceModel, deepseek.ModelFlash, "default_reasoning_level"); got != "low" {
		t.Fatalf("flash model reasoning = %q, want low", got)
	}

	// Restart keeps the settings: a fresh instance loads the persisting store and
	// still reports the deepseek setup as configured.
	if !app.RestartGateway(RestartGatewayRequest{}).OK {
		t.Fatal("RestartGateway() not ok")
	}
	if app.session == nil {
		t.Fatal("session not rebuilt after restart")
	}
	if got := app.session.InstanceID; got == "" {
		t.Fatal("session instance id empty after restart")
	}
	load := app.LoadDeepSeekSettings()
	if !load.OK || load.Value == nil || load.Value.DeepSeek == nil {
		t.Fatalf("LoadDeepSeekSettings() after restart = %#v, want ok", load)
	}
	if !load.Value.DeepSeek.Configured || !load.Value.DeepSeek.ProviderExists {
		t.Fatalf("settings not retained across gateway restart: %#v", load.Value.DeepSeek)
	}
	if load.Value.DeepSeek.SelectedModel != deepseek.ModelFlash {
		t.Fatalf("selectedModel after restart = %q, want %q", load.Value.DeepSeek.SelectedModel, deepseek.ModelFlash)
	}
	if app.session.ConfigValid != true {
		t.Fatal("session.ConfigValid = false after restart, want true")
	}

	// The derive path (used for codex generation) succeeds and stamps the live
	// server token, and never carries the masked api_key into the codex inputs.
	derived, derr := app.deriveConfigCodex(app.session)
	if derr != nil {
		t.Fatalf("deriveConfigCodex() error = %v", derr)
	}
	if derived.AuthToken != app.session.ServerToken {
		t.Fatalf("derived AuthToken = %q, want %q (session server token)", derived.AuthToken, app.session.ServerToken)
	}
	if got := config.ProviderFromGlobalConfig(&derived).Providers[deepseek.ProviderID].APIKey; got == "sk-integration-12345678" {
		t.Fatal("plaintext provider api_key leaked into the derived codex config")
	}
}

// TestRealGatewayClearDeepSeekKeyWhileStopped saves a real key, stops the
// gateway, then clears it through the stopped-state store path. The persisted
// SQLite key must be gone on a subsequent stopped read, and a second clear is
// idempotent.
func TestRealGatewayClearDeepSeekKeyWhileStopped(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	configPath, _ := integrationConfig(t, "server-tok")
	svc := gateway.NewService(gateway.ServiceOptions{Errors: os.Stderr})
	app := NewApp(scopedGatewayIntegration(t, AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	}))
	defer app.shutdown(context.Background())

	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	if !app.SaveDeepSeekSettings(deepseek.Input{
		APIKey: "sk-integration-clear-12345", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "low",
	}).OK {
		t.Fatal("SaveDeepSeekSettings() not ok")
	}
	if !app.StopGateway(StopGatewayRequest{}).OK {
		t.Fatal("StopGateway() not ok")
	}
	if app.session != nil {
		t.Fatal("session not cleared after stop")
	}

	res := app.ClearDeepSeekKey("op-clear-1")
	if !res.OK || res.Error != nil {
		t.Fatalf("ClearDeepSeekKey() = %#v, want ok", res)
	}
	if res.Value == nil || res.Value.DeepSeek == nil {
		t.Fatalf("value = %#v, want deepseek snapshot", res.Value)
	}
	if res.Value.DeepSeek.GatewayRunning {
		t.Fatal("gatewayRunning = true after stopped clear, want false")
	}
	if res.Value.DeepSeek.APIKeySet {
		t.Fatal("apiKeySet = true after stopped clear, want false")
	}
	if res.Value.DeepSeek.CredentialSource != "none" || res.Value.DeepSeek.CredentialState != "missing" {
		t.Fatalf("credential after clear = %q/%q, want none/missing", res.Value.DeepSeek.CredentialSource, res.Value.DeepSeek.CredentialState)
	}

	// Re-read from the persisted store to confirm the key is really gone.
	load := app.LoadDeepSeekSettings()
	if !load.OK || load.Value == nil || load.Value.DeepSeek == nil {
		t.Fatalf("LoadDeepSeekSettings() after clear = %#v, want ok", load)
	}
	if load.Value.DeepSeek.APIKeySet {
		t.Fatal("apiKeySet = true after reload, want false (persisted key cleared)")
	}

	// Idempotent: clearing again returns the same empty snapshot without error.
	again := app.ClearDeepSeekKey("op-clear-2")
	if !again.OK || again.Error != nil {
		t.Fatalf("second ClearDeepSeekKey() = %#v, want ok (idempotent)", again)
	}
	if again.Value == nil || again.Value.DeepSeek == nil || again.Value.DeepSeek.APIKeySet {
		t.Fatalf("second clear snapshot = %#v, want keyless", again.Value)
	}
}

// TestRealGatewayClearDeepSeekKeyStoppedProviderMissing seeds only the starter
// provider (never a deepseek save), stops, then clears: the missing deepseek
// provider must not panic the map access and the current snapshot is returned
// unchanged.
func TestRealGatewayClearDeepSeekKeyStoppedProviderMissing(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	configPath, _ := integrationConfig(t, "server-tok")
	svc := gateway.NewService(gateway.ServiceOptions{Errors: os.Stderr})
	app := NewApp(scopedGatewayIntegration(t, AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	}))
	defer app.shutdown(context.Background())

	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	if !app.StopGateway(StopGatewayRequest{}).OK {
		t.Fatal("StopGateway() not ok")
	}

	res := app.ClearDeepSeekKey("op-clear-missing")
	if !res.OK || res.Error != nil {
		t.Fatalf("ClearDeepSeekKey() = %#v, want ok without panic", res)
	}
	if res.Value == nil || res.Value.DeepSeek == nil {
		t.Fatalf("value = %#v, want deepseek snapshot", res.Value)
	}
	if res.Value.DeepSeek.ProviderExists {
		t.Fatal("providerExists = true, want false (no deepseek provider saved)")
	}
}

func TestRealGatewayLoadWithoutSave(t *testing.T) {
	configPath, _ := integrationConfig(t, "server-tok")
	svc := gateway.NewService(gateway.ServiceOptions{Errors: os.Stderr})
	app := NewApp(scopedGatewayIntegration(t, AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	}))
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	res := app.LoadDeepSeekSettings()
	if !res.OK || res.Value == nil || res.Value.DeepSeek == nil {
		t.Fatalf("LoadDeepSeekSettings() on fresh store = %#v, want ok with provider missing", res)
	}
	if res.Value.DeepSeek.ProviderExists {
		t.Fatal("providerExists = true on a fresh store before any save")
	}
}

// fetchGraph GETs the management API config graph using the live control token.
func fetchGraph(t *testing.T, addr, token string) configgraph.Graph {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/config/graph", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET config/graph status = %d, want 200", resp.StatusCode)
	}
	var g configgraph.Graph
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	return g
}

func graphResourceString(g configgraph.Graph, kind configgraph.ResourceKind, id, field string) string {
	for _, r := range g.Resources {
		if r.Kind == kind && r.ID == id {
			if v, ok := r.Value[field].(string); ok {
				return v
			}
		}
	}
	return ""
}

func graphResourceExists(g configgraph.Graph, kind configgraph.ResourceKind, id string) bool {
	for _, r := range g.Resources {
		if r.Kind == kind && r.ID == id {
			return true
		}
	}
	return false
}
