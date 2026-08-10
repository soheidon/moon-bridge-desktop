package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"moonbridge/internal/config"
	bridgeapp "moonbridge/internal/service/app"
	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/routingprofile"
)

// scriptedRoutingProfile is a routingProfileController fake. The zero value
// returns an empty snapshot and nil errors; tests override snapshot / loadErr /
// saveErr / activateErr. It records the last ActivateSlot args so tests can pin
// which profile/slot reached the service.
type scriptedRoutingProfile struct {
	snapshot         *routingprofile.Snapshot
	loadErr          error
	saveErr          error
	activateErr      error
	activatedProfile string
	activatedSlot    string
}

func (r *scriptedRoutingProfile) Load(context.Context) (*routingprofile.Snapshot, error) {
	return r.snapshot, r.loadErr
}

func (r *scriptedRoutingProfile) Save(context.Context, routingprofile.Input) (*routingprofile.Snapshot, error) {
	return r.snapshot, r.saveErr
}

func (r *scriptedRoutingProfile) ActivateSlot(_ context.Context, profileID, slotID string) (*routingprofile.Snapshot, error) {
	r.activatedProfile = profileID
	r.activatedSlot = slotID
	return r.snapshot, r.activateErr
}

func (r *scriptedRoutingProfile) ActivateProfile(_ context.Context, profileID string) (*routingprofile.Snapshot, error) {
	r.activatedProfile = profileID
	r.activatedSlot = ""
	return r.snapshot, r.activateErr
}

// routingProfileSnapshot is the confirmed default table: active DeepSeek profile
// with sol→flash/max, terra→flash/high, luna→flash/no override.
func routingProfileSnapshot() *routingprofile.Snapshot {
	max := deepseek.ReasoningMax
	high := deepseek.ReasoningHigh
	return &routingprofile.Snapshot{
		GatewayRunning:  true,
		ActiveProfileID: deepseek.ProviderID,
		Profiles: []routingprofile.Profile{{
			ID: deepseek.ProviderID, DisplayName: "DeepSeek", Active: true, Configured: true,
			Slots: []routingprofile.Slot{
				{ID: routingprofile.SlotSol, DisplayName: "Sol", ProviderID: deepseek.ProviderID, ProviderLabel: "DeepSeek", UpstreamModel: deepseek.ModelFlash, Reasoning: &max},
				{ID: routingprofile.SlotTerra, DisplayName: "Terra", ProviderID: deepseek.ProviderID, ProviderLabel: "DeepSeek", UpstreamModel: deepseek.ModelFlash, Reasoning: &high},
				{ID: routingprofile.SlotLuna, DisplayName: "Luna", ProviderID: deepseek.ProviderID, ProviderLabel: "DeepSeek", UpstreamModel: deepseek.ModelFlash, Reasoning: nil},
			},
		}},
	}
}

func validRoutingProfileInput() routingprofile.Input {
	return routingprofile.Input{Profile: routingprofile.ProfileInput{
		ID: deepseek.ProviderID, DisplayName: "DeepSeek",
		Slots: map[string]routingprofile.SlotInput{
			routingprofile.SlotSol:   {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningMax)},
			routingprofile.SlotTerra: {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningHigh)},
			routingprofile.SlotLuna:  {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: nil},
		},
	}}
}

// routingProfileInputWithPro returns a valid Input that changes Sol to Pro+high.
func routingProfileInputWithPro() routingprofile.Input {
	return routingprofile.Input{Profile: routingprofile.ProfileInput{
		ID: deepseek.ProviderID, DisplayName: "DeepSeek",
		Slots: map[string]routingprofile.SlotInput{
			routingprofile.SlotSol:   {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelPro, Reasoning: strPtr(deepseek.ReasoningHigh)},
			routingprofile.SlotTerra: {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: strPtr(deepseek.ReasoningHigh)},
			routingprofile.SlotLuna:  {Provider: deepseek.ProviderID, UpstreamModel: deepseek.ModelFlash, Reasoning: nil},
		},
	}}
}

// buildSlotResolverFromConfig builds a SlotResolver from a config by constructing
// a configgraph. This mirrors the production path in app.buildSlotResolver.
func buildSlotResolverFromConfig(t *testing.T, cfg config.Config) *routingprofile.SlotResolver {
	t.Helper()
	graph := configgraph.BuildGraph(cfg, "")
	resolver := routingprofile.NewSlotResolver(graph)
	if resolver.BootstrapEligible() {
		activeProfileID := ""
		if route, ok := cfg.Routes["moonbridge"]; ok {
			activeProfileID = route.Provider
		}
		resolver = routingprofile.NewSlotResolverFromDefaults(activeProfileID)
	}
	return resolver
}

// configWithRoutingProfileExtension builds a config.Config whose extension
// resources include the given routing_profiles table and active_profile.
func configWithRoutingProfileExtension(t *testing.T, base config.Config, profiles map[string]map[string]any, activeProfile string) config.Config {
	t.Helper()
	cfg := base
	if cfg.Extensions == nil {
		cfg.Extensions = map[string]config.ExtensionSettings{}
	}
	trueVal := true
	cfg.Extensions["routing_profiles"] = config.ExtensionSettings{
		Enabled: &trueVal,
		RawConfig: map[string]any{
			"profiles":       profiles,
			"active_profile": activeProfile,
		},
	}
	return cfg
}

// ---- Load ----

func TestLoadRoutingProfilesStoppedFallsBackToDefaultWhenStoreEmpty(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service: svc, NewIdentity: fixedIdentity("inst-1", "token-1"), ConfigPath: cfg, EmitEvents: noopEmit,
	})
	defer app.shutdown(context.Background())

	// writeCaptureAnthropicConfig has no db_sqlite extension, so the stopped
	// read falls back to YAML, which has no routing_profiles either → default
	// table. This pins that a store-less config does not error.
	res := app.LoadRoutingProfiles()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadRoutingProfiles() = %#v, want ok with nil error (stopped read path)", res)
	}
	rp := res.Value.RoutingProfiles
	if rp == nil {
		t.Fatal("value.routingProfiles = nil")
	}
	if rp.GatewayRunning {
		t.Fatal("gatewayRunning = true, want false (gateway is stopped)")
	}
	if len(rp.Profiles) == 0 {
		t.Fatal("profiles = 0, want at least 1 (default table)")
	}
	card := rp.Profiles[0]
	if card.ID != deepseek.ProviderID {
		t.Fatalf("profile id = %q, want %q (default table)", card.ID, deepseek.ProviderID)
	}
	if len(card.Slots) == 0 {
		t.Fatal("slots = 0, want the default DeepSeek table")
	}
	sol := card.Slots[0]
	if sol.UpstreamModel != deepseek.ModelFlash {
		t.Fatalf("Sol upstream = %q, want %q (default table, not persisted)", sol.UpstreamModel, deepseek.ModelFlash)
	}
	if sol.Reasoning == nil || *sol.Reasoning != deepseek.ReasoningMax {
		t.Fatalf("Sol reasoning = %v, want max (default table)", sol.Reasoning)
	}
}

// TestLoadRoutingProfilesStoppedReadsPersistedSQLite proves SQLite > YAML
// precedence for the stopped read. The YAML is a default seed (no
// routing_profiles), while the persisted store holds Sol = Pro+high active on
// deepseek. A YAML-only implementation returns the default table (Flash/max);
// the stopped read must return the persisted values instead.
func TestLoadRoutingProfilesStoppedReadsPersistedSQLite(t *testing.T) {
	configPath, dbPath := integrationConfig(t, "server-tok")
	specs := bridgeapp.BuiltinExtensions().ConfigSpecs()

	base, err := config.LoadFromFileWithOptions(configPath, config.LoadOptions{ExtensionSpecs: specs})
	if err != nil {
		t.Fatalf("LoadFromFileWithOptions() error = %v", err)
	}

	// Persist a non-default table: Sol = Pro+high, active on deepseek. The
	// default table maps Sol to Flash/max, so any YAML-only read differs.
	saved := configWithRoutingProfileExtension(t, base, map[string]map[string]any{
		deepseek.ProviderID: {
			"display_name": "DeepSeek",
			"slots": map[string]any{
				"sol":   map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelPro, "reasoning": deepseek.ReasoningHigh},
				"terra": map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash, "reasoning": deepseek.ReasoningHigh},
				"luna":  map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash},
			},
		},
	}, deepseek.ProviderID)
	// Validate requires route providers to exist. The route stays on the base
	// provider: ActiveProfileID must be restored from routing_profiles
	// config.active_profile alone (persisted SQLite), not from the route.
	saved.ProviderDefs[deepseek.ProviderID] = config.ProviderDef{
		BaseURL: deepseek.BaseURL, APIKey: "sk-test-routing", Protocol: deepseek.Protocol,
	}

	cs, closeStore, err := openPersistedConfigStore(dbPath, specs)
	if err != nil {
		t.Fatalf("openPersistedConfigStore() error = %v", err)
	}
	if _, err := cs.SaveConfig(context.Background(), &saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	closeStore()

	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service: svc, NewIdentity: fixedIdentity("inst-1", "token-1"), ConfigPath: configPath, EmitEvents: noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.LoadRoutingProfiles()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadRoutingProfiles() = %#v, want ok", res)
	}
	rp := res.Value.RoutingProfiles
	if rp == nil {
		t.Fatal("value.routingProfiles = nil")
	}
	if rp.GatewayRunning {
		t.Fatal("gatewayRunning = true, want false (gateway stopped)")
	}
	if rp.ActiveProfileID != deepseek.ProviderID {
		t.Fatalf("activeProfileId = %q, want %q (persisted SQLite active_profile)", rp.ActiveProfileID, deepseek.ProviderID)
	}
	if len(rp.Profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(rp.Profiles))
	}
	card := rp.Profiles[0]
	if len(card.Slots) != 3 {
		t.Fatalf("slots = %d, want 3", len(card.Slots))
	}
	sol := card.Slots[0]
	if sol.UpstreamModel != deepseek.ModelPro {
		t.Fatalf("Sol upstream = %q, want %q (persisted, not default Flash)", sol.UpstreamModel, deepseek.ModelPro)
	}
	if sol.Reasoning == nil || *sol.Reasoning != deepseek.ReasoningHigh {
		t.Fatalf("Sol reasoning = %v, want high (persisted, not default max)", sol.Reasoning)
	}
}

// writeTransformConfigWithSQLitePath writes a valid Transform config whose
// db_sqlite extension points at rawPath verbatim (relative or absolute),
// mirroring a real user config. The path is emitted as a quoted YAML string.
func writeTransformConfigWithSQLitePath(t *testing.T, dir, rawPath string) string {
	t.Helper()
	content := "mode: Transform\n" +
		"server:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  auth_token: \"server-tok\"\n" +
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
		"      path: " + sqlitePathYAML(rawPath) + "\n" +
		"      wal: false\n"
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath
}

// TestSQLiteDBPathResolutionReusesGatewayResolution proves a relative
// db_sqlite.config.path resolves to the same final DB path the gateway's Open
// uses: resolveSQLiteDBPath runs dbsqlite.ResolvePath (filepath.Abs against the
// process CWD), and a persisted write lands at that path. os.Chdir mutates the
// process CWD, so this test must not be parallel.
func TestSQLiteDBPathResolutionReusesGatewayResolution(t *testing.T) {
	dir := t.TempDir()
	configPath := writeTransformConfigWithSQLitePath(t, dir, "data/moonbridge.db")

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service: svc, NewIdentity: fixedIdentity("inst-1", "token-1"), ConfigPath: configPath, EmitEvents: noopEmit,
	})
	defer app.shutdown(context.Background())

	dbPath, ok, err := app.resolveSQLiteDBPath()
	if err != nil {
		t.Fatalf("resolveSQLiteDBPath() error = %v", err)
	}
	if !ok {
		t.Fatal("resolveSQLiteDBPath() ok = false, want true (db_sqlite path configured)")
	}
	want := filepath.Join(dir, "data", "moonbridge.db")
	if dbPath != want {
		t.Fatalf("resolveSQLiteDBPath() = %q, want %q (same resolution as gateway Open)", dbPath, want)
	}
	// Resolution is side-effect free: neither the parent directory nor the DB
	// file may be created by a mere stopped read.
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatalf("resolveSQLiteDBPath() created the parent dir: %v", err)
	}

	// A persisted write lands at the same resolved path, proving gateway startup
	// and stopped read target the same DB file.
	specs := bridgeapp.BuiltinExtensions().ConfigSpecs()
	base, err := config.LoadFromFileWithOptions(configPath, config.LoadOptions{ExtensionSpecs: specs})
	if err != nil {
		t.Fatalf("LoadFromFileWithOptions() error = %v", err)
	}
	cs, closeStore, err := openPersistedConfigStore(dbPath, specs)
	if err != nil {
		t.Fatalf("openPersistedConfigStore() error = %v", err)
	}
	if _, err := cs.SaveConfig(context.Background(), &base); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	closeStore()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("DB file not found at resolved path %q after save: %v", dbPath, err)
	}
}

func TestLoadRoutingProfilesSuccessAndSecretFree(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.LoadRoutingProfiles()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadRoutingProfiles() = %#v, want ok with nil error", res)
	}
	rp := res.Value.RoutingProfiles
	if rp == nil {
		t.Fatal("value.routingProfiles = nil")
	}
	if !rp.GatewayRunning {
		t.Fatal("gatewayRunning = false, want true")
	}
	if rp.ActiveProfileID != deepseek.ProviderID {
		t.Fatalf("activeProfileId = %q, want %q", rp.ActiveProfileID, deepseek.ProviderID)
	}
	if len(rp.Profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(rp.Profiles))
	}
	card := rp.Profiles[0]
	if card.ID != deepseek.ProviderID || !card.Active || !card.Configured || card.DisplayName != "DeepSeek" {
		t.Fatalf("card = %#v, want active configured DeepSeek", card)
	}
	if len(card.Slots) != 3 {
		t.Fatalf("slots = %d, want 3", len(card.Slots))
	}
	if card.Slots[0].Reasoning == nil || *card.Slots[0].Reasoning != deepseek.ReasoningMax {
		t.Fatalf("sol reasoning = %v, want max", card.Slots[0].Reasoning)
	}
	if card.Slots[2].Reasoning != nil {
		t.Fatalf("luna reasoning = %v, want nil (no override)", card.Slots[2].Reasoning)
	}

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	s := string(data)
	for _, secret := range []string{"token-1", "server-tok", "sk-", "Authorization", "apiKey", "controlToken"} {
		if strings.Contains(s, secret) {
			t.Fatalf("snapshot leaked %q: %s", secret, s)
		}
	}
	if strings.Contains(s, `"reasoning":null`) {
		t.Fatalf("wire carries reasoning:null for luna (must be omitted): %s", s)
	}
}

// ---- factory per-operation creation ----

func TestRoutingProfileFactoryRecreatesPerOperation(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	newIdentity, _ := sequenceIdentity()

	type factoryCall struct{ address, controlToken string }
	var mu sync.Mutex
	var calls []factoryCall

	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: newIdentity,
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(address, controlToken string) routingProfileController {
			mu.Lock()
			calls = append(calls, factoryCall{address: address, controlToken: controlToken})
			mu.Unlock()
			return &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	if res := app.LoadRoutingProfiles(); !res.OK {
		t.Fatalf("LoadRoutingProfiles() = %#v, want ok", res)
	}
	mu.Lock()
	got := append([]factoryCall(nil), calls...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("factory calls = %d, want 1", len(got))
	}
	if got[0].controlToken != "token-1" {
		t.Fatalf("control token = %q, want token-1", got[0].controlToken)
	}
	if got[0].address != "http://127.0.0.1:38440" {
		t.Fatalf("address = %q, want http://127.0.0.1:38440 (scheme included)", got[0].address)
	}

	// A gateway restart rotates the control token; the next operation must build
	// a fresh controller with the new token.
	if !app.RestartGateway(RestartGatewayRequest{}).OK {
		t.Fatal("RestartGateway() not ok")
	}
	if res := app.LoadRoutingProfiles(); !res.OK {
		t.Fatalf("LoadRoutingProfiles() after restart = %#v, want ok", res)
	}
	mu.Lock()
	got = append([]factoryCall(nil), calls...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("factory calls after restart = %d, want 2", len(got))
	}
	if got[1].controlToken != "token-2" {
		t.Fatalf("rotated control token = %q, want token-2", got[1].controlToken)
	}
}

// ---- Activate ----

func TestActivateRoutingSlotGatewayNotRunning(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service: svc, NewIdentity: fixedIdentity("inst-1", "token-1"), ConfigPath: cfg, EmitEvents: noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.ActivateRoutingSlot(ActivateRoutingSlotRequest{ProfileID: deepseek.ProviderID, SlotID: routingprofile.SlotSol})
	if res.OK || res.Error == nil || res.Error.Code != "routing_profile_gateway_not_running" {
		t.Fatalf("ActivateRoutingSlot() = %#v, want routing_profile_gateway_not_running", res)
	}
}

func TestActivateRoutingSlotSuccess(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.ActivateRoutingSlot(ActivateRoutingSlotRequest{ProfileID: deepseek.ProviderID, SlotID: routingprofile.SlotTerra})
	if !res.OK || res.Error != nil {
		t.Fatalf("ActivateRoutingSlot() = %#v, want ok", res)
	}
	if ctrl.activatedProfile != deepseek.ProviderID || ctrl.activatedSlot != routingprofile.SlotTerra {
		t.Fatalf("activate args = %q/%q, want %q/%q", ctrl.activatedProfile, ctrl.activatedSlot, deepseek.ProviderID, routingprofile.SlotTerra)
	}
	if res.Value.RoutingProfiles == nil {
		t.Fatal("value.routingProfiles = nil")
	}
	if app.session == nil || !app.session.ConfigValid {
		t.Fatal("session.ConfigValid = false after successful activate + refresh")
	}
}

func TestActivateRoutingSlotSessionRefreshTotalFailure(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: scriptedDeriver(config.Config{}, errors.New("effective fetch failed")),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.ActivateRoutingSlot(ActivateRoutingSlotRequest{ProfileID: deepseek.ProviderID, SlotID: routingprofile.SlotSol})
	if res.OK {
		t.Fatalf("ActivateRoutingSlot() ok=true, want partial-success failure: %#v", res)
	}
	if res.Value != nil {
		t.Fatalf("ActivateRoutingSlot() value = %#v, want nil on error (envelope contract)", res.Value)
	}
	e := res.Error
	if e == nil {
		t.Fatal("ActivateRoutingSlot() error = nil")
	}
	if e.Code != "routing_profile_activated_session_refresh_failed" {
		t.Fatalf("code = %q, want routing_profile_activated_session_refresh_failed", e.Code)
	}
	if e.Stage != "refresh_session_config" {
		t.Fatalf("stage = %q, want refresh_session_config", e.Stage)
	}
	if !e.MutationStarted {
		t.Fatal("mutationStarted = false, want true (activation succeeded)")
	}
	if !e.Retryable {
		t.Fatal("retryable = false, want true")
	}
	if e.Details["saved"] != true || e.Details["sessionConfigRefreshed"] != false {
		t.Fatalf("details = %#v, want saved=true sessionConfigRefreshed=false", e.Details)
	}
	if app.session == nil || app.session.ConfigValid {
		t.Fatal("session.ConfigValid = true, want false (stale)")
	}
}

// ---- Save ----

func TestSaveRoutingProfileValidateFailsWhileGatewayStopped(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service: svc, NewIdentity: fixedIdentity("inst-1", "token-1"), ConfigPath: cfg, EmitEvents: noopEmit,
	})
	defer app.shutdown(context.Background())

	bad := routingprofile.Input{Profile: routingprofile.ProfileInput{
		ID: deepseek.ProviderID, DisplayName: "DeepSeek", Slots: map[string]routingprofile.SlotInput{},
	}}
	res := app.SaveRoutingProfile(bad)
	if res.OK {
		t.Fatal("SaveRoutingProfile(invalid) ok = true, want false")
	}
	if res.Error == nil || res.Error.Code != "routing_profile_validate_failed" {
		t.Fatalf("SaveRoutingProfile(invalid) error = %#v, want routing_profile_validate_failed", res.Error)
	}
	if res.Error.Code == "routing_profile_gateway_not_running" {
		t.Fatal("validate leaked a gateway dependency (gateway is stopped)")
	}
}

func TestSaveRoutingProfileSuccess(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.SaveRoutingProfile(validRoutingProfileInput())
	if !res.OK || res.Error != nil {
		t.Fatalf("SaveRoutingProfile() = %#v, want ok", res)
	}
	if res.Value.RoutingProfiles == nil {
		t.Fatal("value.routingProfiles = nil")
	}
	if app.session == nil || !app.session.ConfigValid {
		t.Fatal("session.ConfigValid = false after successful save + refresh")
	}
}

func TestSaveRoutingProfileMutationError(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{
		snapshot: routingProfileSnapshot(),
		saveErr:  &routingprofile.ServiceError{Kind: routingprofile.KindSaveRejected, Message: "rejected", MutationStarted: true},
	}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.SaveRoutingProfile(validRoutingProfileInput())
	if res.OK {
		t.Fatal("SaveRoutingProfile() ok = true, want false")
	}
	if res.Value != nil {
		t.Fatalf("SaveRoutingProfile() value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "routing_profile_save_failed" {
		t.Fatalf("SaveRoutingProfile() error = %#v, want routing_profile_save_failed", res.Error)
	}
	if !res.Error.MutationStarted {
		t.Fatal("mutationStarted = false, want true (a mutation began before failure)")
	}
}

// ---- error mapping ----

func TestRoutingProfileErrorMapping(t *testing.T) {
	field := func(s string) *string { return &s }
	tests := []struct {
		name         string
		err          error
		wantCode     string
		wantRetry    bool
		wantMutation bool
	}{
		{"invalid input", &routingprofile.ServiceError{Kind: routingprofile.KindInvalidInput, Message: "bad", Field: field("profile.slots.sol")}, "routing_profile_validate_failed", false, false},
		{"save rejected", &routingprofile.ServiceError{Kind: routingprofile.KindSaveRejected, Message: "rejected", MutationStarted: true}, "routing_profile_save_failed", false, true},
		{"revision conflict exceeded", &routingprofile.ServiceError{Kind: routingprofile.KindRevisionConflictExceeded, Message: "conflict", MutationStarted: true, Retryable: true}, "routing_profile_save_failed", true, true},
		{"verify failed", &routingprofile.ServiceError{Kind: routingprofile.KindVerifyFailed, Message: "mismatch", MutationStarted: true}, "routing_profile_save_failed", false, true},
		{"plain error", errors.New("boom"), "routing_profile_load_failed", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := routingProfileError("Op", "load", "routing_profile_load_failed", tt.err)
			if res.OK {
				t.Fatal("ok = true, want false")
			}
			if res.Value != nil {
				t.Fatalf("value = %#v, want nil", res.Value)
			}
			if res.Error == nil {
				t.Fatal("error = nil")
			}
			if res.Error.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", res.Error.Code, tt.wantCode)
			}
			if res.Error.Retryable != tt.wantRetry {
				t.Fatalf("retryable = %v, want %v", res.Error.Retryable, tt.wantRetry)
			}
			if res.Error.MutationStarted != tt.wantMutation {
				t.Fatalf("mutationStarted = %v, want %v", res.Error.MutationStarted, tt.wantMutation)
			}
		})
	}
}

// ---- ActivateProfile (Plan 6: only changes active_profile) ----

func TestActivateProfileCallsServiceMethod(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.ActivateProfile(ActivateProfileRequest{ProfileID: deepseek.ProviderID})
	if !res.OK || res.Error != nil {
		t.Fatalf("ActivateProfile() = %#v, want ok", res)
	}
	if ctrl.activatedProfile != deepseek.ProviderID {
		t.Fatalf("activatedProfile = %q, want %q", ctrl.activatedProfile, deepseek.ProviderID)
	}
	if ctrl.activatedSlot != "" {
		t.Fatalf("activatedSlot = %q, want empty (ActivateProfile changes only active_profile)", ctrl.activatedSlot)
	}
}

func TestActivateProfileGatewayNotRunning(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service: svc, NewIdentity: fixedIdentity("inst-1", "token-1"), ConfigPath: cfg, EmitEvents: noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.ActivateProfile(ActivateProfileRequest{ProfileID: deepseek.ProviderID})
	if res.OK || res.Error == nil || res.Error.Code != "routing_profile_gateway_not_running" {
		t.Fatalf("ActivateProfile() = %#v, want routing_profile_gateway_not_running", res)
	}
}

func TestActivateProfileMutationErrorNoRefresh(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{
		snapshot:    routingProfileSnapshot(),
		activateErr: &routingprofile.ServiceError{Kind: routingprofile.KindSaveRejected, Message: "rejected", MutationStarted: true},
	}
	var refreshCount int
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
		RoutingProfileRefresh: func(cfg config.Config) {
			refreshCount++
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.ActivateProfile(ActivateProfileRequest{ProfileID: deepseek.ProviderID})
	if res.OK {
		t.Fatalf("ActivateProfile() ok = true, want false: %#v", res)
	}
	if refreshCount != 0 {
		t.Fatalf("refresh called %d times, want 0 (mutation failed)", refreshCount)
	}
}

func TestSaveRoutingProfileDoesNotChangeActiveProfile(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.SaveRoutingProfile(validRoutingProfileInput())
	if !res.OK || res.Error != nil {
		t.Fatalf("SaveRoutingProfile() = %#v, want ok", res)
	}
	if ctrl.activatedProfile != "" || ctrl.activatedSlot != "" {
		t.Fatalf("Save changed activation: profile=%q slot=%q, want both empty", ctrl.activatedProfile, ctrl.activatedSlot)
	}
}

func TestSaveAndActivateTriggerRefresh(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}
	var refreshCount int
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
		RoutingProfileRefresh: func(cfg config.Config) {
			refreshCount++
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	if res := app.SaveRoutingProfile(validRoutingProfileInput()); !res.OK {
		t.Fatalf("SaveRoutingProfile() = %#v, want ok", res)
	}
	if refreshCount != 1 {
		t.Fatalf("after Save: refresh called %d times, want 1", refreshCount)
	}

	if res := app.ActivateProfile(ActivateProfileRequest{ProfileID: deepseek.ProviderID}); !res.OK {
		t.Fatalf("ActivateProfile() = %#v, want ok", res)
	}
	if refreshCount != 2 {
		t.Fatalf("after Activate: refresh called %d times, want 2", refreshCount)
	}
}

// TestSaveRoutingProfileRefreshReceivesPostMutationConfig verifies that the
// refresh callback receives the post-mutation config (not stale pre-mutation).
// The dynamicDeriver returns a config with routing_profiles that includes the
// saved Pro+high for Sol, proving the refresh uses the latest config.
func TestSaveRoutingProfileRefreshReceivesPostMutationConfig(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}

	// Build a post-mutation config with Sol = Pro+high.
	postMutationCfg := configWithRoutingProfileExtension(t, config.Config{}, map[string]map[string]any{
		deepseek.ProviderID: {
			"display_name": "DeepSeek",
			"slots": map[string]any{
				"sol":   map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelPro, "reasoning": "high"},
				"terra": map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash, "reasoning": "high"},
				"luna":  map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash},
			},
		},
	}, deepseek.ProviderID)

	var capturedConfig config.Config
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: dynamicDeriver(func() (config.Config, error) {
			return postMutationCfg, nil
		}),
		RoutingProfileRefresh: func(cfg config.Config) {
			capturedConfig = cfg
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	// Save changes Sol from Flash+max to Pro+high.
	if res := app.SaveRoutingProfile(routingProfileInputWithPro()); !res.OK {
		t.Fatalf("SaveRoutingProfile() = %#v, want ok", res)
	}

	// Build resolver from the config received by the refresh callback.
	resolver := buildSlotResolverFromConfig(t, capturedConfig)
	result, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if !ok {
		t.Fatal("ResolveSlot(gpt-5.6-sol) = false, want true")
	}
	if result.UpstreamModel != deepseek.ModelPro {
		t.Fatalf("after Save: Sol upstream = %q, want %q (post-mutation config)", result.UpstreamModel, deepseek.ModelPro)
	}
	if result.Reasoning == nil || *result.Reasoning != deepseek.ReasoningHigh {
		t.Fatalf("after Save: Sol reasoning = %v, want high", result.Reasoning)
	}
}

// TestActivateProfileRefreshReceivesPostMutationConfig verifies that the
// refresh callback receives the post-mutation config with the new active_profile.
// The dynamicDeriver returns a config with active_profile = profile-B.
func TestActivateProfileRefreshReceivesPostMutationConfig(t *testing.T) {
	const profileB = "profile-b"

	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	ctrl := &scriptedRoutingProfile{snapshot: routingProfileSnapshot()}

	// Build a post-mutation config with active_profile = profile-B.
	postMutationCfg := configWithRoutingProfileExtension(t, config.Config{}, map[string]map[string]any{
		deepseek.ProviderID: {
			"display_name": "DeepSeek",
			"slots": map[string]any{
				"sol":   map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash, "reasoning": "max"},
				"terra": map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash, "reasoning": "high"},
				"luna":  map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash},
			},
		},
		profileB: {
			"display_name": "Profile B",
			"slots": map[string]any{
				"sol":   map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelPro, "reasoning": "high"},
				"terra": map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelPro},
				"luna":  map[string]any{"provider": deepseek.ProviderID, "upstream_model": deepseek.ModelFlash},
			},
		},
	}, profileB)

	var capturedConfig config.Config
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewRoutingProfile: func(_, _ string) routingProfileController {
			return ctrl
		},
		DeriveCodex: dynamicDeriver(func() (config.Config, error) {
			return postMutationCfg, nil
		}),
		RoutingProfileRefresh: func(cfg config.Config) {
			capturedConfig = cfg
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	// Activate profile-B.
	if res := app.ActivateProfile(ActivateProfileRequest{ProfileID: profileB}); !res.OK {
		t.Fatalf("ActivateProfile() = %#v, want ok", res)
	}

	// Build resolver from the config received by the refresh callback.
	resolver := buildSlotResolverFromConfig(t, capturedConfig)
	result, ok := resolver.ResolveSlot("gpt-5.6-sol")
	if !ok {
		t.Fatal("ResolveSlot(gpt-5.6-sol) = false, want true (profile-B active)")
	}
	if result.UpstreamModel != deepseek.ModelPro {
		t.Fatalf("after ActivateProfile: Sol upstream = %q, want %q (profile-B slot)", result.UpstreamModel, deepseek.ModelPro)
	}
	if result.Reasoning == nil || *result.Reasoning != deepseek.ReasoningHigh {
		t.Fatalf("after ActivateProfile: Sol reasoning = %v, want high", result.Reasoning)
	}
}
