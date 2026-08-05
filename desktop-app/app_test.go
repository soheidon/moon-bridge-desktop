package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/service/gateway"
)

func TestRoundTripReturnsConcreteSuccessEnvelope(t *testing.T) {
	result := (&App{}).RoundTrip(RoundTripRequest{Payload: " hello "})
	if !result.OK {
		t.Fatalf("RoundTrip() ok = false, error = %#v", result.Error)
	}
	if result.Value == nil || result.Value.Payload != "hello" {
		t.Fatalf("RoundTrip() value = %#v, want trimmed payload", result.Value)
	}
	if result.Error != nil {
		t.Fatalf("RoundTrip() error = %#v, want nil", result.Error)
	}
}

func TestRoundTripPreservesStructuredError(t *testing.T) {
	result := (&App{}).RoundTrip(RoundTripRequest{})
	if result.OK {
		t.Fatal("RoundTrip() ok = true, want false")
	}
	if result.Value != nil {
		t.Fatalf("RoundTrip() value = %#v, want nil", result.Value)
	}
	if result.Error == nil || result.Error.Code != "invalid_payload" {
		t.Fatalf("RoundTrip() error = %#v, want invalid_payload", result.Error)
	}
}

func TestCommandErrorCarriesFullContract(t *testing.T) {
	err := (&App{}).RoundTrip(RoundTripRequest{}).Error
	if err == nil {
		t.Fatal("RoundTrip() error = nil, want non-nil")
	}
	if err.Operation != "RoundTrip" {
		t.Errorf("error.operation = %q, want RoundTrip", err.Operation)
	}
	if err.Stage != "validation" {
		t.Errorf("error.stage = %q, want validation", err.Stage)
	}
	if err.Code != "invalid_payload" {
		t.Errorf("error.code = %q, want invalid_payload", err.Code)
	}
	if err.Message == "" {
		t.Error("error.message = empty, want non-empty")
	}
	if err.Field != nil {
		t.Errorf("error.field = %v, want nil", *err.Field)
	}
	if err.Retryable {
		t.Error("error.retryable = true, want false")
	}
	if err.MutationStarted {
		t.Error("error.mutationStarted = true, want false")
	}
	if err.GatewayLeftRunning {
		t.Error("error.gatewayLeftRunning = true, want false")
	}
	if err.GatewaySnapshot != nil {
		t.Errorf("error.gatewaySnapshot = %#v, want nil", err.GatewaySnapshot)
	}
	if err.Details != nil {
		t.Errorf("error.details = %#v, want nil", err.Details)
	}
}

// ---- gateway binding helpers ----

// m5ConfigYAML is the minimal CaptureAnthropic config used by the integration
// test. addr :0 materializes to the actual bound loopback port.
const m5ConfigYAML = `mode: CaptureAnthropic
server:
  addr: 127.0.0.1:0
  auth_token: server-tok
proxy:
  anthropic:
    base_url: https://api.example.invalid
    api_key: test-key
    version: "2023-06-01"
`

func writeCaptureAnthropicConfig(t *testing.T, dir string) string {
	t.Helper()
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte(m5ConfigYAML), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	return configPath
}

type scriptedController struct {
	mu         sync.Mutex
	state      gateway.State
	startFn    func(ctx context.Context, opts gateway.StartOptions) (gateway.State, error)
	stopFn     func(ctx context.Context) error
	startCalls int
	stopCalls  int
}

func newScriptedController(state gateway.State) *scriptedController {
	return &scriptedController{state: state}
}

func (c *scriptedController) Start(ctx context.Context, opts gateway.StartOptions) (gateway.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startCalls++
	if c.startFn == nil {
		st := gateway.State{
			Status:     gateway.StatusRunning,
			Addr:       "127.0.0.1:38440",
			PID:        os.Getpid(),
			InstanceID: opts.InstanceID,
		}
		c.state = st
		return st, nil
	}
	return c.startFn(ctx, opts)
}

func (c *scriptedController) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopCalls++
	if c.stopFn != nil {
		return c.stopFn(ctx)
	}
	c.state = gateway.State{Status: gateway.StatusStopped}
	return nil
}

func (c *scriptedController) Status() gateway.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

type gatewayIdentity struct {
	instanceID string
	token      string
}

// sequenceIdentity yields inst-1/token-1, inst-2/token-2, ... and records them.
func sequenceIdentity() (func() (string, string), *[]gatewayIdentity) {
	ids := &[]gatewayIdentity{}
	return func() (string, string) {
		n := len(*ids) + 1
		id := gatewayIdentity{instanceID: fmt.Sprintf("inst-%d", n), token: fmt.Sprintf("token-%d", n)}
		*ids = append(*ids, id)
		return id.instanceID, id.token
	}, ids
}

func fixedIdentity(instanceID, token string) func() (string, string) {
	return func() (string, string) { return instanceID, token }
}

func noopEmit(string, any) {}

// scriptedDeriver returns a codexConfigDeriver that always produces the given
// config and error — used to test Save/Launch without a live gateway HTTP fetch.
func scriptedDeriver(cfg config.Config, err error) codexConfigDeriver {
	return func(context.Context, *gatewaySession, config.LoadOptions) (config.Config, error) {
		return cfg, err
	}
}

type eventRecord struct {
	name    string
	payload *GatewaySnapshot
}

type eventRecorder struct {
	mu     sync.Mutex
	events []eventRecord
}

func (r *eventRecorder) emit(name string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var snap *GatewaySnapshot
	if payload != nil {
		snap = payload.(*GatewaySnapshot)
	}
	r.events = append(r.events, eventRecord{name: name, payload: snap})
}

func (r *eventRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *eventRecorder) states() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		if e.payload != nil {
			out[i] = e.payload.State
		}
	}
	return out
}

func pollUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// ---- error mapping ----

func TestStartGatewayErrorMapping(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())

	tests := []struct {
		name      string
		state     gateway.Status
		startFn   func(ctx context.Context, opts gateway.StartOptions) (gateway.State, error)
		wantCode  string
		wantStage string
		retryable bool
	}{
		{
			name:      "already running",
			state:     gateway.StatusRunning,
			wantCode:  "gateway_already_running",
			wantStage: "starting",
		},
		{
			name:      "identity required",
			startFn:   func(context.Context, gateway.StartOptions) (gateway.State, error) { return gateway.State{}, gateway.ErrDesktopModeRequiresIdentity },
			wantCode:  "gateway_identity_required",
			wantStage: "validating_config",
		},
		{
			name:      "loopback required",
			startFn:   func(context.Context, gateway.StartOptions) (gateway.State, error) { return gateway.State{}, gateway.ErrDesktopModeRequiresLoopback },
			wantCode:  "gateway_loopback_required",
			wantStage: "validating_config",
		},
		{
			name:      "start canceled",
			startFn:   func(context.Context, gateway.StartOptions) (gateway.State, error) { return gateway.State{}, gateway.ErrStartCanceled },
			wantCode:  "gateway_start_canceled",
			wantStage: "starting",
		},
		{
			name:      "start failed",
			startFn:   func(context.Context, gateway.StartOptions) (gateway.State, error) { return gateway.State{}, fmt.Errorf("boom") },
			wantCode:  "gateway_start_failed",
			wantStage: "starting",
			retryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newScriptedController(gateway.State{Status: tt.state})
			svc.startFn = tt.startFn
			app := NewApp(AppOptions{
				Service:     svc,
				NewIdentity: fixedIdentity("inst-x", "tok-x"),
				ConfigPath:  cfg,
				EmitEvents:  noopEmit,
			})
			defer app.shutdown(context.Background())

			res := app.StartGateway(StartGatewayRequest{})
			if res.OK {
				t.Fatal("StartGateway() ok = true, want false")
			}
			if res.Value != nil {
				t.Fatalf("StartGateway() value = %#v, want nil", res.Value)
			}
			if res.Error == nil {
				t.Fatal("StartGateway() error = nil, want non-nil")
			}
			if res.Error.Code != tt.wantCode {
				t.Fatalf("error.code = %q, want %q", res.Error.Code, tt.wantCode)
			}
			if res.Error.Stage != tt.wantStage {
				t.Fatalf("error.stage = %q, want %q", res.Error.Stage, tt.wantStage)
			}
			if res.Error.Retryable != tt.retryable {
				t.Fatalf("error.retryable = %v, want %v", res.Error.Retryable, tt.retryable)
			}
			if res.Error.GatewaySnapshot == nil {
				t.Fatal("error.gatewaySnapshot = nil, want current snapshot")
			}
		})
	}
}

func TestStartGatewayConfigLoadFailure(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte("mode: [unclosed"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-x", "tok-x"),
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.StartGateway(StartGatewayRequest{})
	if res.OK || res.Error == nil {
		t.Fatalf("StartGateway() = %#v, want config load failure", res)
	}
	if res.Error.Code != "gateway_config_load_failed" {
		t.Fatalf("error.code = %q, want gateway_config_load_failed", res.Error.Code)
	}
	if res.Error.Stage != "loading_config" {
		t.Fatalf("error.stage = %q, want loading_config", res.Error.Stage)
	}
}

func TestStopGatewayErrorMapping(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())

	timeoutSvc := newScriptedController(gateway.State{Status: gateway.StatusRunning})
	timeoutSvc.stopFn = func(context.Context) error { return context.DeadlineExceeded }
	app := NewApp(AppOptions{
		Service:     timeoutSvc,
		NewIdentity: fixedIdentity("inst-x", "tok-x"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	res := app.StopGateway(StopGatewayRequest{})
	if res.OK || res.Error == nil {
		t.Fatalf("StopGateway() = %#v, want timeout failure", res)
	}
	if res.Error.Code != "gateway_stop_timeout" {
		t.Fatalf("error.code = %q, want gateway_stop_timeout", res.Error.Code)
	}
	if res.Error.Stage != "stopping" {
		t.Fatalf("error.stage = %q, want stopping", res.Error.Stage)
	}
	if !res.Error.GatewayLeftRunning {
		t.Fatal("error.gatewayLeftRunning = false, want true (measured while still running)")
	}
	app.shutdown(context.Background())

	failSvc := newScriptedController(gateway.State{Status: gateway.StatusRunning})
	failSvc.stopFn = func(context.Context) error { return fmt.Errorf("stop boom") }
	app2 := NewApp(AppOptions{
		Service:     failSvc,
		NewIdentity: fixedIdentity("inst-x", "tok-x"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	defer app2.shutdown(context.Background())
	res2 := app2.StopGateway(StopGatewayRequest{})
	if res2.OK || res2.Error == nil {
		t.Fatalf("StopGateway() = %#v, want failure", res2)
	}
	if res2.Error.Code != "gateway_stop_failed" {
		t.Fatalf("error.code = %q, want gateway_stop_failed", res2.Error.Code)
	}
	if res2.Error.GatewayLeftRunning {
		t.Fatal("error.gatewayLeftRunning = true, want false")
	}
}

// ---- contracts ----

func TestClosedAppRejectsAllBindings(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-x", "tok-x"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})

	app.shutdown(context.Background())

	bindings := []func() GatewayCommandResult{
		func() GatewayCommandResult { return app.StartGateway(StartGatewayRequest{}) },
		func() GatewayCommandResult { return app.StopGateway(StopGatewayRequest{}) },
		func() GatewayCommandResult { return app.RestartGateway(RestartGatewayRequest{}) },
		func() GatewayCommandResult { return app.GatewayStatus() },
	}
	for i, call := range bindings {
		res := call()
		if res.OK {
			t.Fatalf("binding %d ok = true after shutdown, want false", i)
		}
		if res.Error == nil || res.Error.Code != "gateway_host_closed" {
			t.Fatalf("binding %d error = %#v, want gateway_host_closed", i, res.Error)
		}
		if res.Error.Stage != "host" {
			t.Fatalf("binding %d error.stage = %q, want host", i, res.Error.Stage)
		}
	}
	if svc.startCalls != 0 {
		t.Fatalf("Start() calls = %d after shutdown, want 0 (no new gateway starts)", svc.startCalls)
	}
}

func TestSnapshotClearsRuntimeFieldsOnStop(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	rec := &eventRecorder{}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  rec.emit,
	})
	defer app.shutdown(context.Background())

	start := app.StartGateway(StartGatewayRequest{})
	if !start.OK || start.Value == nil {
		t.Fatalf("StartGateway() = %#v, want ok", start)
	}
	if start.Value.State != "running" || start.Value.Address == "" || start.Value.PID == nil || start.Value.InstanceID == nil {
		t.Fatalf("running snapshot = %#v, want populated runtime fields", start.Value)
	}

	stop := app.StopGateway(StopGatewayRequest{})
	if !stop.OK || stop.Value == nil {
		t.Fatalf("StopGateway() = %#v, want ok", stop)
	}
	if stop.Value.State != "stopped" {
		t.Fatalf("stopped state = %q, want stopped", stop.Value.State)
	}
	if stop.Value.Address != "" {
		t.Fatalf("stopped address = %q, want empty", stop.Value.Address)
	}
	if stop.Value.PID != nil {
		t.Fatalf("stopped pid = %v, want nil", *stop.Value.PID)
	}
	if stop.Value.InstanceID != nil {
		t.Fatalf("stopped instanceId = %v, want nil", *stop.Value.InstanceID)
	}
	// ConfigPath is the last successful start and survives the stop.
	if stop.Value.ConfigPath != cfg {
		t.Fatalf("stopped configPath = %q, want %q", stop.Value.ConfigPath, cfg)
	}
}

func TestConfigPathSeparation(t *testing.T) {
	validCfg := writeCaptureAnthropicConfig(t, t.TempDir())
	invalidCfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(invalidCfg, []byte("mode: [unclosed"), 0o600); err != nil {
		t.Fatalf("WriteFile(invalid config) error = %v", err)
	}
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	if got := app.GatewayStatus().Value.ConfigPath; got != "" {
		t.Fatalf("initial snapshot configPath = %q, want empty", got)
	}

	start := app.StartGateway(StartGatewayRequest{ConfigPath: validCfg})
	if !start.OK || start.Value == nil {
		t.Fatalf("StartGateway(valid) = %#v, want ok", start)
	}
	if start.Value.ConfigPath != validCfg {
		t.Fatalf("running configPath = %q, want %q", start.Value.ConfigPath, validCfg)
	}
	if !app.StopGateway(StopGatewayRequest{}).OK {
		t.Fatal("StopGateway() not ok")
	}

	// A config load failure must not clobber the previous successful path.
	fail := app.StartGateway(StartGatewayRequest{ConfigPath: invalidCfg})
	if fail.OK || fail.Error == nil || fail.Error.Code != "gateway_config_load_failed" {
		t.Fatalf("StartGateway(invalid) = %#v, want config load failure", fail)
	}
	if got := app.GatewayStatus().Value.ConfigPath; got != validCfg {
		t.Fatalf("configPath after failed start = %q, want previous %q", got, validCfg)
	}
}

func TestIdempotentStopDoesNotEmit(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	rec := &eventRecorder{}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  rec.emit,
	})
	defer app.shutdown(context.Background())

	first := app.StopGateway(StopGatewayRequest{})
	second := app.StopGateway(StopGatewayRequest{})
	if !first.OK || !second.OK {
		t.Fatalf("idempotent StopGateway() = %#v / %#v, want both ok", first, second)
	}
	if rec.count() != 0 {
		t.Fatalf("events = %v, want 0 for a no-op stop", rec.states())
	}
	if svc.stopCalls != 0 {
		t.Fatalf("svc.Stop() calls = %d, want 0 (App-level no-op)", svc.stopCalls)
	}
}

func TestRestartGatewayFromRunningEmitsStopAndStart(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	rec := &eventRecorder{}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  rec.emit,
	})
	defer app.shutdown(context.Background())

	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	if got := rec.states(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("events after start = %v, want [running]", got)
	}

	res := app.RestartGateway(RestartGatewayRequest{})
	if !res.OK || res.Value == nil {
		t.Fatalf("RestartGateway() = %#v, want ok", res)
	}
	got := rec.states()
	if len(got) != 3 {
		t.Fatalf("events after running restart = %v, want [running stopped running]", got)
	}
	if got[1] != "stopped" || got[2] != "running" {
		t.Fatalf("restart transition events = %v, want stop then start", got)
	}
}

// ---- shutdown interrupt ----

type blockingController struct {
	mu      sync.Mutex
	state   gateway.State
	started chan struct{}
}

func newBlockingController() *blockingController {
	return &blockingController{
		state:   gateway.State{Status: gateway.StatusStopped},
		started: make(chan struct{}),
	}
}

func (b *blockingController) Start(ctx context.Context, opts gateway.StartOptions) (gateway.State, error) {
	b.mu.Lock()
	b.state = gateway.State{Status: gateway.StatusStarting, PID: os.Getpid(), InstanceID: opts.InstanceID}
	b.mu.Unlock()
	close(b.started)
	<-ctx.Done() // block while holding the App's operationMu
	b.mu.Lock()
	b.state = gateway.State{Status: gateway.StatusStopped}
	b.mu.Unlock()
	return b.state, gateway.ErrStartCanceled
}

func (b *blockingController) Stop(context.Context) error { return nil }

func (b *blockingController) Status() gateway.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func TestShutdownInterruptsInflightStart(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newBlockingController()
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})

	done := make(chan GatewayCommandResult, 1)
	go func() {
		done <- app.StartGateway(StartGatewayRequest{})
	}()
	<-svc.started // Start is now blocked inside the App's operationMu

	shutdownDone := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown() did not return while a Start was in flight")
	}

	select {
	case res := <-done:
		if res.OK || res.Error == nil {
			t.Fatalf("StartGateway() = %#v, want canceled failure", res)
		}
		if res.Error.Code != "gateway_start_canceled" {
			t.Fatalf("error.code = %q, want gateway_start_canceled", res.Error.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartGateway() did not return after shutdown canceled its context")
	}

	if got := app.svc.Status().Status; got != gateway.StatusStopped {
		t.Fatalf("final Status() = %v, want stopped", got)
	}

	closed := app.StartGateway(StartGatewayRequest{})
	if closed.OK || closed.Error == nil || closed.Error.Code != "gateway_host_closed" {
		t.Fatalf("post-shutdown StartGateway() = %#v, want gateway_host_closed", closed)
	}
}

// ---- M5 integration flow ----

func TestGatewayM5Flow(t *testing.T) {
	configPath := writeCaptureAnthropicConfig(t, t.TempDir())
	newIdentity, identities := sequenceIdentity()
	rec := &eventRecorder{}
	app := NewApp(AppOptions{
		NewIdentity: newIdentity,
		ConfigPath:  configPath,
		EmitEvents:  rec.emit,
	})
	defer app.shutdown(context.Background())

	// 1. StartGateway → running in-process with the DI identity.
	start := app.StartGateway(StartGatewayRequest{})
	if !start.OK {
		t.Fatalf("StartGateway() = %#v, want ok", start)
	}
	snap := start.Value
	if snap == nil {
		t.Fatal("StartGateway() value = nil")
	}
	if snap.State != "running" {
		t.Fatalf("state = %q, want running", snap.State)
	}
	if snap.PID == nil || *snap.PID != os.Getpid() {
		t.Fatalf("pid = %v, want %d (in-process)", snap.PID, os.Getpid())
	}
	if snap.InstanceID == nil || *snap.InstanceID != "inst-1" {
		t.Fatalf("instanceId = %v, want inst-1", snap.InstanceID)
	}
	if !strings.HasPrefix(snap.Address, "127.0.0.1:") {
		t.Fatalf("address = %q, want loopback with materialized port", snap.Address)
	}
	if snap.ConfigPath != configPath {
		t.Fatalf("configPath = %q, want %q", snap.ConfigPath, configPath)
	}
	if got := rec.states(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("events after start = %v, want one running event", got)
	}

	// 2. A second Start while running fails fast.
	dup := app.StartGateway(StartGatewayRequest{})
	if dup.OK || dup.Error == nil || dup.Error.Code != "gateway_already_running" {
		t.Fatalf("duplicate StartGateway() = %#v, want gateway_already_running", dup)
	}

	// 3. The external management API is reachable over loopback with the
	// per-run control token, and reports this process as the listener.
	addr := snap.Address
	token := (*identities)[0].token
	body := authedStatusBody(t, addr, token)
	if got := body["status"]; got != "ok" {
		t.Fatalf("status = %#v, want ok", got)
	}
	if pid, ok := body["pid"].(float64); !ok || int(pid) != os.Getpid() {
		t.Fatalf("pid = %#v, want %d", body["pid"], os.Getpid())
	}

	resp := authedRequest(t, http.MethodPost, addr, token, "/api/v1/system/shutdown")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown status = %d, want 202", resp.StatusCode)
	}

	// 4. External shutdown stops the gateway; without watchRun no event is
	// pushed (state is only observable through GatewayStatus).
	pollUntil(t, 5*time.Second, func() bool {
		return app.GatewayStatus().Value.State == "stopped"
	})
	if got := rec.count(); got != 1 {
		t.Fatalf("events after external shutdown = %d, want 1 (no auto-push)", got)
	}

	// 5. StopGateway on an already-stopped gateway is an idempotent no-op.
	stop := app.StopGateway(StopGatewayRequest{})
	if !stop.OK || stop.Value == nil {
		t.Fatalf("StopGateway() = %#v, want ok", stop)
	}
	if stop.Value.Address != "" || stop.Value.PID != nil || stop.Value.InstanceID != nil {
		t.Fatalf("stopped snapshot = %#v, want cleared runtime fields", stop.Value)
	}
	if !app.StopGateway(StopGatewayRequest{}).OK {
		t.Fatal("second StopGateway() not ok")
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("events after idempotent stops = %d, want 1", got)
	}

	// 6. Restart from stopped starts a new instance in the same process.
	restart := app.RestartGateway(RestartGatewayRequest{})
	if !restart.OK || restart.Value == nil {
		t.Fatalf("RestartGateway() = %#v, want ok", restart)
	}
	if restart.Value.InstanceID == nil || *restart.Value.InstanceID != "inst-2" {
		t.Fatalf("instanceId after restart = %v, want inst-2", restart.Value.InstanceID)
	}
	if restart.Value.PID == nil || *restart.Value.PID != os.Getpid() {
		t.Fatalf("pid after restart = %v, want %d", restart.Value.PID, os.Getpid())
	}
	if got := rec.states(); len(got) != 2 || got[1] != "running" {
		t.Fatalf("events after restart-from-stopped = %v, want [running running]", got)
	}

	// 7. Shutdown stops the gateway and releases the port for re-bind.
	addr2 := restart.Value.Address
	app.shutdown(context.Background())
	if got := app.svc.Status().Status; got != gateway.StatusStopped {
		t.Fatalf("Status() after shutdown = %v, want stopped", got)
	}
	ln, err := net.Listen("tcp", addr2)
	if err != nil {
		t.Fatalf("re-bind %s after shutdown: %v", addr2, err)
	}
	ln.Close()

	if got := rec.states(); len(got) != 3 || got[2] != "stopped" {
		t.Fatalf("events after shutdown = %v, want trailing stopped event", got)
	}
}

// ---- helpers for the management API ----

func authedRequest(t *testing.T, method, addr, token, path string) *http.Response {
	t.Helper()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	req, err := http.NewRequest(method, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func authedStatusBody(t *testing.T, addr, token string) map[string]any {
	t.Helper()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/system/status", nil)
	if err != nil {
		t.Fatalf("NewRequest(status) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("status request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status response = %d %s, want 200", resp.StatusCode, body)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status body error = %v", err)
	}
	return body
}
