package codexlauncher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/extension/codex"
	"moonbridge/internal/service/publishrecovery"
	"moonbridge/internal/service/recovery"
)

type Status string

const (
	StatusIdle     Status = "idle"
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
	StatusStopped  Status = "stopped"
	StatusError    Status = "error"
)

type StopReason string

const (
	StopReasonGraceful       StopReason = "graceful"
	StopReasonTimeoutForce   StopReason = "timeout_force"
	StopReasonShutdown       StopReason = "shutdown"
	StopReasonTerminalClosed StopReason = "terminal_closed"
)

const (
	defaultGracefulStopTimeout = 5 * time.Second
	defaultForceStopTimeout    = 5 * time.Second
	defaultProbeTimeout        = 5 * time.Second
)

// State is a snapshot of the launcher. It never carries secrets.
type State struct {
	Status     Status
	PID        int
	CodexHome  string
	StartedAt  time.Time
	StoppedAt  time.Time
	ExitCode   *int // PowerShell (terminal) exit code, not the codex binary
	StopReason StopReason
	Error      string
}

type ProgressFunc func(stage, detail string)

// LaunchOptions carries everything the App derives from its gateway session.
// AuthToken is the codex auth.json token and lives only in memory.
type LaunchOptions struct {
	CodexHome        string
	ProjectDirectory string
	ModelAlias       string
	BaseURL          string
	AuthToken        string
	ProviderCfg      config.ProviderConfig
	PluginCfg        config.PluginConfig
	ServerCfg        config.ServerConfig
}

type discoverFunc func(ctx context.Context, timeout time.Duration) (string, error)

// discoverCodex is the platform discovery used when Options does not inject
// one. The windows build overrides it in init().
var discoverCodex discoverFunc = func(ctx context.Context, timeout time.Duration) (string, error) {
	return "", ErrUnsupportedPlatform
}

// Options configures a Launcher. Nil Runner/Discover/SendCtrlBreak default to
// the platform implementations; zero durations default to the package values.
// A nil Publisher defaults to the production crash-journaled publisher
// (publishrecovery.Service rooted at %LOCALAPPDATA%\Moon Bridge\recovery),
// resolved lazily per publish so an init failure surfaces as a launch error.
type Options struct {
	Runner              processRunner
	Discover            discoverFunc
	SendCtrlBreak       func(ctx context.Context, childPID int) error
	GracefulStopTimeout time.Duration
	ForceStopTimeout    time.Duration
	VersionProbeTimeout time.Duration
	Progress            ProgressFunc
	Publisher           homePublisher
}

// Launcher owns the codex terminal process. All public methods are safe for
// concurrent use. Exactly one run is owned at a time; a running process is
// always registered in l.run before Status reports running, so Stop (and the
// App's shutdown) can always reach it.
type Launcher struct {
	mu sync.Mutex

	runner          processRunner
	discover        discoverFunc
	sendCtrlBreak   func(ctx context.Context, childPID int) error
	gracefulTimeout time.Duration
	forceTimeout    time.Duration
	probeTimeout    time.Duration
	progress        ProgressFunc
	publisher       homePublisher
	recovery        *recoveryPublisher // production default behind publisher

	status     Status
	run        *runState
	gen        uint64
	pid        int
	codexHome  string
	startedAt  time.Time
	stoppedAt  time.Time
	exitCode   *int
	stopReason StopReason
	lastErr    string
}

// runState is the ownership record for one terminal session. The run's monitor
// goroutine is the only Wait caller; handle close happens exactly once inside
// finishOnce; Stop only requests termination and waits on done.
type runState struct {
	generation uint64
	process    ProcessHandle
	done       chan struct{}
	waitErr    error
	exitCode   *int
	finishOnce sync.Once
}

func New(opts Options) *Launcher {
	l := &Launcher{
		runner:          opts.Runner,
		discover:        opts.Discover,
		sendCtrlBreak:   opts.SendCtrlBreak,
		gracefulTimeout: opts.GracefulStopTimeout,
		forceTimeout:    opts.ForceStopTimeout,
		probeTimeout:    opts.VersionProbeTimeout,
		progress:        opts.Progress,
		publisher:       opts.Publisher,
		status:          StatusIdle,
	}
	if l.publisher == nil {
		l.recovery = &recoveryPublisher{}
		l.publisher = l.recovery
	}
	if l.runner == nil {
		l.runner = newProcessRunner()
	}
	if l.discover == nil {
		l.discover = discoverCodex
	}
	if l.sendCtrlBreak == nil {
		l.sendCtrlBreak = platformSendCtrlBreak
	}
	if l.gracefulTimeout <= 0 {
		l.gracefulTimeout = defaultGracefulStopTimeout
	}
	if l.forceTimeout <= 0 {
		l.forceTimeout = defaultForceStopTimeout
	}
	if l.probeTimeout <= 0 {
		l.probeTimeout = defaultProbeTimeout
	}
	return l
}

func (l *Launcher) SetProgress(fn ProgressFunc) {
	l.mu.Lock()
	l.progress = fn
	l.mu.Unlock()
}

func (l *Launcher) emitProgress(stage, detail string) {
	l.mu.Lock()
	fn := l.progress
	l.mu.Unlock()
	if fn != nil {
		fn(stage, detail)
	}
}

// Status returns the current state snapshot.
func (l *Launcher) Status() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentStateLocked()
}

// Launch detects codex, generates and publishes the codex-home files, then
// starts the visible PowerShell terminal. ctx is the run-wide parent context:
// shutdown cancels it so a launch in progress is interrupted between stages.
func (l *Launcher) Launch(ctx context.Context, opts LaunchOptions) (State, error) {
	l.mu.Lock()
	switch l.status {
	case StatusStarting, StatusRunning, StatusStopping:
		l.mu.Unlock()
		return State{}, &Error{Kind: KindAlreadyRunning, Message: "codex is already running"}
	}
	l.gen++
	gen := l.gen
	l.status = StatusStarting
	l.lastErr = ""
	l.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return l.failLaunch(gen, err)
	}

	l.emitProgress("detecting_codex", "")
	exe, err := l.discover(ctx, l.probeTimeout)
	if err != nil {
		return l.failLaunch(gen, err)
	}
	if err := ctx.Err(); err != nil {
		return l.failLaunch(gen, err)
	}

	l.emitProgress("checking_gateway", "")
	if err := ctx.Err(); err != nil {
		return l.failLaunch(gen, err)
	}

	l.emitProgress("checking_route", "")
	if err := checkRoute(opts); err != nil {
		return l.failLaunch(gen, err)
	}
	if err := ctx.Err(); err != nil {
		return l.failLaunch(gen, err)
	}

	staging, err := CreateStagingHome(opts.CodexHome)
	if err != nil {
		return l.failLaunch(gen, asErrorKind(err, KindConfigGenerationFailed))
	}
	defer os.RemoveAll(staging)

	// auth.json is a conditional artifact: it is published only when a server
	// token is configured (otherwise config.toml declares no requires_openai_auth
	// and a stale auth.json is removed within the publish transaction).
	requireAuth := opts.AuthToken != ""

	l.emitProgress("generating_config", "")
	if err := GenerateAndVerify(staging, generateStaging(opts), requireAuth); err != nil {
		return l.failLaunch(gen, asErrorKind(err, KindConfigGenerationFailed))
	}
	if err := ctx.Err(); err != nil {
		return l.failLaunch(gen, err)
	}

	l.emitProgress("publishing_config", "")
	if err := publishStaged(ctx, l.publisher, staging, opts.CodexHome, requireAuth); err != nil {
		return l.failLaunch(gen, err)
	}
	if err := ctx.Err(); err != nil {
		return l.failLaunch(gen, err)
	}

	l.emitProgress("launching_terminal", "")
	workDir, err := resolveProjectDir(opts.ProjectDirectory, opts.CodexHome)
	if err != nil {
		return l.failLaunch(gen, err)
	}
	env := MergeEnv(os.Environ(), map[string]string{
		"CODEX_HOME":           opts.CodexHome,
		"MOONBRIDGE_CODEX_EXE": exe,
	})
	handle, err := l.runner.Start(ctx, startOptions{
		Executable:  ResolvePowerShellPath(os.Getenv("SystemRoot")),
		CommandLine: PowerShellCommand(),
		WorkingDir:  workDir,
		Env:         env,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return l.failLaunch(gen, err)
		}
		return l.failLaunch(gen, asErrorKind(err, KindStartFailed))
	}

	l.mu.Lock()
	if gen != l.gen {
		l.mu.Unlock()
		_ = handle.Terminate()
		_ = handle.Close()
		return State{}, context.Canceled
	}
	run := &runState{generation: gen, process: handle, done: make(chan struct{})}
	l.run = run
	l.status = StatusRunning
	l.pid = handle.PID()
	l.codexHome = opts.CodexHome
	l.startedAt = time.Now()
	l.stoppedAt = time.Time{}
	l.exitCode = nil
	l.stopReason = ""
	l.mu.Unlock()

	go l.monitorRun(gen, run)

	l.emitProgress("complete", "")
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentStateLocked(), nil
}

// Stop requests a graceful CTRL_BREAK stop and falls back to a forced job
// termination after GracefulStopTimeout. It is idempotent and independent of
// the gateway. reason is recorded unless the actual exit path was a force
// (timeout_force) or a self-exit already recorded by the monitor.
func (l *Launcher) Stop(ctx context.Context, reason StopReason) (State, error) {
	l.mu.Lock()
	run := l.run
	if run == nil {
		st := l.currentStateLocked()
		l.mu.Unlock()
		return st, nil
	}
	l.status = StatusStopping
	l.stopReason = reason
	l.mu.Unlock()

	if err := l.sendCtrlBreak(ctx, run.process.PID()); err == nil {
		if waitDone(ctx, run.done, l.gracefulTimeout) {
			return l.finishStop(run, false)
		}
	}
	_ = run.process.Terminate()
	if waitDone(ctx, run.done, l.forceTimeout) {
		return l.finishStop(run, true)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentStateLocked(), &Error{Kind: KindStopFailed, Message: "codex did not stop within the force timeout"}
}

// Restart stops the current session (always allowed) and launches a new one.
func (l *Launcher) Restart(ctx context.Context, opts LaunchOptions) (State, error) {
	if _, err := l.Stop(ctx, StopReasonGraceful); err != nil {
		return State{}, err
	}
	return l.Launch(ctx, opts)
}

func (l *Launcher) finishStop(run *runState, forced bool) (State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if forced {
		l.stopReason = StopReasonTimeoutForce
	}
	return l.currentStateLocked(), nil
}

func (l *Launcher) monitorRun(gen uint64, run *runState) {
	waitErr := run.process.Wait(context.Background())
	exitCode, _ := run.process.ExitCode()
	run.finishOnce.Do(func() { _ = run.process.Close() })

	l.mu.Lock()
	defer l.mu.Unlock()
	if gen != l.gen {
		close(run.done)
		return
	}
	run.waitErr = waitErr
	run.exitCode = exitCode
	l.exitCode = exitCode
	l.stoppedAt = time.Now()
	if l.stopReason == "" {
		l.stopReason = StopReasonTerminalClosed
	}
	l.status = StatusStopped
	l.run = nil
	close(run.done)
}

func (l *Launcher) failLaunch(gen uint64, err error) (State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if gen != l.gen {
		return State{}, err
	}
	l.status = StatusError
	l.lastErr = err.Error()
	l.run = nil
	l.pid = 0
	return l.currentStateLocked(), err
}

func (l *Launcher) currentStateLocked() State {
	return State{
		Status:     l.status,
		PID:        l.pid,
		CodexHome:  l.codexHome,
		StartedAt:  l.startedAt,
		StoppedAt:  l.stoppedAt,
		ExitCode:   l.exitCode,
		StopReason: l.stopReason,
		Error:      l.lastErr,
	}
}

func waitDone(ctx context.Context, done chan struct{}, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	case <-ctx.Done():
		return false
	}
}

// checkRoute verifies the model alias resolves through the provider config so
// we never publish a codex-home pointing at a route that cannot exist.
func checkRoute(opts LaunchOptions) error {
	route, ok := opts.ProviderCfg.Routes[opts.ModelAlias]
	if !ok || route.Provider == "" {
		return &Error{Kind: KindRouteNotFound, Message: "codex route " + opts.ModelAlias + " not found"}
	}
	if _, ok := opts.ProviderCfg.Providers[route.Provider]; !ok {
		return &Error{Kind: KindRouteNotFound, Message: "codex route provider not found"}
	}
	return nil
}

// generateStaging writes config.toml into stagingHome and lets
// GenerateConfigToml write models_catalog.json and auth.json alongside it. The
// generated config.toml embeds `model_catalog_json = <stagingHome>/models_catalog.json`;
// that path must be rewritten to the final codex home (opts.CodexHome), because
// the staging directory is removed after publish — otherwise codex would point
// at a deleted catalog.
func generateStaging(opts LaunchOptions) GenerateConfigFunc {
	return func(stagingHome string) error {
		var buf bytes.Buffer
		if err := codex.GenerateConfigToml(&buf, opts.ModelAlias, opts.BaseURL, stagingHome,
			opts.ProviderCfg, opts.PluginCfg, opts.ServerCfg); err != nil {
			return err
		}
		// GenerateConfigToml %q-escapes the path, so replace the quoted (escaped)
		// form. The published value must reference opts.CodexHome, not staging.
		stagingQuoted := strconv.Quote(filepath.Join(stagingHome, "models_catalog.json"))
		finalQuoted := strconv.Quote(filepath.Join(opts.CodexHome, "models_catalog.json"))
		content := strings.ReplaceAll(buf.String(), stagingQuoted, finalQuoted)
		return os.WriteFile(filepath.Join(stagingHome, "config.toml"), []byte(content), 0o600)
	}
}

// recoveryPublisher is the production homePublisher: a publishrecovery.Service
// rooted at %LOCALAPPDATA%\Moon Bridge\recovery. The root is resolved and the
// Service built lazily on first publish, so an init failure (missing/relative
// recovery root, or a journal left in an inconsistent state) surfaces as a
// launch error rather than being silently retried through the old non-journal
// publish. The Service is held for the launcher's lifetime (plan E): it is
// reused across publishes and guards the single-journal slot itself.
type recoveryPublisher struct {
	mu      sync.Mutex
	svc     *publishrecovery.Service
	initErr error
}

func (p *recoveryPublisher) Publish(ctx context.Context, in publishrecovery.PublishInput) error {
	svc, err := p.service()
	if err != nil {
		return err
	}
	return svc.Publish(ctx, in)
}

func (p *recoveryPublisher) service() (*publishrecovery.Service, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.svc != nil || p.initErr != nil {
		return p.svc, p.initErr
	}
	root, err := recovery.DefaultDir(os.Getenv)
	if err != nil {
		p.initErr = err
		return nil, err
	}
	recoveryDir := filepath.Join(root, "recovery")
	svc, err := publishrecovery.New(publishrecovery.ServiceOptions{RecoveryDir: recoveryDir})
	if err != nil {
		p.initErr = err
		return nil, err
	}
	p.svc = svc
	return svc, nil
}

// resolveProjectDir validates the caller's ProjectDirectory and returns the
// absolute working directory for CreateProcess. Empty falls back to codex-home.
func resolveProjectDir(projectDir, codexHome string) (string, error) {
	if projectDir == "" {
		return codexHome, nil
	}
	if !filepath.IsAbs(projectDir) {
		return "", &Error{Kind: KindProjectInvalid, Message: "project directory must be absolute"}
	}
	if isUncPath(projectDir) {
		return "", &Error{Kind: KindProjectInvalid, Message: "UNC project directories are not supported"}
	}
	info, err := os.Stat(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", &Error{Kind: KindProjectNotFound, Message: "project directory does not exist"}
		}
		return "", err
	}
	if !info.IsDir() {
		return "", &Error{Kind: KindProjectNotDirectory, Message: "project path is not a directory"}
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
