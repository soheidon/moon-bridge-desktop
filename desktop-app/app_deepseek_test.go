package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/gateway"
)

// ---- fakes ----

// scriptedDeepSeek is a deepSeekController fake. The zero value returns an
// empty snapshot and nil errors; tests override snapshot / loadErr / saveErr.
type scriptedDeepSeek struct {
	snapshot    *deepseek.Snapshot
	loadErr     error
	saveErr     error
	clearErr    error
	validateErr error
	testResult  deepseek.ConnectionTestResult
	testErr     error
}

func (d *scriptedDeepSeek) Load(context.Context) (*deepseek.Snapshot, error) { return d.snapshot, d.loadErr }
func (d *scriptedDeepSeek) Save(context.Context, deepseek.Input) (*deepseek.Snapshot, error) {
	return d.snapshot, d.saveErr
}
func (d *scriptedDeepSeek) Clear(context.Context) (*deepseek.Snapshot, error) { return d.snapshot, d.clearErr }
func (d *scriptedDeepSeek) TestProvider(context.Context) (deepseek.ConnectionTestResult, error) {
	return d.testResult, d.testErr
}
func (d *scriptedDeepSeek) Validate(deepseek.Input) error { return d.validateErr }

func deepSeekProSnapshot() *deepseek.Snapshot {
	return &deepseek.Snapshot{
		GatewayRunning:                true,
		ProviderExists:                true,
		APIKeySet:                     true,
		APIKeyMasked:                  "configured",
		Configured:                    true,
		Active:                        true,
		SelectedModel:                 deepseek.ModelPro,
		DefaultModel:                  "pro",
		ReasoningEffort:               deepseek.ReasoningHigh,
		ReasoningExplicitlyConfigured: true,
		AllowedReasoningEfforts:       deepseek.AllowedReasoningEfforts(deepseek.ModelPro),
		RouteAlias:                    deepseek.RouteID,
		Pro: deepseek.ModelConfig{
			ModelID: deepseek.ModelPro, Reasoning: deepseek.ReasoningHigh,
			Supported: deepseek.AllowedReasoningEfforts(deepseek.ModelPro),
		},
		Flash: deepseek.ModelConfig{
			ModelID: deepseek.ModelFlash, Reasoning: deepseek.ReasoningLow,
			Supported: deepseek.AllowedReasoningEfforts(deepseek.ModelFlash),
		},
	}
}

func validDeepSeekInput() deepseek.Input {
	return deepseek.Input{DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "low"}
}

// setStatus mutates the scripted gateway controller's state under its lock.
func (c *scriptedController) setStatus(st gateway.State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = st
}

// ---- Load ----

func TestLoadDeepSeekSettingsGatewayNotRunning(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	// While stopped, Load reads the persisted config directly so the UI can
	// still show the saved key state; an empty config resolves none/missing.
	res := app.LoadDeepSeekSettings()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadDeepSeekSettings() = %#v, want an ok stopped-state snapshot", res)
	}
	if res.Value.DeepSeek == nil {
		t.Fatalf("LoadDeepSeekSettings() value=%#v, want a DeepSeek snapshot", res.Value)
	}
	if res.Value.DeepSeek.GatewayRunning || res.Value.DeepSeek.ProviderExists {
		t.Fatalf("stopped snapshot must not report running/existing: %#v", res.Value.DeepSeek)
	}
	if res.Value.DeepSeek.CredentialSource != "none" || res.Value.DeepSeek.CredentialState != "missing" {
		t.Fatalf("empty stopped config must resolve none/missing, got %q/%q", res.Value.DeepSeek.CredentialSource, res.Value.DeepSeek.CredentialState)
	}
}

func TestLoadDeepSeekSettingsStaleSessionCleared(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController { return &scriptedDeepSeek{snapshot: deepSeekProSnapshot()} },
	})
	defer app.shutdown(context.Background())

	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	if app.session == nil {
		t.Fatal("session not built after start")
	}

	// The gateway run no longer matches the retained session (external restart
	// with a new instance id): the stale session must be cleared, never reused.
	svc.setStatus(gateway.State{Status: gateway.StatusRunning, Addr: "127.0.0.1:38440", PID: os.Getpid(), InstanceID: "inst-OTHER"})

	res := app.LoadDeepSeekSettings()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadDeepSeekSettings() = %#v, want an ok stopped-state snapshot", res)
	}
	if res.Value.DeepSeek == nil || res.Value.DeepSeek.GatewayRunning {
		t.Fatalf("LoadDeepSeekSettings() value=%#v, want a stopped-state DeepSeek snapshot", res.Value)
	}
	if app.session != nil {
		t.Fatal("stale session not cleared")
	}

	// GatewayStatus never errors on a stale session (snapshot is svc-derived).
	gs := app.GatewayStatus()
	if !gs.OK || gs.Error != nil {
		t.Fatalf("GatewayStatus() = %#v, want ok with nil error", gs)
	}

	// Recovery: a proper stop+start rebuilds the session and Load works again.
	if !app.StopGateway(StopGatewayRequest{}).OK {
		t.Fatal("StopGateway() not ok")
	}
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() after stop not ok")
	}
	if app.session == nil {
		t.Fatal("session not rebuilt after restart")
	}
	if res := app.LoadDeepSeekSettings(); !res.OK {
		t.Fatalf("LoadDeepSeekSettings() after rebuild = %#v, want ok", res)
	}
}

// ---- factory per-operation creation ----

func TestDeepSeekFactoryRecreatesPerOperation(t *testing.T) {
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
		NewDeepSeek: func(address, controlToken string) deepSeekController {
			mu.Lock()
			calls = append(calls, factoryCall{address: address, controlToken: controlToken})
			mu.Unlock()
			return &scriptedDeepSeek{snapshot: deepSeekProSnapshot()}
		},
	})
	defer app.shutdown(context.Background())

	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	if res := app.LoadDeepSeekSettings(); !res.OK {
		t.Fatalf("LoadDeepSeekSettings() = %#v, want ok", res)
	}
	mu.Lock()
	got := append([]factoryCall(nil), calls...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("factory calls after first load = %d, want 1", len(got))
	}
	if got[0].controlToken != "token-1" {
		t.Fatalf("first control token = %q, want token-1", got[0].controlToken)
	}
	if got[0].address != "http://127.0.0.1:38440" {
		t.Fatalf("first address = %q, want http://127.0.0.1:38440 (scheme included)", got[0].address)
	}

	// A gateway restart rotates the control token; the next operation must build
	// a fresh controller with the new token, never reuse the old one.
	if !app.RestartGateway(RestartGatewayRequest{}).OK {
		t.Fatal("RestartGateway() not ok")
	}
	if res := app.LoadDeepSeekSettings(); !res.OK {
		t.Fatalf("LoadDeepSeekSettings() after restart = %#v, want ok", res)
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
	if got[0].controlToken == got[1].controlToken {
		t.Fatal("control token reused across gateway restart")
	}
}

// ---- Validate (gateway-independent) ----

func TestValidateDeepSeekSettingsWorksWhileGatewayStopped(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.ValidateDeepSeekSettings(deepseek.Input{
		APIKey: "sk-abcdefgh", DefaultModel: "flash", ProReasoning: "max", FlashReasoning: "xhigh",
	})
	if !res.OK || res.Value == nil {
		t.Fatalf("ValidateDeepSeekSettings() = %#v, want ok with snapshot", res)
	}
	if res.Error != nil {
		t.Fatalf("ValidateDeepSeekSettings() error = %#v, want nil", res.Error)
	}
	ds := res.Value.DeepSeek
	if ds == nil {
		t.Fatal("value.deepseek = nil")
	}
	if ds.GatewayRunning {
		t.Fatal("gatewayRunning = true, want false (gateway stopped)")
	}
	if !ds.APIKeySet {
		t.Fatal("apiKeySet = false, want true")
	}
	if ds.APIKeyMasked == "" {
		t.Fatal("apiKeyMasked = empty, want a mask form")
	}
	if ds.APIKeyMasked == "sk-abcdefgh" {
		t.Fatal("apiKeyMasked leaked plaintext")
	}
	if ds.SelectedModel != deepseek.ModelFlash {
		t.Fatalf("selectedModel = %q, want %q", ds.SelectedModel, deepseek.ModelFlash)
	}
	if ds.DefaultModel != "flash" {
		t.Fatalf("defaultModel = %q, want flash", ds.DefaultModel)
	}
	if ds.ReasoningEffort != deepseek.ReasoningMax {
		t.Fatalf("reasoningEffort = %q, want max (xhigh normalized)", ds.ReasoningEffort)
	}
	if ds.RouteAlias != deepseek.RouteID {
		t.Fatalf("routeAlias = %q, want %q", ds.RouteAlias, deepseek.RouteID)
	}
}

func TestValidateDeepSeekSettingsInvalidField(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.ValidateDeepSeekSettings(deepseek.Input{DefaultModel: "gpt-4", ProReasoning: "high", FlashReasoning: "low"})
	if res.OK {
		t.Fatal("ValidateDeepSeekSettings() ok=true, want false")
	}
	if res.Value != nil {
		t.Fatalf("ValidateDeepSeekSettings() value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "deepseek_validate_failed" {
		t.Fatalf("ValidateDeepSeekSettings() error = %#v, want deepseek_validate_failed", res.Error)
	}
	if res.Error.Field == nil || *res.Error.Field != "defaultModel" {
		t.Fatalf("ValidateDeepSeekSettings() field = %v, want defaultModel", res.Error.Field)
	}
	if res.Error.Code == "deepseek_gateway_not_running" {
		t.Fatal("validate leaked a gateway dependency (gateway is stopped)")
	}
}

// ---- Save ----

func TestSaveDeepSeekSettingsGatewayNotRunning(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.SaveDeepSeekSettings(validDeepSeekInput())
	if res.OK || res.Error == nil || res.Error.Code != "deepseek_gateway_not_running" {
		t.Fatalf("SaveDeepSeekSettings() = %#v, want deepseek_gateway_not_running", res)
	}
}

func TestSaveDeepSeekSettingsSuccess(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController { return &scriptedDeepSeek{snapshot: deepSeekProSnapshot()} },
		// The Live derivation fetches /config/effective over HTTP; inject a
		// scripted derivation so the unit test does not need a live gateway.
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.SaveDeepSeekSettings(validDeepSeekInput())
	if !res.OK || res.Value == nil {
		t.Fatalf("SaveDeepSeekSettings() = %#v, want ok", res)
	}
	if res.Error != nil {
		t.Fatalf("SaveDeepSeekSettings() error = %#v, want nil", res.Error)
	}
	if res.Value.DeepSeek == nil {
		t.Fatal("value.deepseek = nil")
	}
	if app.session == nil || !app.session.ConfigValid {
		t.Fatal("session.ConfigValid = false after successful save + refresh")
	}
}

func TestSaveDeepSeekSettingsSessionRefreshTotalFailure(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController { return &scriptedDeepSeek{snapshot: deepSeekProSnapshot()} },
		// The save succeeds against the (fake) gateway, but the live effective
		// config fetch fails: derivation must surface partial success, not OK.
		DeriveCodex: scriptedDeriver(config.Config{}, errors.New("effective fetch failed")),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.SaveDeepSeekSettings(validDeepSeekInput())
	if res.OK {
		t.Fatalf("SaveDeepSeekSettings() ok=true, want partial-success failure: %#v", res)
	}
	if res.Value != nil {
		t.Fatalf("SaveDeepSeekSettings() value = %#v, want nil on error (envelope contract)", res.Value)
	}
	e := res.Error
	if e == nil {
		t.Fatal("SaveDeepSeekSettings() error = nil")
	}
	if e.Code != "deepseek_saved_session_refresh_failed" {
		t.Fatalf("code = %q, want deepseek_saved_session_refresh_failed", e.Code)
	}
	if e.Stage != "refresh_session_config" {
		t.Fatalf("stage = %q, want refresh_session_config", e.Stage)
	}
	if !e.MutationStarted {
		t.Fatal("mutationStarted = false, want true (save succeeded)")
	}
	if !e.Retryable {
		t.Fatal("retryable = false, want true")
	}
	if e.Details["saved"] != true {
		t.Fatalf("details[saved] = %#v, want true", e.Details["saved"])
	}
	if e.Details["sessionConfigRefreshed"] != false {
		t.Fatalf("details[sessionConfigRefreshed] = %#v, want false", e.Details["sessionConfigRefreshed"])
	}
	if e.Details["requiresGatewayRestart"] != true {
		t.Fatalf("details[requiresGatewayRestart] = %#v, want true", e.Details["requiresGatewayRestart"])
	}
	if app.session == nil || app.session.ConfigValid {
		t.Fatal("session.ConfigValid = true, want false (stale)")
	}
}

// TestSaveDeepSeekSettingsConfigValidRecoversOnSubsequentSave pins the new
// source-of-truth behavior: a failed derivation marks ConfigValid=false, and a
// later successful save (with a working derivation) restores it. (The old
// YAML-file backoff retry is gone; config is always derived from the live
// effective store.)
func TestSaveDeepSeekSettingsConfigValidRecoversOnSubsequentSave(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	failThenSucceed := func() codexConfigDeriver {
		n := 0
		return func(context.Context, *gatewaySession, config.LoadOptions) (config.Config, error) {
			n++
			if n == 1 {
				return config.Config{}, errors.New("transient")
			}
			return config.Config{}, nil
		}
	}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController { return &scriptedDeepSeek{snapshot: deepSeekProSnapshot()} },
		DeriveCodex: failThenSucceed(),
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	if res := app.SaveDeepSeekSettings(validDeepSeekInput()); res.OK {
		t.Fatal("first save ok=true, want derive failure")
	}
	if app.session == nil || app.session.ConfigValid {
		t.Fatal("session.ConfigValid = true after first failed derivation, want false")
	}
	if res := app.SaveDeepSeekSettings(validDeepSeekInput()); !res.OK {
		t.Fatalf("second save = %#v, want ok after derivation recovers", res)
	}
	if app.session == nil || !app.session.ConfigValid {
		t.Fatal("session.ConfigValid = false after recovered derivation, want true")
	}
}

func TestSaveDeepSeekSettingsMutationError(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController {
			return &scriptedDeepSeek{
				snapshot: deepSeekProSnapshot(),
				saveErr:  &deepseek.ServiceError{Kind: deepseek.ServiceErrorKindSaveRejected, Message: "rejected", MutationStarted: true},
			}
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.SaveDeepSeekSettings(validDeepSeekInput())
	if res.OK {
		t.Fatal("SaveDeepSeekSettings() ok=true, want false")
	}
	if res.Value != nil {
		t.Fatalf("SaveDeepSeekSettings() value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "deepseek_save_failed" {
		t.Fatalf("SaveDeepSeekSettings() error = %#v, want deepseek_save_failed", res.Error)
	}
	if !res.Error.MutationStarted {
		t.Fatal("mutationStarted = false, want true (a mutation began before failure)")
	}
}

// ---- error mapping + envelope contract ----

func TestDeepSeekErrorMapping(t *testing.T) {
	field := func(s string) *string { return &s }
	tests := []struct {
		name         string
		err          error
		wantCode     string
		wantRetry    bool
		wantMutation bool
	}{
		{"invalid input", &deepseek.ServiceError{Kind: deepseek.ServiceErrorKindInvalidInput, Message: "bad", Field: field("defaultModel")}, "deepseek_validate_failed", false, false},
		{"api key required", &deepseek.ServiceError{Kind: deepseek.ServiceErrorKindAPIKeyRequired, Message: "need key"}, "deepseek_api_key_required", false, false},
		{"save rejected", &deepseek.ServiceError{Kind: deepseek.ServiceErrorKindSaveRejected, Message: "rejected", MutationStarted: true}, "deepseek_save_failed", false, true},
		{"revision conflict exceeded", &deepseek.ServiceError{Kind: deepseek.ServiceErrorKindRevisionConflictExceeded, Message: "conflict", MutationStarted: true, Retryable: true}, "deepseek_save_failed", true, true},
		{"verify failed", &deepseek.ServiceError{Kind: deepseek.ServiceErrorKindVerifyFailed, Message: "mismatch", MutationStarted: true}, "deepseek_save_failed", false, true},
		{"plain error", errors.New("boom"), "deepseek_load_failed", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := deepSeekError("Op", "load", "deepseek_load_failed", tt.err)
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

// TestDesktopEnvelopeAndNoSecretInSnapshot pins the envelope contract (OK=true
// → Error nil; OK=false → Value nil) and asserts secrets never reach the wire.
func TestDesktopEnvelopeAndNoSecretInSnapshot(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController { return &scriptedDeepSeek{snapshot: deepSeekProSnapshot()} },
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	ok := app.LoadDeepSeekSettings()
	if !ok.OK {
		t.Fatalf("LoadDeepSeekSettings() = %#v, want ok", ok)
	}
	if ok.Error != nil {
		t.Fatalf("ok result error = %#v, want nil", ok.Error)
	}
	data, err := json.Marshal(ok)
	if err != nil {
		t.Fatalf("Marshal(ok) error = %v", err)
	}
	s := string(data)
	for _, secret := range []string{"token-1", "server-tok", "sk-"} {
		if strings.Contains(s, secret) {
			t.Fatalf("ok snapshot leaked %q: %s", secret, s)
		}
	}

	bad := app.ValidateDeepSeekSettings(deepseek.Input{DefaultModel: "bogus", ProReasoning: "high", FlashReasoning: "low"})
	if bad.OK {
		t.Fatal("ValidateDeepSeekSettings(invalid) ok = true, want false")
	}
	bdata, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("Marshal(error) error = %v", err)
	}
	if strings.Contains(string(bdata), `"value"`) {
		t.Fatalf("error envelope carries a value: %s", bdata)
	}
}

// ---- TestDeepSeekConnection ----

func TestDeepSeekConnectionSuccess(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController {
			return &scriptedDeepSeek{testResult: deepseek.ConnectionTestResult{
				Success: true, Code: "ok", Message: "connection succeeded",
				Model: "claude-sonnet-20241022", Duration: "120ms",
			}}
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.TestDeepSeekConnection("op-1")
	if !res.OK || res.Error != nil {
		t.Fatalf("TestDeepSeekConnection() = %#v, want ok with nil error", res)
	}
	v := res.Value
	if v == nil {
		t.Fatal("value = nil")
	}
	if v.ConnectionTestOperationID != "op-1" {
		t.Fatalf("operationId = %q, want op-1", v.ConnectionTestOperationID)
	}
	if v.ConnectionTest == nil {
		t.Fatal("result = nil")
	}
	if !v.ConnectionTest.OK || v.ConnectionTest.Code != "ok" {
		t.Fatalf("result = %+v, want ok/code ok", *v.ConnectionTest)
	}
	if v.ConnectionTest.Message != "connection succeeded" || v.ConnectionTest.Model != "claude-sonnet-20241022" {
		t.Fatalf("result = %+v, want message/model echoed", *v.ConnectionTest)
	}
	if !v.ConnectionGatewayLeftRunning {
		t.Fatal("gatewayLeftRunning = false, want true (probe must not stop the gateway)")
	}
	if v.ConnectionGatewaySnapshot == nil {
		t.Fatal("gatewaySnapshot = nil, want a live snapshot")
	}
	if v.ConnectionTestWarning == nil {
		t.Fatal("warning = nil, want an empty warning slot")
	}

	// The structured DTO must never carry a key, a token, or a raw error body.
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal error = %v", err)
	}
	for _, forbidden := range []string{"sk-", "token-1", "Bearer", "x-api-key", "invalid api key"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("connection test result leaked %q: %s", forbidden, data)
		}
	}
}

func TestDeepSeekConnectionGatewayNotRunning(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.TestDeepSeekConnection("op-1")
	if res.OK {
		t.Fatalf("TestDeepSeekConnection() ok=true while stopped, want false")
	}
	if res.Value != nil {
		t.Fatalf("value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "deepseek_gateway_not_running" {
		t.Fatalf("error = %#v, want deepseek_gateway_not_running", res.Error)
	}
}

func TestDeepSeekConnectionTransportError(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		NewDeepSeek: func(_, _ string) deepSeekController {
			return &scriptedDeepSeek{testErr: errors.New("management api unreachable")}
		},
	})
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.TestDeepSeekConnection("op-1")
	if res.OK {
		t.Fatal("TestDeepSeekConnection() ok=true, want false")
	}
	if res.Value != nil {
		t.Fatalf("value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "deepseek_test_failed" {
		t.Fatalf("error = %#v, want deepseek_test_failed", res.Error)
	}
	if strings.Contains(res.Error.Message, "management api unreachable") {
		t.Fatalf("error message leaked transport detail: %q", res.Error.Message)
	}
}
