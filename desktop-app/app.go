package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"moonbridge/internal/config"
	"moonbridge/internal/service/app"
	"moonbridge/internal/service/gateway"
)

const (
	roundTripEvent     = "desktop:roundtrip"
	gatewayStatusEvent = "gateway-status"
)

type CommandError struct {
	Operation          string         `json:"operation"`
	Stage              string         `json:"stage"`
	Code               string         `json:"code"`
	Message            string         `json:"message"`
	Field              *string        `json:"field"`
	Retryable          bool           `json:"retryable"`
	MutationStarted    bool           `json:"mutationStarted"`
	GatewayLeftRunning bool           `json:"gatewayLeftRunning"`
	GatewaySnapshot    any            `json:"gatewaySnapshot"`
	Details            map[string]any `json:"details,omitempty"`
}

type RoundTripRequest struct {
	Payload string `json:"payload"`
}

type RoundTripValue struct {
	Payload string `json:"payload"`
}

type RoundTripResult struct {
	OK    bool            `json:"ok"`
	Value *RoundTripValue `json:"value,omitempty"`
	Error *CommandError   `json:"error,omitempty"`
}

// GatewaySnapshot is the JSON shape surfaced by the gateway bindings. State is
// the lifecycle status with the failed status mapped to "error". Address, PID
// and InstanceID are only populated while running; ConfigPath is the last
// successfully started config path and survives a stop.
type GatewaySnapshot struct {
	State      string  `json:"state"`      // stopped|starting|running|stopping|error
	Address    string  `json:"address"`    // running時のみ実Listenアドレス
	ConfigPath string  `json:"configPath"` // activeConfigPath（最後に成功した Start の path）のみ
	PID        *int    `json:"pid"`        // running時のみ
	InstanceID *string `json:"instanceId"` // running時のみ
	Error      *string `json:"error"`      // state==error 時のみ lastError
}

type StartGatewayRequest struct {
	ConfigPath string `json:"configPath"`
}

type StopGatewayRequest struct{}

type RestartGatewayRequest struct {
	ConfigPath string `json:"configPath"`
}

type GatewayCommandResult struct {
	OK    bool             `json:"ok"`
	Value *GatewaySnapshot `json:"value,omitempty"`
	Error *CommandError    `json:"error,omitempty"` // 既存 RoundTrip 契約の CommandError を再利用
}

// gatewayController is the subset of the gateway service the App drives. It is
// an interface so the App's shutdown behavior is unit testable without a real
// run. gateway.Service's public API is unchanged.
type gatewayController interface {
	Start(ctx context.Context, opts gateway.StartOptions) (gateway.State, error)
	Stop(ctx context.Context) error
	Status() gateway.State
}

type App struct {
	ctx              context.Context // Wails runtime ctx（startup で保存のみ）
	appCtx           context.Context // gateway run の親 ctx（NewApp で生成）
	cancel           context.CancelFunc
	operationMu      sync.Mutex // binding と shutdown を直列化
	svc              gatewayController
	configuredPath   string // AppOptions で指定された Start 候補
	activeConfigPath string // 最後に成功した Start の path（snapshot にのみ表示）
	newIdentity      func() (string, string)
	emitEvents       func(name string, payload any)
	closed           atomic.Bool
}

type AppOptions struct {
	Service     gatewayController              // nil → gateway.NewService(ServiceOptions{Errors: os.Stderr})
	NewIdentity func() (string, string)        // nil → gateway.NewDesktopIdentity
	ConfigPath  string                         // Start 候補。"" → 既定パス（lazy resolve）
	EmitEvents  func(name string, payload any) // nil → runtime.EventsEmit（a.ctx 非nil時のみ）
}

func NewApp(opts AppOptions) *App {
	svc := opts.Service
	if svc == nil {
		svc = gateway.NewService(gateway.ServiceOptions{Errors: os.Stderr})
	}
	newIdentity := opts.NewIdentity
	if newIdentity == nil {
		newIdentity = gateway.NewDesktopIdentity
	}
	appCtx, cancel := context.WithCancel(context.Background())
	a := &App{
		appCtx:         appCtx,
		cancel:         cancel,
		svc:            svc,
		configuredPath: opts.ConfigPath,
		newIdentity:    newIdentity,
		emitEvents:     opts.EmitEvents,
	}
	if a.emitEvents == nil {
		a.emitEvents = func(name string, payload any) {
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, name, payload)
			}
		}
	}
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) shutdown(context.Context) {
	if !a.closed.CompareAndSwap(false, true) {
		return // 多重 shutdown を防ぐ
	}
	a.cancel() // run 全体の親 context を cancel → 進行中 Start を中断、稼働中 Gateway の停止も開始

	a.operationMu.Lock()
	defer a.operationMu.Unlock()

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = a.svc.Stop(stopCtx) // cleanup 完了を待つ

	a.emitStatus()
}

func (a *App) RoundTrip(req RoundTripRequest) RoundTripResult {
	payload := strings.TrimSpace(req.Payload)
	if payload == "" {
		return RoundTripResult{
			OK: false,
			Error: &CommandError{
				Operation: "RoundTrip",
				Stage:     "validation",
				Code:      "invalid_payload",
				Message:   "payload must not be empty",
			},
		}
	}

	value := &RoundTripValue{Payload: payload}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, roundTripEvent, value)
	}
	return RoundTripResult{OK: true, Value: value}
}

func (a *App) roundTripEventName() string {
	return roundTripEvent
}

// StartGateway starts the gateway in-process from the given (or default) config.
func (a *App) StartGateway(req StartGatewayRequest) GatewayCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return a.closedResult("gateway.start")
	}
	return a.startGatewayLocked(req.ConfigPath)
}

// StopGateway stops a running gateway. It is idempotent and a no-op when the
// gateway is already stopped or failed (no status event is emitted).
func (a *App) StopGateway(StopGatewayRequest) GatewayCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return a.closedResult("gateway.stop")
	}
	return a.stopGatewayLocked()
}

// RestartGateway stops the running gateway (if any) and starts it again with a
// fresh instance identity.
func (a *App) RestartGateway(req RestartGatewayRequest) GatewayCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return a.closedResult("gateway.restart")
	}
	if r := a.stopGatewayLocked(); !r.OK {
		return r
	}
	return a.startGatewayLocked(req.ConfigPath)
}

// GatewayStatus returns the current gateway snapshot without mutating state.
func (a *App) GatewayStatus() GatewayCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return a.closedResult("gateway.status")
	}
	return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
}

func (a *App) startGatewayLocked(requestPath string) GatewayCommandResult {
	if st := a.svc.Status().Status; st == gateway.StatusStarting || st == gateway.StatusRunning || st == gateway.StatusStopping {
		return a.fail("gateway.start", "starting", "gateway_already_running", "gateway is already running", false, gateway.ErrAlreadyRunning)
	}

	configPath, err := a.resolveConfigPath(requestPath)
	if err != nil {
		return a.fail("gateway.start", "config_init", "gateway_config_init_failed", "unable to resolve config path", false, err)
	}
	loadOpts := config.LoadOptions{ExtensionSpecs: app.BuiltinExtensions().ConfigSpecs()}
	if _, err := config.EnsureStarterConfig(configPath, loadOpts); err != nil {
		return a.fail("gateway.start", "config_init", "gateway_config_init_failed", "unable to initialize default config", false, err)
	}
	cfg, err := config.LoadFromFileWithOptions(configPath, loadOpts)
	if err != nil {
		return a.fail("gateway.start", "loading_config", "gateway_config_load_failed", "unable to load config", false, err)
	}
	instanceID, token := a.newIdentity()
	// Start derives the whole run context from ctx, so it must not carry a
	// timeout: a timeout here would auto-stop the running gateway.
	_, err = a.svc.Start(a.appCtx, gateway.StartOptions{
		Config:      cfg,
		DesktopMode: true,
		InstanceID:  instanceID,
		Token:       token,
		ServerToken: cfg.AuthToken,
	})
	if err != nil {
		return a.startError("gateway.start", err)
	}
	a.activeConfigPath = configPath
	a.emitStatus()
	return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
}

func (a *App) stopGatewayLocked() GatewayCommandResult {
	if st := a.svc.Status().Status; st == gateway.StatusStopped || st == gateway.StatusFailed {
		return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
	}
	stopCtx, cancel := context.WithTimeout(a.appCtx, 10*time.Second)
	defer cancel()
	if err := a.svc.Stop(stopCtx); err != nil {
		return a.stopError("gateway.stop", err)
	}
	a.emitStatus()
	return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
}

func (a *App) startError(operation string, err error) GatewayCommandResult {
	switch {
	case errors.Is(err, gateway.ErrAlreadyRunning):
		return a.fail(operation, "starting", "gateway_already_running", "gateway is already running", false, err)
	case errors.Is(err, gateway.ErrDesktopModeRequiresIdentity):
		return a.fail(operation, "validating_config", "gateway_identity_required", "desktop mode requires an instance identity and token", false, err)
	case errors.Is(err, gateway.ErrDesktopModeRequiresLoopback):
		return a.fail(operation, "validating_config", "gateway_loopback_required", "desktop mode requires a loopback listen address", false, err)
	case errors.Is(err, gateway.ErrStartCanceled):
		return a.fail(operation, "starting", "gateway_start_canceled", "gateway start was canceled", false, err)
	default:
		return a.fail(operation, "starting", "gateway_start_failed", "gateway start failed", true, err)
	}
}

func (a *App) stopError(operation string, err error) GatewayCommandResult {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		res := a.fail(operation, "stopping", "gateway_stop_timeout", "gateway did not stop within the deadline", false, err)
		res.Error.GatewayLeftRunning = a.svc.Status().Status != gateway.StatusStopped
		return res
	}
	return a.fail(operation, "stopping", "gateway_stop_failed", "gateway stop failed", false, err)
}

func (a *App) fail(operation, stage, code, message string, retryable bool, cause error) GatewayCommandResult {
	if message == "" && cause != nil {
		message = cause.Error()
	}
	snap := a.snapshot()
	return GatewayCommandResult{
		OK: false,
		Error: &CommandError{
			Operation:       operation,
			Stage:           stage,
			Code:            code,
			Message:         message,
			Retryable:       retryable,
			GatewaySnapshot: &snap,
		},
	}
}

func (a *App) closedResult(operation string) GatewayCommandResult {
	return a.fail(operation, "host", "gateway_host_closed", "desktop host is shut down", false, nil)
}

func (a *App) snapshot() GatewaySnapshot {
	st := a.svc.Status()
	snap := GatewaySnapshot{ConfigPath: a.activeConfigPath}
	switch st.Status {
	case gateway.StatusRunning:
		snap.State = "running"
		snap.Address = st.Addr
		pid := st.PID
		snap.PID = &pid
		if st.InstanceID != "" {
			instanceID := st.InstanceID
			snap.InstanceID = &instanceID
		}
	case gateway.StatusStarting:
		snap.State = "starting"
	case gateway.StatusStopping:
		snap.State = "stopping"
	case gateway.StatusFailed:
		snap.State = "error"
		if st.LastError != "" {
			lastErr := st.LastError
			snap.Error = &lastErr
		}
	default:
		snap.State = "stopped"
	}
	return snap
}

func (a *App) snapshotPtr() *GatewaySnapshot {
	snap := a.snapshot()
	return &snap
}

func (a *App) emitStatus() {
	a.emitEvents(gatewayStatusEvent, a.snapshotPtr())
}

// resolveDefaultConfigPath returns the default config path: the CLI's existing
// resolution first, falling back to the OS user config dir when HOME is unset.
func resolveDefaultConfigPath() (string, error) {
	if p, err := config.ResolveConfigPath(""); err == nil {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "moonbridge", "config.yml"), nil
}

// resolveConfigPath resolves a Start candidate: request → configuredPath →
// default path.
func (a *App) resolveConfigPath(request string) (string, error) {
	if request != "" {
		return request, nil
	}
	if a.configuredPath != "" {
		return a.configuredPath, nil
	}
	return resolveDefaultConfigPath()
}
