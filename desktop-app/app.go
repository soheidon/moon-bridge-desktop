package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	"moonbridge/internal/service/gatewayintegration"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/routingprofile"
	"moonbridge/internal/service/routingswitch"
	"moonbridge/internal/service/trafficanalysis"
	"moonbridge/internal/service/traffictransaction"
)

const (
	roundTripEvent     = "desktop:roundtrip"
	gatewayStatusEvent = "gateway-status"
	gatewayLogEvent    = "gateway-log"
	trafficEvent       = "traffic-transaction-event"
)

type CommandError struct {
	Operation            string         `json:"operation"`
	Stage                string         `json:"stage"`
	Code                 string         `json:"code"`
	Message              string         `json:"message"`
	Field                *string        `json:"field"`
	Retryable            bool           `json:"retryable"`
	MutationStarted      bool           `json:"mutationStarted"`
	GatewayLeftRunning   bool           `json:"gatewayLeftRunning"`
	ConfirmationRequired bool           `json:"confirmationRequired,omitempty"`
	RecoveryRequired     bool           `json:"recoveryRequired,omitempty"`
	GatewaySnapshot      any            `json:"gatewaySnapshot"`
	Details              map[string]any `json:"details,omitempty"`
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
	State      string                        `json:"state"`      // stopped|starting|running|stopping|error
	Address    string                        `json:"address"`    // running時のみ実Listenアドレス
	ConfigPath string                        `json:"configPath"` // activeConfigPath（最後に成功した Start の path）のみ
	PID        *int                          `json:"pid"`        // running時のみ
	InstanceID *string                       `json:"instanceId"` // running時のみ
	Error      *string                       `json:"error"`      // state==error 時のみ lastError
	Runtime    *RuntimeConfigurationSnapshot `json:"runtimeConfiguration,omitempty"`
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
	RefreshRoutingProfile(cfg config.Config)
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
	Clear(ctx context.Context) (*deepseek.Snapshot, error)
	TestProvider(ctx context.Context) (deepseek.ConnectionTestResult, error)
	Validate(input deepseek.Input) error
}

// deepSeekFactory builds a controller per operation from the live session, so
// a gateway restart's fresh control token is always used.
type deepSeekFactory func(address, controlToken string) deepSeekController

// routingProfileFactory builds a controller per operation from the live
// session, so a gateway restart's fresh control token is always used. The
// routingProfileController interface it returns lives in
// routing_profile_bindings.go.
type routingProfileFactory func(address, controlToken string) routingProfileController

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
	operationMu      sync.Mutex   // Gateway lifecycle + DeepSeek 管理 API
	codexMu          sync.Mutex   // Codex terminal process
	configMu         sync.Mutex   // ユーザー実 Codex config
	trafficMu        sync.Mutex   // Desktop traffic transaction coordination
	sessionMu        sync.RWMutex // short immutable copy of the live gateway session
	routeGateMu      sync.Mutex
	routeGate        *routingswitch.Gate
	transitionGen    *routingswitch.Generator
	lifecycleMu      sync.Mutex
	lifecycle        appLifecycle
	startupOnce      sync.Once
	startupErr       error
	domReadySeen     bool
	shutdownMu       sync.Mutex
	shutdownDone     chan struct{}
	shutdownStarted  bool
	exitMu           sync.Mutex
	exitState        exitState
	svc              gatewayController
	traffic          *trafficanalysis.Service
	configuredPath   string // AppOptions で指定された Start 候補
	activeConfigPath string // 最後に成功した Start の path（snapshot にのみ表示）
	newIdentity      func() (string, string)
	emitEvents       func(name string, payload any)
	gatewayLogs      *gatewayLogBridge
	closed           atomic.Bool

	session           *gatewaySession
	codex             codexController
	codexConfig       codexConfigController
	newDeepSeek       deepSeekFactory
	newRoutingProfile routingProfileFactory
	deriveCodex       codexConfigDeriver
	quitDesktop       func(context.Context)
	codexOp           string // current codex operation for progress events（codexMu で保護）
	trafficTx         *traffictransaction.Service
	recovery          *recovery.Store
	recoveryHome      string
	trafficConfigPath string
	trafficBackupDir  string

	gatewayInt           *gatewayintegration.Service
	gatewayIntConfigPath string

	// trafficLog holds the autosave writer for the current capture session. It
	// is atomic so EndRun (which must not take trafficMu) and TrafficAnalysisStatus
	// (which reads the snapshot without trafficMu) can access it safely.
	trafficLog        atomic.Pointer[trafficLogWriter]
	trafficLogInitErr atomic.Pointer[string]
	trafficLogDir     string
	exportMu          sync.Mutex
	lastTrafficExport string
	saveDialogFunc    func(context.Context, runtime.SaveDialogOptions) (string, error)
	explorerFunc      func(args ...string) error

	// routingProfileRefresh rebuilds the Gateway's runtime SlotResolver from a
	// fresh config snapshot. Set by the gateway run via desktop control hook.
	routingProfileRefresh func(cfg config.Config)
}

type AppOptions struct {
	Service            gatewayController              // nil → gateway.NewService(ServiceOptions{Errors: os.Stderr})
	NewIdentity        func() (string, string)        // nil → gateway.NewDesktopIdentity
	ConfigPath         string                         // Start 候補。"" → 既定パス（lazy resolve）
	EmitEvents         func(name string, payload any) // nil → runtime.EventsEmit（a.ctx 非nil時のみ）
	Codex              codexController                // nil → codexlauncher.New(Options{Progress: …})
	CodexConfig        codexConfigController          // nil → codexconfig.New(codexconfig.Options{})
	NewDeepSeek        deepSeekFactory                // nil → NewHTTPClient ベースの既定 factory
	NewRoutingProfile  routingProfileFactory          // nil → routingprofile.NewService(NewHTTPClient) の既定 factory
	DeriveCodex        codexConfigDeriver             // nil → 稼働中 Gateway の effective config から導出
	Quit               func(context.Context)          // nil → Wails runtime.Quit; test seam only
	Traffic            *trafficanalysis.Service       // nil → 長寿命 Service を新規生成・所有
	TrafficTransaction *traffictransaction.Service
	Recovery           *recovery.Store
	// RecoveryHome identifies the Codex profile associated with an injected
	// Recovery store. It is primarily a test/embedding seam; the normal Wails
	// path resolves the profile from codexconfig.Service.
	RecoveryHome string
	// BackupDir scopes transaction backups for an injected profile. The normal
	// Wails path leaves it empty and uses the platform app-data root.
	BackupDir string
	// RoutingProfileRefresh rebuilds the Gateway's runtime SlotResolver from a
	// fresh config snapshot. Set by the gateway run via desktop control hook.
	RoutingProfileRefresh func(cfg config.Config)
}

func NewApp(opts AppOptions) *App {
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
	newRoutingProfile := opts.NewRoutingProfile
	if newRoutingProfile == nil {
		newRoutingProfile = func(address, controlToken string) routingProfileController {
			return routingprofile.NewService(deepseek.NewHTTPClient(address, controlToken))
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
		appCtx:                appCtx,
		cancel:                cancel,
		svc:                   opts.Service,
		traffic:               traffic,
		configuredPath:        opts.ConfigPath,
		newIdentity:           newIdentity,
		emitEvents:            opts.EmitEvents,
		trafficTx:             opts.TrafficTransaction,
		recovery:              opts.Recovery,
		recoveryHome:          opts.RecoveryHome,
		trafficBackupDir:      opts.BackupDir,
		codexConfig:           codexConfig,
		newDeepSeek:           newDeepSeek,
		newRoutingProfile:     newRoutingProfile,
		deriveCodex:           deriveCodex,
		quitDesktop:           opts.Quit,
		routingProfileRefresh: opts.RoutingProfileRefresh,
		routeGate:             routingswitch.NewGate(),
		transitionGen:         routingswitch.NewGenerator(),
	}
	if a.quitDesktop == nil {
		a.quitDesktop = runtime.Quit
	}
	a.saveDialogFunc = runtime.SaveFileDialog
	a.explorerFunc = defaultExplorerFunc
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
	// The gateway's runtime notices are bridged to the "gateway-log" Wails
	// event while the existing os.Stderr output is preserved. Construction is
	// deferred until after emitEvents is set so the bridge can emit safely
	// (a.ctx may still be nil here; the default emitEvents drops events then).
	if a.svc == nil {
		a.gatewayLogs = newGatewayLogBridge(a.safeEmit)
		a.svc = gateway.NewService(gateway.ServiceOptions{
			Errors: io.MultiWriter(os.Stderr, a.gatewayLogs),
		})
	}
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.startupOnce.Do(func() {
		a.lifecycleMu.Lock()
		a.lifecycle = lifecycleStarting
		a.lifecycleMu.Unlock()
		configPath, pathErr := a.resolveCodexConfigPath(ctx)
		if pathErr != nil {
			a.startupErr = pathErr
		}
		if configPath == "" {
			a.startupErr = errors.New("codex config path is unavailable")
		} else if err := a.ensureRecoveryStore(filepath.Dir(configPath)); err != nil {
			a.startupErr = err
		} else if a.recovery != nil {
			reconcileCtx, cancel := context.WithTimeout(a.appCtx, 10*time.Second)
			_, a.startupErr = a.recovery.ReconcileStartup(reconcileCtx, func(path string) ([]byte, error) {
				return os.ReadFile(path)
			})
			cancel()
		}
		a.lifecycleMu.Lock()
		a.lifecycle = lifecycleStarted
		a.lifecycleMu.Unlock()
	})
}

func (a *App) shutdown(context.Context) {
	a.shutdownMu.Lock()
	if a.shutdownStarted {
		done := a.shutdownDone
		a.shutdownMu.Unlock()
		<-done
		return
	}
	a.shutdownStarted = true
	a.shutdownDone = make(chan struct{})
	done := a.shutdownDone
	a.shutdownMu.Unlock()

	a.closed.Store(true)
	a.lifecycleMu.Lock()
	a.lifecycle = lifecycleClosing
	a.lifecycleMu.Unlock()
	defer func() {
		a.lifecycleMu.Lock()
		a.lifecycle = lifecycleClosed
		a.lifecycleMu.Unlock()
		close(done)
	}()

	a.cancel() // cancel active App operations, then continue independent cleanup

	a.operationMu.Lock()
	defer a.operationMu.Unlock()

	// Traffic cleanup is best effort and never blocks later Codex/Gateway
	// cleanup. A confirmation-required Finish is retained as Recovery evidence;
	// shutdown never invents discard consent.
	a.trafficMu.Lock()
	if a.trafficTx != nil {
		trafficCtx, trafficCancel := context.WithTimeout(context.Background(), 15*time.Second)
		st := a.traffic.Status()
		if st.Mode == trafficanalysis.ModeDesktop {
			_, _ = a.trafficTx.Disable(trafficCtx)
		} else if st.Mode == trafficanalysis.ModeCaptureOnly && st.CaptureState == "passthrough" {
			_, _ = a.trafficTx.Finish(trafficCtx, false)
		}
		trafficCancel()
	}
	// Close the autosave log after the traffic transaction cleanup: the final
	// drain needs the observations that Disable/Finish still expose.
	a.closeTrafficAutosaveLocked(true)
	a.trafficMu.Unlock()

	// Codex terminal first (reason=shutdown), gateway-independent.
	a.codexMu.Lock()
	codexStopCtx, codexCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, _ = a.codex.Stop(codexStopCtx, codexlauncher.StopReasonShutdown)
	codexCancel()
	a.codexMu.Unlock()

	// Restore the original upstream before stopping the gateway. Best effort:
	// a failed restore is retained as recovery evidence for the next boot.
	_ = a.disableGatewayIntegration()

	gwStopCtx, gwCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = a.svc.Stop(gwStopCtx) // cleanup continues even after traffic failure
	gwCancel()

	a.clearSession()
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
	snap := a.snapshot()
	snap.Runtime = a.runtimeConfigurationSnapshot()
	return GatewayCommandResult{OK: true, Value: &snap}
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
	// Wire the routing profile refresh callback so that binding-layer mutations
	// (SaveRoutingProfile, ActivateProfile) can trigger a live resolver swap
	// via gateway.Service.RefreshRoutingProfile. Only set if the caller didn't
	// provide one (production path via main.go leaves it nil; tests may inject).
	if a.routingProfileRefresh == nil {
		a.routingProfileRefresh = func(cfg config.Config) { a.svc.RefreshRoutingProfile(cfg) }
	}
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
				// Abnormal EndRun must not take trafficMu (StopGateway holds
				// operationMu while waiting for EndRun; trafficGatewayProvider
				// snapshots take operationMu via bindings → lock-order
				// inversion). The atomic pointer + writer mutex are enough.
				if w := a.trafficLog.Load(); w != nil {
					w.Close(false)
				}
			},
		},
		RoutingProfileRefresh: a.routingProfileRefresh,
	})
	if err != nil {
		return a.startError("gateway.start", err)
	}
	// The session is built only after Start succeeds: the address materializes
	// with the successful run, and the control token must be this run's.
	st := a.svc.Status()
	// Redirect Codex to this run's gateway address (S0 → S1). A failure here is
	// classified so the gateway is stopped only when the config is unchanged or
	// rolled back; a fail-closed or already-integrated outcome keeps it running.
	if err := a.integrateGateway("http://" + st.Addr); err != nil {
		return a.gatewayIntegrationError("gateway.start", err)
	}
	a.sessionMu.Lock()
	a.session = &gatewaySession{
		InstanceID:   st.InstanceID,
		Address:      st.Addr,
		ControlToken: token,
		ServerToken:  cfg.AuthToken,
		ConfigPath:   configPath,
		Config:       cfg,
		ConfigValid:  true,
	}
	a.sessionMu.Unlock()
	a.activeConfigPath = configPath
	a.emitStatus()
	return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
}

func (a *App) stopGatewayLocked() GatewayCommandResult {
	if st := a.svc.Status().Status; st == gateway.StatusStopped || st == gateway.StatusFailed {
		a.clearSession()
		return GatewayCommandResult{OK: true, Value: a.snapshotPtr()}
	}
	// 1. Demote the inner Traffic Analysis layer first (S2 → S1) so Codex never
	// points at a stopped capture listener. Fail-closed: a teardown failure
	// aborts the whole stop rather than leaving Codex pointing at a dead port.
	if a.trafficTx != nil {
		a.trafficMu.Lock()
		trafficCtx, trafficCancel := context.WithTimeout(a.appCtx, 15*time.Second)
		st := a.traffic.Status()
		var trafficErr error
		switch {
		case st.Mode == trafficanalysis.ModeDesktop:
			_, trafficErr = a.trafficTx.Disable(trafficCtx)
		case st.Mode == trafficanalysis.ModeCaptureOnly && st.CaptureState == "passthrough":
			_, trafficErr = a.trafficTx.Finish(trafficCtx, false)
		}
		trafficCancel()
		if trafficErr == nil {
			a.closeTrafficAutosaveLocked(true)
		}
		a.trafficMu.Unlock()
		if trafficErr != nil {
			return a.fail("gateway.stop", "traffic", "traffic_teardown_failed", "traffic analysis could not be stopped", true, trafficErr)
		}
	}
	// 2. Restore the original upstream (S1 → S0) before the process stops.
	// disableGatewayIntegration logs its own success/failure diagnostic.
	if err := a.disableGatewayIntegration(); err != nil {
		return a.fail("gateway.stop", "integration", "gateway_integration_disable_failed", "gateway integration could not be disabled", true, err)
	}
	// 3. Stop the gateway process.
	stopCtx, cancel := context.WithTimeout(a.appCtx, 10*time.Second)
	defer cancel()
	if err := a.svc.Stop(stopCtx); err != nil {
		return a.stopError("gateway.stop", err)
	}
	a.clearSession()
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

// integrateGateway binds the outer integration service and redirects Codex to
// the given gateway address. It is a thin wrapper so startGatewayLocked can
// classify the failure without reaching into the service.
func (a *App) integrateGateway(targetURL string) error {
	gwInt, err := a.ensureGatewayIntegration()
	if err != nil {
		return err
	}
	return gwInt.Enable(a.appCtx, targetURL)
}

// disableGatewayIntegration restores the original upstream recorded by the
// Gateway layer. It is a no-op when nothing is integrated. The Disable outcome
// is logged so a no-op that hides an orphaned gateway config stays visible.
func (a *App) disableGatewayIntegration() error {
	gwInt, err := a.ensureGatewayIntegration()
	if err != nil {
		return err
	}
	report, err := gwInt.DisableWithReport(a.appCtx)
	logGatewayDisable(err == nil, report, err)
	return err
}

// gatewayIntegrationError classifies a failed gateway integration. A fail-closed
// or already-integrated outcome leaves the gateway running (Codex may already
// point at it); any other failure means the config is unchanged or rolled back,
// so the just-started gateway is stopped.
func (a *App) gatewayIntegrationError(operation string, err error) GatewayCommandResult {
	logGatewayIntegrationError(operation, err)
	if errors.Is(err, gatewayintegration.ErrFailClosed) || errors.Is(err, gatewayintegration.ErrAlreadyIntegrated) {
		res := a.fail(operation, "integration", "gateway_integration_failed", "gateway integration failed; recovery is required", true, err)
		res.Error.GatewayLeftRunning = true
		res.Error.RecoveryRequired = true
		return res
	}
	stopCtx, cancel := context.WithTimeout(a.appCtx, 10*time.Second)
	defer cancel()
	_ = a.svc.Stop(stopCtx)
	a.clearSession()
	return a.fail(operation, "integration", "gateway_integration_failed", "gateway integration failed", true, err)
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

type runtimeConfigurationWire struct {
	State                 string             `json:"state"`
	ServerInstance        string             `json:"server_instance"`
	ResolverGeneration    uint64             `json:"resolver_generation"`
	InstallSource         string             `json:"install_source"`
	ConfigSource          string             `json:"config_source"`
	ResolverPresent       bool               `json:"resolver_present"`
	RoutingExtensionState string             `json:"routing_extension_state"`
	ActiveProfileState    string             `json:"active_profile_state"`
	ReadySlotCount        int                `json:"ready_slot_count"`
	CredentialState       string             `json:"credential_state"`
	Slots                 runtimeSlotWireSet `json:"slots"`
}

type runtimeSlotWireSet struct {
	Sol   runtimeSlotWire `json:"sol"`
	Terra runtimeSlotWire `json:"terra"`
	Luna  runtimeSlotWire `json:"luna"`
}

type runtimeSlotWire struct {
	State            string `json:"state"`
	Provider         string `json:"provider,omitempty"`
	UpstreamModel    string `json:"upstream_model,omitempty"`
	Mode             string `json:"mode,omitempty"`
	ConfiguredEffort string `json:"configured_effort,omitempty"`
	CredentialState  string `json:"credential_state,omitempty"`
}

func unavailableRuntimeConfiguration() *RuntimeConfigurationSnapshot {
	return &RuntimeConfigurationSnapshot{State: "unavailable", CredentialState: "unknown"}
}

// runtimeConfigurationSnapshot reads the authenticated status endpoint of the
// current Gateway run. Failure is intentionally reduced to unavailable; raw
// response bodies, tokens, paths, and transport errors never cross the Wails
// boundary.
func (a *App) runtimeConfigurationSnapshot() *RuntimeConfigurationSnapshot {
	if a.svc.Status().Status != gateway.StatusRunning {
		return unavailableRuntimeConfiguration()
	}
	session, ok := a.copySession()
	if !ok || session.Address == "" || session.ControlToken == "" {
		return unavailableRuntimeConfiguration()
	}
	req, err := http.NewRequestWithContext(a.appCtx, http.MethodGet, "http://"+session.Address+"/api/v1/system/status", nil)
	if err != nil {
		return unavailableRuntimeConfiguration()
	}
	req.Header.Set("Authorization", "Bearer "+session.ControlToken)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return unavailableRuntimeConfiguration()
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return unavailableRuntimeConfiguration()
	}
	var body struct {
		Runtime *runtimeConfigurationWire `json:"runtime_configuration"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Runtime == nil {
		return unavailableRuntimeConfiguration()
	}
	return mapRuntimeConfiguration(body.Runtime)
}

func mapRuntimeConfiguration(wire *runtimeConfigurationWire) *RuntimeConfigurationSnapshot {
	if wire == nil {
		return unavailableRuntimeConfiguration()
	}
	return &RuntimeConfigurationSnapshot{
		State: wire.State, ServerInstance: wire.ServerInstance, ResolverGeneration: wire.ResolverGeneration,
		InstallSource: wire.InstallSource, ConfigSource: wire.ConfigSource, ResolverPresent: wire.ResolverPresent,
		RoutingExtensionState: wire.RoutingExtensionState, ActiveProfileState: wire.ActiveProfileState,
		ReadySlotCount: wire.ReadySlotCount, CredentialState: wire.CredentialState,
		Slots: RuntimeSlotSnapshotSet{
			Sol: mapRuntimeSlot(wire.Slots.Sol), Terra: mapRuntimeSlot(wire.Slots.Terra), Luna: mapRuntimeSlot(wire.Slots.Luna),
		},
	}
}

func mapRuntimeSlot(slot runtimeSlotWire) RuntimeSlotSnapshot {
	return RuntimeSlotSnapshot{State: slot.State, Provider: slot.Provider, UpstreamModel: slot.UpstreamModel, Mode: slot.Mode, ConfiguredEffort: slot.ConfiguredEffort, CredentialState: slot.CredentialState}
}

func (a *App) emitStatus() {
	a.safeEmit(gatewayStatusEvent, a.snapshotPtr())
}

// invalidateStaleSession clears the session when the running gateway no longer
// matches it (status, instance id, or address). Called under operationMu.
func (a *App) invalidateStaleSession() {
	session, ok := a.copySession()
	if !ok {
		return
	}
	st := a.svc.Status()
	if st.Status != gateway.StatusRunning || st.InstanceID != session.InstanceID || st.Addr != session.Address {
		a.clearSession()
	}
}

// ensureActiveSession validates the current gateway run against the retained
// session, clearing it on mismatch so a stale control token is never reused.
func (a *App) ensureActiveSession() (*gatewaySession, bool) {
	a.invalidateStaleSession()
	// Existing operation bindings call this while holding operationMu and may
	// refresh ConfigValid on the live session. The lock-free copy is reserved
	// for trafficGatewayProvider, whose snapshot must not acquire operationMu.
	if a.session == nil {
		return nil, false
	}
	return a.session, true
}

func (a *App) copySession() (*gatewaySession, bool) {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()
	if a.session == nil {
		return nil, false
	}
	session := *a.session
	return &session, true
}

func (a *App) clearSession() {
	a.sessionMu.Lock()
	a.session = nil
	a.sessionMu.Unlock()
}

func (a *App) operationGate() *routingswitch.Gate {
	a.routeGateMu.Lock()
	defer a.routeGateMu.Unlock()
	if a.routeGate == nil {
		a.routeGate = routingswitch.NewGate()
	}
	return a.routeGate
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
