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
	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/codexlauncher"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/trafficanalysis"
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

// gatewaySession is the per-run identity/config bundle the DeepSeek and Codex
// bindings derive from. The secrets (ControlToken / ServerToken) never leave
// this struct.
type gatewaySession struct {
	InstanceID   string        // matches svc.Status().InstanceID; guards against stale sessions
	Address      string        // management API base / codex base URL source
	ControlToken string        // DeepSeek management API bearer. Secret.
	ServerToken  string        // codex auth.json token. Secret.
	ConfigPath   string        // config the session started from / reloads after saves
	Config       config.Config // codex config generation source (loaded at start)
	ConfigValid  bool          // Config matches the current on-disk config
}

// codexController is the codex terminal-session controller the App drives. It
// is an interface so tests can inject a fake; codexlauncher.Launcher satisfies
// it directly.
type codexController interface {
	Launch(ctx context.Context, opts codexlauncher.LaunchOptions) (codexlauncher.State, error)
	Stop(ctx context.Context, reason codexlauncher.StopReason) (codexlauncher.State, error)
	Restart(ctx context.Context, opts codexlauncher.LaunchOptions) (codexlauncher.State, error)
	Status() codexlauncher.State
}

// deepSeekController is the subset of the deepseek service the App drives.
type deepSeekController interface {
	Load(ctx context.Context) (*deepseek.Snapshot, error)
	Save(ctx context.Context, input deepseek.Input) (*deepseek.Snapshot, error)
	Validate(input deepseek.Input) error
}

// deepSeekFactory builds a controller per operation from the live session, so
// a gateway restart's fresh control token is always used.
type deepSeekFactory func(address, controlToken string) deepSeekController

// codexConfigController is the user codex-config editor the App drives.
type codexConfigController interface {
	Load(ctx context.Context) (codexconfig.Snapshot, error)
	Update(ctx context.Context, input codexconfig.Input) (codexconfig.Snapshot, error)
	Restore(ctx context.Context, id string) (codexconfig.Snapshot, error)
	ListBackups(ctx context.Context) ([]codexconfig.BackupInfo, error)
}

// codexConfigDeriver rebuilds session.Config from the live gateway effective
// config (the SQLite-backed source of truth). It is a seam for unit tests;
// nil defaults to the live /config/effective fetch.
type codexConfigDeriver func(ctx context.Context, session *gatewaySession, loadOpts config.LoadOptions) (config.Config, error)

type App struct {
	ctx              context.Context // Wails runtime ctx（startup で保存のみ）
	appCtx           context.Context // gateway run の親 ctx（NewApp で生成）
	cancel           context.CancelFunc
	operationMu      sync.Mutex // Gateway lifecycle + DeepSeek 管理 API
	codexMu          sync.Mutex // Codex terminal process
	configMu         sync.Mutex // ユーザー実 Codex config
	svc              gatewayController
	traffic          *trafficanalysis.Service
	configuredPath   string // AppOptions で指定された Start 候補
	activeConfigPath string // 最後に成功した Start の path（snapshot にのみ表示）
	newIdentity      func() (string, string)
	emitEvents       func(name string, payload any)
	closed           atomic.Bool

	session     *gatewaySession
	codex       codexController
	codexConfig codexConfigController
	newDeepSeek deepSeekFactory
	deriveCodex codexConfigDeriver
	codexOp     string // current codex operation for progress events（codexMu で保護）
}

type AppOptions struct {
	Service     gatewayController              // nil → gateway.NewService(ServiceOptions{Errors: os.Stderr})
	NewIdentity func() (string, string)        // nil → gateway.NewDesktopIdentity
	ConfigPath  string                         // Start 候補。"" → 既定パス（lazy resolve）
	EmitEvents  func(name string, payload any) // nil → runtime.EventsEmit（a.ctx 非nil時のみ）
	Codex       codexController                // nil → codexlauncher.New(Options{Progress: …})
	CodexConfig codexConfigController          // nil → codexconfig.New(codexconfig.Options{})
	NewDeepSeek deepSeekFactory                // nil → NewHTTPClient ベースの既定 factory
	DeriveCodex codexConfigDeriver             // nil → 稼働中 Gateway の effective config から導出
	Traffic     *trafficanalysis.Service       // nil → 長寿命 Service を新規生成・所有
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
	codexConfig := opts.CodexConfig
	if codexConfig == nil {
		codexConfig = codexconfig.New(codexconfig.Options{})
	}
	newDeepSeek := opts.NewDeepSeek
	if newDeepSeek == nil {
		newDeepSeek = func(address, controlToken string) deepSeekController {
			return deepseek.NewService(deepseek.NewHTTPClient(address, controlToken))
		}
	}
	deriveCodex := opts.DeriveCodex
	if deriveCodex == nil {
		deriveCodex = deriveCodexLive
	}
	traffic := opts.Traffic
	if traffic == nil {
		traffic = trafficanalysis.NewService()
	}
	appCtx, cancel := context.WithCancel(context.Background())
	a := &App{
		appCtx:         appCtx,
		cancel:         cancel,
		svc:            svc,
		traffic:        traffic,
		configuredPath: opts.ConfigPath,
		newIdentity:    newIdentity,
		emitEvents:     opts.EmitEvents,
		codexConfig:    codexConfig,
		newDeepSeek:    newDeepSeek,
		deriveCodex:    deriveCodex,
	}
	if opts.Codex == nil {
		a.codex = codexlauncher.New(codexlauncher.Options{
			// Progress runs synchronously inside Launch/Restart, which the
			// bindings call under codexMu, so reading a.codexOp is lock-guarded.
			Progress: func(stage, detail string) {
				a.emitCodexProgress(a.codexOp, stage, detail)
			},
		})
	} else {
		a.codex = opts.Codex
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
	a.cancel() // run 全体の親 context を cancel → 進行中 Start / Launch を中断、稼働中 Gateway の停止も開始

	a.operationMu.Lock()
	defer a.operationMu.Unlock()

	// Codex terminal first (reason=shutdown), gateway-independent. Lock order
	// operationMu → codexMu only; both waits are bounded.
	a.codexMu.Lock()
	codexStopCtx, codexCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, _ = a.codex.Stop(codexStopCtx, codexlauncher.StopReasonShutdown)
	codexCancel()
	a.codexMu.Unlock()

	gwStopCtx, gwCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer gwCancel()
	_ = a.svc.Stop(gwStopCtx) // cleanup 完了を待つ

	a.session = nil
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
	a.invalidateStaleSession() // clear a session the running gateway no longer matches (never errors)
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
		Traffic:     a.traffic,
		TrafficLifecycle: &app.TrafficLifecycle{
			BindRun: func(id, address string) error {
				_, err := a.traffic.BindGatewayRun(id, address)
				return err
			},
			EndRun: func(id string, reason app.EndRunReason) {
				a.traffic.MarkGatewayLost(id, reason != app.EndRunStopped)
			},
		},
	})
	if err != nil {
		return a.startError("gateway.start", err)
	}
	// The session is built only after Start succeeds: the address materializes
	// with the successful run, and the control token must be this run's.
	st := a.svc.Status()
	a.session = &gatewaySession{
		InstanceID:   st.InstanceID,
		Address:      st.Addr,
		ControlToken: token,
		ServerToken:  cfg.AuthToken,
		ConfigPath:   configPath,
		Config:       cfg,
		ConfigValid:  true,
	}
	a.activeConfigPath = configPath
	a.emitStatus()
	return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
}

func (a *App) stopGatewayLocked() GatewayCommandResult {
	if st := a.svc.Status().Status; st == gateway.StatusStopped || st == gateway.StatusFailed {
		a.session = nil
		return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
	}
	stopCtx, cancel := context.WithTimeout(a.appCtx, 10*time.Second)
	defer cancel()
	if err := a.svc.Stop(stopCtx); err != nil {
		return a.stopError("gateway.stop", err)
	}
	a.session = nil
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

// invalidateStaleSession clears the session when the running gateway no longer
// matches it (status, instance id, or address). Called under operationMu.
func (a *App) invalidateStaleSession() {
	if a.session == nil {
		return
	}
	st := a.svc.Status()
	if st.Status != gateway.StatusRunning || st.InstanceID != a.session.InstanceID || st.Addr != a.session.Address {
		a.session = nil
	}
}

// ensureActiveSession validates the current gateway run against the retained
// session, clearing it on mismatch so a stale control token is never reused.
func (a *App) ensureActiveSession() (*gatewaySession, bool) {
	a.invalidateStaleSession()
	if a.session == nil {
		return nil, false
	}
	return a.session, true
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

// deriveConfigCodex rebuilds session.Config from the live gateway effective
// config. Returns a zero Config and the error on failure.
func (a *App) deriveConfigCodex(session *gatewaySession) (config.Config, error) {
	loadOpts := config.LoadOptions{ExtensionSpecs: app.BuiltinExtensions().ConfigSpecs()}
	return a.deriveCodex(a.appCtx, session, loadOpts)
}

// deriveCodexLive is the default derivation: fetch GET /api/v1/config/effective
// with the session control token (the SQLite-backed store is the source of
// truth; config.yml is only a one-time seed), rebuild a config.Config, and stamp
// the live server token back (the effective payload masks it). Masked secrets
// from the effective payload are never copied into a codex-generation config:
// only session.ServerToken (the gateway auth token for codex auth.json) is used,
// and masked provider api_key fields are never referenced.
func deriveCodexLive(ctx context.Context, session *gatewaySession, loadOpts config.LoadOptions) (config.Config, error) {
	fc, err := deepseek.NewHTTPClient("http://"+session.Address, session.ControlToken).Effective(ctx)
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.FromFileConfigWithOptions(fc, loadOpts)
	if err != nil {
		return config.Config{}, err
	}
	// The effective payload masks Server.AuthToken. Re-inject the real run token
	// so codex auth.json uses the live session credential (never the mask).
	cfg.AuthToken = session.ServerToken
	return cfg, nil
}
