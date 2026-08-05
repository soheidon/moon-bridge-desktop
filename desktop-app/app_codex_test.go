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
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/service/codexlauncher"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/gateway"
)

// scriptedCodex is a codexController fake. The zero value reports an empty
// status; a successful Launch returns a running state, a successful Stop a
// stopped state. All calls are recorded.
type scriptedCodex struct {
	mu          sync.Mutex
	status      codexlauncher.State
	launchFn    func(ctx context.Context, opts codexlauncher.LaunchOptions) (codexlauncher.State, error)
	stopFn      func(ctx context.Context, reason codexlauncher.StopReason) (codexlauncher.State, error)
	launchCalls int
	stopCalls   int
	stopReasons []codexlauncher.StopReason
	lastOpts    codexlauncher.LaunchOptions
}

func (c *scriptedCodex) Launch(ctx context.Context, opts codexlauncher.LaunchOptions) (codexlauncher.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launchCalls++
	c.lastOpts = opts
	if c.launchFn != nil {
		return c.launchFn(ctx, opts)
	}
	st := codexlauncher.State{Status: codexlauncher.StatusRunning, PID: 4242, CodexHome: opts.CodexHome}
	c.status = st
	return st, nil
}

func (c *scriptedCodex) Stop(ctx context.Context, reason codexlauncher.StopReason) (codexlauncher.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopCalls++
	c.stopReasons = append(c.stopReasons, reason)
	if c.stopFn != nil {
		return c.stopFn(ctx, reason)
	}
	st := codexlauncher.State{Status: codexlauncher.StatusStopped, StopReason: reason}
	c.status = st
	return st, nil
}

func (c *scriptedCodex) Restart(ctx context.Context, opts codexlauncher.LaunchOptions) (codexlauncher.State, error) {
	if _, err := c.Stop(ctx, codexlauncher.StopReasonGraceful); err != nil {
		return codexlauncher.State{}, err
	}
	return c.Launch(ctx, opts)
}

func (c *scriptedCodex) Status() codexlauncher.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *scriptedCodex) reasons() []codexlauncher.StopReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]codexlauncher.StopReason(nil), c.stopReasons...)
}

func newCodexApp(t *testing.T, codex codexController) *App {
	t.Helper()
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	return NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		Codex:       codex,
		// Launch now derives codex config from the live effective store over
		// HTTP; inject a scripted derivation so the unit test needs no gateway.
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
	})
}

func TestLaunchCodexGatewayNotRunning(t *testing.T) {
	codex := &scriptedCodex{}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())

	res := app.LaunchCodex(LaunchCodexRequest{ProjectDirectory: `C:\work`})
	if res.OK {
		t.Fatalf("LaunchCodex() ok = true, want false: %#v", res)
	}
	if res.Value != nil {
		t.Fatalf("LaunchCodex() value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "codex_gateway_not_running" {
		t.Fatalf("LaunchCodex() error = %#v, want codex_gateway_not_running", res.Error)
	}
	if res.Error.Stage != "gateway_check" {
		t.Fatalf("LaunchCodex() stage = %q, want gateway_check", res.Error.Stage)
	}
	if codex.launchCalls != 0 {
		t.Fatalf("launch calls = %d, want 0", codex.launchCalls)
	}
}

func TestLaunchCodexSuccessAndDerivation(t *testing.T) {
	codex := &scriptedCodex{}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	project := filepath.Join(t.TempDir(), "my project (dev) v2")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}

	res := app.LaunchCodex(LaunchCodexRequest{ProjectDirectory: project})
	if !res.OK || res.Value == nil {
		t.Fatalf("LaunchCodex() = %#v, want ok", res)
	}
	if res.Error != nil {
		t.Fatalf("LaunchCodex() error = %#v, want nil", res.Error)
	}
	if res.Value.Codex == nil || res.Value.Codex.Status != "running" {
		t.Fatalf("codex state = %#v, want running", res.Value.Codex)
	}

	opts := codex.lastOpts
	if opts.CodexHome != resolveCodexHome() {
		t.Fatalf("codexHome = %q, want %q", opts.CodexHome, resolveCodexHome())
	}
	if opts.ModelAlias != deepseek.RouteID {
		t.Fatalf("modelAlias = %q, want %q", opts.ModelAlias, deepseek.RouteID)
	}
	if want := "http://" + app.session.Address + "/v1"; opts.BaseURL != want {
		t.Fatalf("baseURL = %q, want %q", opts.BaseURL, want)
	}
	if opts.AuthToken != app.session.ServerToken {
		t.Fatal("opts.AuthToken does not match the session server token")
	}
	if opts.ServerCfg.AuthToken != app.session.ServerToken {
		t.Fatal("opts.ServerCfg.AuthToken does not match the session server token")
	}
	if opts.ProjectDirectory != project {
		t.Fatalf("projectDirectory = %q, want %q", opts.ProjectDirectory, project)
	}
}

func TestLaunchCodexSessionStale(t *testing.T) {
	codex := &scriptedCodex{}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	app.session.ConfigValid = false // saved settings could not be re-read

	res := app.LaunchCodex(LaunchCodexRequest{ProjectDirectory: `C:\work`})
	if res.OK {
		t.Fatalf("LaunchCodex() ok = true, want stale-session failure: %#v", res)
	}
	if res.Error == nil || res.Error.Code != "codex_gateway_session_stale" {
		t.Fatalf("LaunchCodex() error = %#v, want codex_gateway_session_stale", res.Error)
	}
	if res.Error.Stage != "config" {
		t.Fatalf("LaunchCodex() stage = %q, want config", res.Error.Stage)
	}
	if codex.launchCalls != 0 {
		t.Fatalf("launch calls = %d, want 0 (no launch against stale config)", codex.launchCalls)
	}
}

func TestStopCodexGatewayIndependent(t *testing.T) {
	codex := &scriptedCodex{}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}
	if !app.StopGateway(StopGatewayRequest{}).OK {
		t.Fatal("StopGateway() not ok")
	}
	if app.session != nil {
		t.Fatal("session should be cleared on gateway stop")
	}

	res := app.StopCodex(StopCodexRequest{})
	if !res.OK {
		t.Fatalf("StopCodex() = %#v, want ok while gateway is stopped", res)
	}
	if reasons := codex.reasons(); len(reasons) != 1 || reasons[0] != codexlauncher.StopReasonGraceful {
		t.Fatalf("stop reasons = %v, want [graceful]", reasons)
	}
}

func TestStopCodexErrorCarriesCodexState(t *testing.T) {
	codex := &scriptedCodex{
		stopFn: func(context.Context, codexlauncher.StopReason) (codexlauncher.State, error) {
			return codexlauncher.State{Status: codexlauncher.StatusError, Error: "nope"},
				&codexlauncher.Error{Kind: codexlauncher.KindStopFailed, Message: "did not stop"}
		},
	}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())

	res := app.StopCodex(StopCodexRequest{})
	if res.OK {
		t.Fatal("StopCodex() ok = true, want false")
	}
	if res.Value != nil {
		t.Fatalf("StopCodex() value = %#v, want nil", res.Value)
	}
	e := res.Error
	if e == nil || e.Code != "codex_stop_failed" {
		t.Fatalf("StopCodex() error = %#v, want codex_stop_failed", e)
	}
	st, ok := e.Details["codexState"].(*CodexState)
	if !ok {
		t.Fatalf("details[codexState] = %#v, want *CodexState", e.Details["codexState"])
	}
	if st.Status != CodexStatus("error") {
		t.Fatalf("details[codexState].status = %q, want error", st.Status)
	}
}

func TestCodexStatusGatewayIndependent(t *testing.T) {
	codex := &scriptedCodex{status: codexlauncher.State{Status: codexlauncher.StatusIdle}}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())

	res := app.CodexStatus()
	if !res.OK || res.Value == nil {
		t.Fatalf("CodexStatus() = %#v, want ok", res)
	}
	if res.Value.Codex == nil {
		t.Fatal("value.codex = nil")
	}
	if res.Value.Codex.Status != "idle" {
		t.Fatalf("codex status = %q, want idle", res.Value.Codex.Status)
	}
}

func TestRestartCodexStopsThenLaunches(t *testing.T) {
	codex := &scriptedCodex{}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.RestartCodex(LaunchCodexRequest{ProjectDirectory: `C:\work`})
	if !res.OK || res.Value == nil {
		t.Fatalf("RestartCodex() = %#v, want ok", res)
	}
	if reasons := codex.reasons(); len(reasons) != 1 || reasons[0] != codexlauncher.StopReasonGraceful {
		t.Fatalf("stop reasons = %v, want [graceful]", reasons)
	}
	if codex.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", codex.launchCalls)
	}
}

func TestShutdownStopsCodexWithShutdownReason(t *testing.T) {
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	codex := &scriptedCodex{}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		Codex:       codex,
	})
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	app.shutdown(context.Background())

	if reasons := codex.reasons(); len(reasons) != 1 || reasons[0] != codexlauncher.StopReasonShutdown {
		t.Fatalf("stop reasons = %v, want [shutdown]", reasons)
	}
	if app.svc.Status().Status != gateway.StatusStopped {
		t.Fatalf("gateway status after shutdown = %v, want stopped", app.svc.Status().Status)
	}
	if app.session != nil {
		t.Fatal("session not cleared on shutdown")
	}
}

func TestCodexBindingsClosed(t *testing.T) {
	codex := &scriptedCodex{}
	app := newCodexApp(t, codex)
	app.shutdown(context.Background())

	bindings := []func() DesktopCommandResult{
		func() DesktopCommandResult { return app.LaunchCodex(LaunchCodexRequest{}) },
		func() DesktopCommandResult { return app.StopCodex(StopCodexRequest{}) },
		func() DesktopCommandResult { return app.RestartCodex(LaunchCodexRequest{}) },
		func() DesktopCommandResult { return app.CodexStatus() },
	}
	stopBaseline := codex.stopCalls
	for i, call := range bindings {
		res := call()
		if res.OK {
			t.Fatalf("binding %d ok = true after shutdown, want false", i)
		}
		if res.Value != nil {
			t.Fatalf("binding %d value = %#v, want nil", i, res.Value)
		}
		if res.Error == nil || res.Error.Code != "codex_host_closed" {
			t.Fatalf("binding %d error = %#v, want codex_host_closed", i, res.Error)
		}
		if res.Error.Stage != "host" {
			t.Fatalf("binding %d stage = %q, want host", i, res.Error.Stage)
		}
	}
	if codex.stopCalls != stopBaseline {
		t.Fatalf("post-shutdown bindings called codex.Stop: %d → %d", stopBaseline, codex.stopCalls)
	}
	if codex.launchCalls != 0 {
		t.Fatalf("codex.Launch calls = %d after shutdown, want 0", codex.launchCalls)
	}
}

func TestCodexSnapshotOmitsSecrets(t *testing.T) {
	codex := &scriptedCodex{}
	app := newCodexApp(t, codex)
	defer app.shutdown(context.Background())
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	res := app.LaunchCodex(LaunchCodexRequest{ProjectDirectory: `C:\work`})
	if !res.OK {
		t.Fatalf("LaunchCodex() = %#v, want ok", res)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal(result) error = %v", err)
	}
	s := string(data)
	for _, secret := range []string{"server-tok", "token-1", "sk-"} {
		if strings.Contains(s, secret) {
			t.Fatalf("codex snapshot leaked %q: %s", secret, s)
		}
	}
}

func TestCodexErrorMapping(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantCode  string
		retryable bool
	}{
		{"not found", &codexlauncher.Error{Kind: codexlauncher.KindNotFound, Message: "x"}, "codex_not_found", true},
		{"invalid executable", &codexlauncher.Error{Kind: codexlauncher.KindInvalidExecutable, Message: "x"}, "codex_invalid_executable", true},
		{"version probe failed", &codexlauncher.Error{Kind: codexlauncher.KindVersionProbeFailed, Message: "x"}, "codex_version_probe_failed", true},
		{"route not found", &codexlauncher.Error{Kind: codexlauncher.KindRouteNotFound, Message: "x"}, "codex_route_not_found", true},
		{"config generation failed", &codexlauncher.Error{Kind: codexlauncher.KindConfigGenerationFailed, Message: "x"}, "codex_config_generation_failed", true},
		{"config publish failed", &codexlauncher.Error{Kind: codexlauncher.KindConfigPublishFailed, Message: "x"}, "codex_config_publish_failed", true},
		{"already running", &codexlauncher.Error{Kind: codexlauncher.KindAlreadyRunning, Message: "x"}, "codex_already_running", false},
		{"start failed", &codexlauncher.Error{Kind: codexlauncher.KindStartFailed, Message: "x"}, "codex_start_failed", true},
		{"project invalid", &codexlauncher.Error{Kind: codexlauncher.KindProjectInvalid, Message: "x"}, "codex_project_invalid", true},
		{"project not found", &codexlauncher.Error{Kind: codexlauncher.KindProjectNotFound, Message: "x"}, "codex_project_not_found", true},
		{"project not directory", &codexlauncher.Error{Kind: codexlauncher.KindProjectNotDirectory, Message: "x"}, "codex_project_not_directory", true},
		{"stop failed", &codexlauncher.Error{Kind: codexlauncher.KindStopFailed, Message: "x"}, "codex_stop_failed", false},
		{"plain error", errors.New("boom"), "codex_start_failed", true},
		{"canceled", context.Canceled, "codex_host_closed", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := codexError("Op", "launch", tt.err)
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
			if res.Error.Retryable != tt.retryable {
				t.Fatalf("retryable = %v, want %v", res.Error.Retryable, tt.retryable)
			}
		})
	}
}

func TestShutdownInterruptsInflightCodexLaunch(t *testing.T) {
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	started := make(chan struct{})
	codex := &scriptedCodex{
		launchFn: func(ctx context.Context, _ codexlauncher.LaunchOptions) (codexlauncher.State, error) {
			close(started)
			<-ctx.Done()
			return codexlauncher.State{}, context.Canceled
		},
	}
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		Codex:       codex,
		// The launch under test blocks in the codex launcher, not in the config
		// derivation; inject a scripted deriver so the HTTP effective fetch does
		// not block before the codex launchFn is reached.
		DeriveCodex: scriptedDeriver(config.Config{}, nil),
	})
	if !app.StartGateway(StartGatewayRequest{}).OK {
		t.Fatal("StartGateway() not ok")
	}

	launchDone := make(chan DesktopCommandResult, 1)
	go func() {
		launchDone <- app.LaunchCodex(LaunchCodexRequest{ProjectDirectory: `C:\work`})
	}()
	<-started // Launch is blocked inside codexMu, holding operationMu

	shutdownDone := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown() did not return while a codex launch was in flight")
	}

	select {
	case res := <-launchDone:
		if res.OK {
			t.Fatalf("LaunchCodex() = %#v, want canceled failure", res)
		}
		if res.Error == nil || res.Error.Code != "codex_host_closed" {
			t.Fatalf("LaunchCodex() error = %#v, want codex_host_closed", res.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LaunchCodex() did not return after shutdown canceled its context")
	}
}
