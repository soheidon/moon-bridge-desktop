package codexlauncher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/config"
)

const testExe = `C:\Users\test\bin\codex.exe`

func testLaunchOptions(codexHome string) LaunchOptions {
	return LaunchOptions{
		CodexHome:  codexHome,
		ModelAlias: "moonbridge",
		BaseURL:    "http://127.0.0.1:38440/v1",
		AuthToken:  "sk-test-launch-token-1234567890",
		ProviderCfg: config.ProviderConfig{
			Providers: map[string]config.ProviderDef{
				"deepseek": {BaseURL: "http://127.0.0.1:38440/v1"},
			},
			Routes: map[string]config.RouteEntry{
				"moonbridge": {Provider: "deepseek", Model: "deepseek-v4-pro"},
			},
		},
		ServerCfg: config.ServerConfig{AuthToken: "sk-test-launch-token-1234567890"},
	}
}

type fakeProcess struct {
	pid         int
	exited      chan struct{}
	code        int
	terminateErr error // when set, Terminate fails without exiting the process
	mu          sync.Mutex
	closeCalls  int
}

func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{pid: pid, exited: make(chan struct{})}
}

func (p *fakeProcess) PID() int { return p.pid }

func (p *fakeProcess) Wait(ctx context.Context) error {
	<-p.exited
	return nil
}

func (p *fakeProcess) Terminate() error {
	if p.terminateErr != nil {
		return p.terminateErr
	}
	p.exit(1)
	return nil
}

func (p *fakeProcess) Close() error {
	p.mu.Lock()
	p.closeCalls++
	p.mu.Unlock()
	return nil
}

func (p *fakeProcess) ExitCode() (*int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.exited:
		ec := p.code
		return &ec, nil
	default:
		return nil, errors.New("not exited")
	}
}

func (p *fakeProcess) exit(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.exited:
	default:
		p.code = code
		close(p.exited)
	}
}

func (p *fakeProcess) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCalls
}

type fakeRunner struct {
	mu      sync.Mutex
	nextPID int
	started []*fakeProcess
	opts    []startOptions
	startFn func(ctx context.Context, opts startOptions) (ProcessHandle, error)
	entered chan struct{}
}

func (r *fakeRunner) Start(ctx context.Context, opts startOptions) (ProcessHandle, error) {
	if r.entered != nil {
		select {
		case r.entered <- struct{}{}:
		default:
		}
	}
	if r.startFn != nil {
		return r.startFn(ctx, opts)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts = append(r.opts, opts)
	r.nextPID++
	p := newFakeProcess(r.nextPID)
	r.started = append(r.started, p)
	return p, nil
}

func (r *fakeRunner) exitByPID(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.started {
		if p.pid == pid {
			p.exit(0)
			return
		}
	}
}

func (r *fakeRunner) lastOpts() startOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opts[len(r.opts)-1]
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opts)
}

func (r *fakeRunner) process(pid int) *fakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.started {
		if p.pid == pid {
			return p
		}
	}
	return nil
}

func fakeDiscover(ctx context.Context, timeout time.Duration) (string, error) {
	return testExe, nil
}

func newTestLauncher(runner *fakeRunner, ctrlBreak func(ctx context.Context, pid int) error) *Launcher {
	return New(Options{
		Runner:              runner,
		Discover:            fakeDiscover,
		SendCtrlBreak:       ctrlBreak,
		GracefulStopTimeout: 50 * time.Millisecond,
		ForceStopTimeout:    50 * time.Millisecond,
		VersionProbeTimeout: 100 * time.Millisecond,
	})
}

func gracefulStub(runner *fakeRunner) func(ctx context.Context, pid int) error {
	return func(ctx context.Context, pid int) error {
		runner.exitByPID(pid)
		return nil
	}
}

func waitForStatus(t *testing.T, l *Launcher, want Status) State {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := l.Status()
		if st.Status == want {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("status never became %s, last %+v", want, l.Status())
	return State{}
}

func launchOK(t *testing.T, l *Launcher, codexHome string) State {
	t.Helper()
	st, err := l.Launch(context.Background(), testLaunchOptions(codexHome))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	if st.Status != StatusRunning {
		t.Fatalf("expected running, got %+v", st)
	}
	return st
}

func TestLaunchLifecycleTerminalClosedOnSelfExit(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	codexHome := filepath.Join(t.TempDir(), "codex-home")

	st := launchOK(t, l, codexHome)
	if st.PID <= 0 {
		t.Fatalf("expected a PID, got %d", st.PID)
	}
	// The terminal closes on its own → terminal_closed.
	runner.exitByPID(st.PID)
	after := waitForStatus(t, l, StatusStopped)
	if after.StopReason != StopReasonTerminalClosed {
		t.Fatalf("expected terminal_closed, got %q", after.StopReason)
	}
}

func TestLaunchPublishesCodexHomeAndCleansStaging(t *testing.T) {
	parent := t.TempDir()
	codexHome := filepath.Join(parent, "codex-home")
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))

	launchOK(t, l, codexHome)
	for _, name := range codexHomeFiles {
		if _, err := os.Stat(filepath.Join(codexHome, name)); err != nil {
			t.Fatalf("published file missing: %s: %v", name, err)
		}
	}
	// The published config.toml's model_catalog_json must point at the final
	// codex home, never at the (now-removed) staging directory.
	cfgTOML, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfgTOML), ".codex-home-staging") {
		t.Fatalf("published config.toml references the staging directory:\n%s", cfgTOML)
	}
	if !strings.Contains(string(cfgTOML), strconv.Quote(filepath.Join(codexHome, "models_catalog.json"))) {
		t.Fatalf("published config.toml model_catalog_json should point at %q:\n%s",
			filepath.Join(codexHome, "models_catalog.json"), cfgTOML)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".codex-home-staging") {
			t.Fatalf("staging dir not cleaned: %q", e.Name())
		}
	}
	got := runner.lastOpts()
	if got.WorkingDir != codexHome {
		t.Fatalf("empty project dir must fall back to codex-home, got %q", got.WorkingDir)
	}
	if !hasEnv(got.Env, "CODEX_HOME="+codexHome) || !hasEnv(got.Env, "MOONBRIDGE_CODEX_EXE="+testExe) {
		t.Fatalf("launch env missing overrides: %v", got.Env)
	}
	if strings.Contains(got.CommandLine, "codex.exe") || strings.Contains(got.CommandLine, testExe) {
		t.Fatalf("codex path must not appear on the command line: %q", got.CommandLine)
	}
	if _, err := l.Stop(context.Background(), StopReasonShutdown); err != nil {
		t.Fatalf("cleanup stop failed: %v", err)
	}
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if strings.EqualFold(e, want) {
			return true
		}
	}
	return false
}

func TestLaunchDoubleLaunchRejected(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	codexHome := filepath.Join(t.TempDir(), "codex-home")

	launchOK(t, l, codexHome)
	_, err := l.Launch(context.Background(), testLaunchOptions(codexHome))
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindAlreadyRunning {
		t.Fatalf("expected already_running, got %v", err)
	}
	if _, err := l.Stop(context.Background(), StopReasonGraceful); err != nil {
		t.Fatal(err)
	}
	// A stopped launcher can launch again.
	launchOK(t, l, codexHome)
	if _, err := l.Stop(context.Background(), StopReasonGraceful); err != nil {
		t.Fatal(err)
	}
}

func TestStopGracefulRecordsReason(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	launchOK(t, l, filepath.Join(t.TempDir(), "codex-home"))

	st, err := l.Stop(context.Background(), StopReasonGraceful)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if st.Status != StatusStopped || st.StopReason != StopReasonGraceful {
		t.Fatalf("unexpected stop state: %+v", st)
	}
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", st.ExitCode)
	}
}

func TestStopShutdownRecordsReason(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	launchOK(t, l, filepath.Join(t.TempDir(), "codex-home"))

	st, err := l.Stop(context.Background(), StopReasonShutdown)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if st.StopReason != StopReasonShutdown {
		t.Fatalf("expected shutdown, got %q", st.StopReason)
	}
}

func TestStopForceOnHelperFailure(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, func(ctx context.Context, pid int) error {
		return errors.New("helper delivery failed")
	})
	launchOK(t, l, filepath.Join(t.TempDir(), "codex-home"))

	st, err := l.Stop(context.Background(), StopReasonGraceful)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if st.StopReason != StopReasonTimeoutForce {
		t.Fatalf("expected timeout_force after helper failure, got %q", st.StopReason)
	}
	if st.ExitCode == nil || *st.ExitCode != 1 {
		t.Fatalf("expected force exit code 1, got %v", st.ExitCode)
	}
}

func TestStopGracefulTimeoutFallsBackToForce(t *testing.T) {
	runner := &fakeRunner{}
	// SendCtrlBreak succeeds but the process never reacts to CTRL_BREAK.
	l := newTestLauncher(runner, func(ctx context.Context, pid int) error { return nil })
	launchOK(t, l, filepath.Join(t.TempDir(), "codex-home"))

	start := time.Now()
	st, err := l.Stop(context.Background(), StopReasonGraceful)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if st.StopReason != StopReasonTimeoutForce {
		t.Fatalf("expected timeout_force, got %q", st.StopReason)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("force fallback too fast: %v", elapsed)
	}
}

func TestStopIdempotentWhenNotRunning(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	st, err := l.Stop(context.Background(), StopReasonGraceful)
	if err != nil {
		t.Fatalf("Stop on idle must not error: %v", err)
	}
	if st.Status != StatusIdle {
		t.Fatalf("expected idle, got %+v", st)
	}
}

func TestRestartChangesPID(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	codexHome := filepath.Join(t.TempDir(), "codex-home")

	first := launchOK(t, l, codexHome)
	st, err := l.Restart(context.Background(), testLaunchOptions(codexHome))
	if err != nil {
		t.Fatalf("Restart failed: %v", err)
	}
	if st.Status != StatusRunning {
		t.Fatalf("expected running after restart, got %+v", st)
	}
	if st.PID == first.PID || st.PID == 0 {
		t.Fatalf("restart must use a new PID: %d -> %d", first.PID, st.PID)
	}
	if _, err := l.Stop(context.Background(), StopReasonShutdown); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchCancelBeforeStartDoesNotStartProcess(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := l.Launch(ctx, testLaunchOptions(filepath.Join(t.TempDir(), "codex-home")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if n := runner.startCount(); n != 0 {
		t.Fatalf("process started despite cancel: %d", n)
	}
	if st := l.Status(); st.Status != StatusError {
		t.Fatalf("expected error status after canceled launch, got %+v", st)
	}
}

func TestRunnerStartCancelPropagatesAsCancel(t *testing.T) {
	runner := &fakeRunner{startFn: func(ctx context.Context, opts startOptions) (ProcessHandle, error) {
		return nil, context.Canceled
	}}
	l := newTestLauncher(runner, gracefulStub(runner))
	_, err := l.Launch(context.Background(), testLaunchOptions(filepath.Join(t.TempDir(), "codex-home")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled (not start_failed), got %v", err)
	}
}

func TestLaunchCancelDuringStartCleansStaging(t *testing.T) {
	parent := t.TempDir()
	codexHome := filepath.Join(parent, "codex-home")
	runner := &fakeRunner{
		entered: make(chan struct{}, 1),
		startFn: func(ctx context.Context, opts startOptions) (ProcessHandle, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	l := newTestLauncher(runner, gracefulStub(runner))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_, _ = l.Launch(ctx, testLaunchOptions(codexHome))
		close(done)
	}()
	<-runner.entered
	cancel()
	<-done

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".codex-home-staging") {
			t.Fatalf("staging dir leaked on cancel: %q", e.Name())
		}
	}
}

func TestLaunchFailureCleansStaging(t *testing.T) {
	parent := t.TempDir()
	codexHome := filepath.Join(parent, "codex-home")
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))

	// Without a server auth token GenerateConfigToml skips auth.json, so
	// verification fails after staging was created — the failure path must still
	// remove the staging dir (whose auth.json could hold a secret).
	opts := testLaunchOptions(codexHome)
	opts.ServerCfg.AuthToken = ""
	_, err := l.Launch(context.Background(), opts)
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigGenerationFailed {
		t.Fatalf("expected config_generation_failed, got %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".codex-home-staging") {
			t.Fatalf("staging dir leaked on failure: %q", e.Name())
		}
	}
}

func TestStopFailedWhenForceCannotKill(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, func(ctx context.Context, pid int) error {
		return errors.New("helper delivery failed")
	})
	launchOK(t, l, filepath.Join(t.TempDir(), "codex-home"))

	// Make the fake process unkillable: both graceful and force fail to stop it,
	// so Stop runs past the force timeout and returns stop_failed.
	runner.process(1).terminateErr = errors.New("terminate failed")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := l.Stop(ctx, StopReasonGraceful)
	if err == nil {
		t.Fatal("expected stop_failed when force cannot kill")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindStopFailed {
		t.Fatalf("expected stop_failed, got %v", err)
	}
}

func TestHandleClosedExactlyOnce(t *testing.T) {
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	launchOK(t, l, filepath.Join(t.TempDir(), "codex-home"))
	p := runner.process(1)
	if _, err := l.Stop(context.Background(), StopReasonGraceful); err != nil {
		t.Fatal(err)
	}
	if n := p.closeCount(); n != 1 {
		t.Fatalf("handle close must happen exactly once, got %d", n)
	}
}

func TestResolveProjectDir(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex-home")

	if got, err := resolveProjectDir("", codexHome); err != nil || got != codexHome {
		t.Fatalf("empty project dir should fall back to codex-home: %q err=%v", got, err)
	}
	if _, err := resolveProjectDir("relative/project", codexHome); kindOf(err) != KindProjectInvalid {
		t.Fatalf("expected project_invalid for relative path, got %v", err)
	}
	if _, err := resolveProjectDir(filepath.Join(t.TempDir(), "missing"), codexHome); kindOf(err) != KindProjectNotFound {
		t.Fatalf("expected project_not_found, got %v", err)
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProjectDir(file, codexHome); kindOf(err) != KindProjectNotDirectory {
		t.Fatalf("expected project_not_directory, got %v", err)
	}
	if runtime.GOOS == "windows" {
		if _, err := resolveProjectDir(`\\server\share`, codexHome); kindOf(err) != KindProjectInvalid {
			t.Fatalf("expected project_invalid for UNC path, got %v", err)
		}
	}
}

func TestLaunchPassesResolvedProjectDir(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "my project 日本語")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	l := newTestLauncher(runner, gracefulStub(runner))
	opts := testLaunchOptions(filepath.Join(t.TempDir(), "codex-home"))
	opts.ProjectDirectory = proj
	if _, err := l.Launch(context.Background(), opts); err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	if got := runner.lastOpts().WorkingDir; got != filepath.Clean(proj) {
		t.Fatalf("working dir = %q, want %q", got, filepath.Clean(proj))
	}
	if _, err := l.Stop(context.Background(), StopReasonShutdown); err != nil {
		t.Fatal(err)
	}
}

func kindOf(err error) ErrorKind {
	var le *Error
	if !errors.As(err, &le) {
		return ""
	}
	return le.Kind
}
