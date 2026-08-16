package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/trafficanalysis"
)

// setupTrafficIntegrationRoot creates a disposable gateway/Codex/backup root.
// On Windows it sets LOCALAPPDATA so any backup-related platform helper that
// resolves the trusted base uses the temp tree instead of the real profile.
func setupTrafficIntegrationRoot(t *testing.T) (root, codexHome, backupDir, recoveryDir string) {
	t.Helper()
	root = t.TempDir()
	if runtime.GOOS == "windows" {
		// The Windows backup implementation anchors BackupDir beneath
		// LOCALAPPDATA. Keep the fixture self-contained and avoid comparing a
		// hosted runner's RUNNER~1 temp spelling with its long profile spelling.
		t.Setenv("LOCALAPPDATA", root)
	}
	codexHome = filepath.Join(root, "codex")
	backupDir = filepath.Join(root, "backups")
	recoveryDir = filepath.Join(root, "recovery")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, codexHome, backupDir, recoveryDir
}

// TestRealTrafficBindingLifecycle exercises the Wails-facing bindings against
// the real in-process Gateway and Capture services. It uses isolated gateway,
// Codex, Recovery, and backup roots; no user profile or fixed config is read.
func TestRealTrafficBindingLifecycle(t *testing.T) {
	if listener, err := net.Listen("tcp", "127.0.0.1:38441"); err != nil {
		t.Skipf("capture listener 127.0.0.1:38441 is unavailable: %v", err)
	} else {
		_ = listener.Close()
	}

	root, codexHome, backupDir, recoveryDir := setupTrafficIntegrationRoot(t)
	codexPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gatewayPath := filepath.Join(root, "gateway.yml")
	gatewayYAML := "mode: CaptureAnthropic\n" +
		"server:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  auth_token: integration-server-token\n" +
		"proxy:\n" +
		"  anthropic:\n" +
		"    base_url: https://api.example.invalid\n" +
		"    api_key: integration-upstream-key\n" +
		"    version: 2023-06-01\n"
	if err := os.WriteFile(gatewayPath, []byte(gatewayYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   recoveryDir,
		CodexHome:     codexHome,
		BackupDir:     backupDir,
		TrafficLogDir: filepath.Join(root, "logs", "traffic-analysis"),
		AppDataRoot:   filepath.Join(root, "appdata"),
	}, filepath.Join(recoveryDir, "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(AppOptions{
		Service:      gateway.NewService(gateway.ServiceOptions{Errors: os.Stderr}),
		ConfigPath:   gatewayPath,
		CodexConfig:  codexconfig.New(codexconfig.Options{Home: codexHome, BackupDir: backupDir}),
		Recovery:     store,
		RecoveryHome: codexHome,
		BackupDir:    backupDir,
		EmitEvents:   noopEmit,
	})
	// The injected Recovery store skips the create branch of ensureRecoveryStore,
	// so the autosave log dir is never derived. Pin it to the temp root to keep
	// the real user profile untouched.
	app.trafficLogDir = filepath.Join(root, "logs", "traffic-analysis")
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	started := app.StartGateway(StartGatewayRequest{})
	if !started.OK || started.Value == nil || started.Value.State != "running" {
		t.Fatalf("StartGateway() = %#v, want running", started)
	}
	if snapshot, err := app.codexConfig.Load(context.Background()); err != nil {
		t.Fatalf("CodexConfig.Load() error = %v", err)
	} else if snapshot.Path != codexPath {
		t.Fatalf("CodexConfig.Load() path = %q, want %q", snapshot.Path, codexPath)
	}
	if _, err := app.ensureTrafficTransaction(); err != nil {
		t.Fatalf("ensureTrafficTransaction() error = %v (recoveryHome=%q configHome=%q)", err, app.recoveryHome, filepath.Dir(codexPath))
	}
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("initial recovery Load() error = %v", err)
	}
	if state == nil || state.IntegrationTarget != recovery.TargetGateway || !state.IntegrationActive {
		t.Fatalf("initial recovery state = %#v, want gateway integration active", state)
	}
	if unresolved, err := (trafficRecoveryWriter{store: store, configHome: codexHome, backupDir: backupDir}).HasUnresolved(context.Background()); err != nil || unresolved {
		t.Fatalf("initial recovery unresolved = %v, error=%v, want false", unresolved, err)
	}
	if state := app.traffic.Status(); state.Mode != "idle" || state.CaptureState != "stopped" {
		t.Fatalf("initial traffic state = %#v, want idle/stopped", state)
	}
	if state := app.traffic.Status(); state.GatewayInstanceID == "" || state.GatewayAddress == "" {
		t.Fatalf("initial bound traffic identity = %#v, want gateway identity", state)
	}

	trafficStarted := app.StartTrafficAnalysis()
	if !trafficStarted.OK || trafficStarted.Value == nil || trafficStarted.Value.TrafficAnalysis == nil {
		state, _ := store.Load(context.Background())
		t.Fatalf("StartTrafficAnalysis() = %#v, error=%#v, recovery=%#v, traffic=%#v, want success", trafficStarted, trafficStarted.Error, state, app.traffic.Status())
	}
	traffic := trafficStarted.Value.TrafficAnalysis
	if traffic.Mode != "desktop_managed" || !traffic.Listening || !traffic.IntegrationActive {
		t.Fatalf("traffic snapshot = %#v, want desktop-managed/listening/integrated", traffic)
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:38441", time.Second)
	if err != nil {
		t.Fatalf("capture listener is not reachable: %v", err)
	}
	_ = conn.Close()

	paused := app.StopTrafficAnalysis()
	if !paused.OK || paused.Value == nil || paused.Value.TrafficAnalysis == nil {
		state, _ := store.Load(context.Background())
		t.Fatalf("StopTrafficAnalysis() = %#v, error=%#v, recovery=%#v, traffic=%#v, want success", paused, paused.Error, state, app.traffic.Status())
	}
	if paused.Value.TrafficAnalysis.Mode != "capture_only" || paused.Value.TrafficAnalysis.CaptureState != "passthrough" {
		t.Fatalf("paused traffic snapshot = %#v, want capture_only/passthrough", paused.Value.TrafficAnalysis)
	}

	finished := app.FinishTrafficRelay(FinishTrafficRelayRequest{})
	if !finished.OK || finished.Value == nil || finished.Value.TrafficAnalysis == nil {
		t.Fatalf("FinishTrafficRelay() = %#v, want success", finished)
	}
	if finished.Value.TrafficAnalysis.Mode != "idle" || finished.Value.TrafficAnalysis.Listening {
		t.Fatalf("finished traffic snapshot = %#v, want idle/not listening", finished.Value.TrafficAnalysis)
	}

	probe, err := net.Listen("tcp", "127.0.0.1:38441")
	if err != nil {
		t.Fatalf("capture listener was not released after FinishTrafficRelay: %v", err)
	}
	_ = probe.Close()

	stopped := app.StopGateway(StopGatewayRequest{})
	if !stopped.OK {
		t.Fatalf("StopGateway() = %#v, want success", stopped)
	}
	if st := app.GatewayStatus(); !st.OK || st.Value == nil || st.Value.State != "stopped" {
		t.Fatalf("GatewayStatus() after stop = %#v, want stopped", st)
	}
}

// TestLiveRestoreConflictResolutionFlow drives the Plan 4t resolution flow
// against the real in-process Gateway, Capture, and Recovery services. It
// reproduces the live recovery dead-end (config changed externally while the
// integration is active), then proves the existing RestoreRecovery API resolves
// it without a process restart: unconfirmed restore fails closed and stays
// retryable, confirmed restore releases Desktop ownership only on success, and
// Finish then completes.
func TestLiveRestoreConflictResolutionFlow(t *testing.T) {
	if listener, err := net.Listen("tcp", "127.0.0.1:38441"); err != nil {
		t.Skipf("capture listener 127.0.0.1:38441 is unavailable: %v", err)
	} else {
		_ = listener.Close()
	}

	root, codexHome, backupDir, recoveryDir := setupTrafficIntegrationRoot(t)
	original := "model = \"gpt-test\"\n"
	codexPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(codexPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	gatewayPath := filepath.Join(root, "gateway.yml")
	gatewayYAML := "mode: CaptureAnthropic\n" +
		"server:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  auth_token: integration-server-token\n" +
		"proxy:\n" +
		"  anthropic:\n" +
		"    base_url: https://api.example.invalid\n" +
		"    api_key: integration-upstream-key\n" +
		"    version: 2023-06-01\n"
	if err := os.WriteFile(gatewayPath, []byte(gatewayYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   recoveryDir,
		CodexHome:     codexHome,
		BackupDir:     backupDir,
		TrafficLogDir: filepath.Join(root, "logs", "traffic-analysis"),
		AppDataRoot:   filepath.Join(root, "appdata"),
	}, filepath.Join(recoveryDir, "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(AppOptions{
		Service:      gateway.NewService(gateway.ServiceOptions{Errors: os.Stderr}),
		ConfigPath:   gatewayPath,
		CodexConfig:  codexconfig.New(codexconfig.Options{Home: codexHome, BackupDir: backupDir}),
		Recovery:     store,
		RecoveryHome: codexHome,
		BackupDir:    backupDir,
		EmitEvents:   noopEmit,
	})
	app.trafficLogDir = filepath.Join(root, "logs", "traffic-analysis")
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	started := app.StartGateway(StartGatewayRequest{})
	if !started.OK || started.Value == nil || started.Value.State != "running" {
		t.Fatalf("StartGateway() = %#v, want running", started)
	}
	if _, err := app.ensureTrafficTransaction(); err != nil {
		t.Fatalf("ensureTrafficTransaction() error = %v", err)
	}
	trafficStarted := app.StartTrafficAnalysis()
	if !trafficStarted.OK || trafficStarted.Value == nil || trafficStarted.Value.TrafficAnalysis == nil {
		t.Fatalf("StartTrafficAnalysis() = %#v, error=%#v, want success", trafficStarted, trafficStarted.Error)
	}
	traffic := trafficStarted.Value.TrafficAnalysis
	if traffic.Mode != string(trafficanalysis.ModeDesktop) || !traffic.Listening || !traffic.IntegrationActive {
		t.Fatalf("traffic snapshot = %#v, want desktop-managed/listening/integrated", traffic)
	}

	// Externally modify the managed config so Disable must fail closed with a
	// restore conflict: the current hash no longer matches the journal's
	// ConfigHashAfterApply.
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-test\"\nopenai_base_url = \"http://127.0.0.1:39999\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stop := app.StopTrafficAnalysis()
	if stop.OK || stop.Error == nil || stop.Error.Code != "traffic_config_restore_conflict" || !stop.Error.ConfirmationRequired {
		t.Fatalf("StopTrafficAnalysis() = %#v, want traffic_config_restore_conflict confirmation required", stop)
	}

	status := app.TrafficAnalysisStatus()
	if !status.OK || status.Value == nil || status.Value.Recovery == nil || status.Value.TrafficAnalysis == nil {
		t.Fatalf("TrafficAnalysisStatus() = %#v, want success", status)
	}
	rec := status.Value.Recovery
	if !rec.Exists || rec.Phase != string(recovery.PhaseReconciliationReq) || rec.ReconciliationStatus != string(recovery.StatusConfigConflict) {
		t.Fatalf("recovery snapshot = %#v, want reconciliation_required/config_conflict", rec)
	}
	if !rec.RecoveryRequired || !rec.Conflict || !rec.IntegrationActive {
		t.Fatalf("recovery snapshot = %#v, want recoveryRequired+conflict+integrationActive", rec)
	}
	if status.Value.TrafficAnalysis.Mode != string(trafficanalysis.ModeDesktop) || status.Value.TrafficAnalysis.CaptureState != "passthrough" || !status.Value.TrafficAnalysis.IntegrationActive {
		t.Fatalf("traffic snapshot = %#v, want desktop_managed/passthrough/integrated (ownership kept)", status.Value.TrafficAnalysis)
	}

	// Unconfirmed restore must fail closed with confirmation required while
	// leaving the state fully retryable: Desktop ownership is not released, the
	// conflict is still surfaced, and a confirmed retry can still succeed.
	blocked := app.RestoreRecovery(RestoreRecoveryInput{})
	if blocked.OK || blocked.Error == nil || blocked.Error.Code != "recovery_config_conflict" || !blocked.Error.ConfirmationRequired {
		t.Fatalf("unconfirmed RestoreRecovery() = %#v, want recovery_config_conflict confirmation required", blocked)
	}
	status = app.TrafficAnalysisStatus()
	if !status.OK || status.Value == nil || status.Value.Recovery == nil || status.Value.TrafficAnalysis == nil {
		t.Fatalf("TrafficAnalysisStatus() after blocked restore = %#v, want success", status)
	}
	if status.Value.TrafficAnalysis.Mode != string(trafficanalysis.ModeDesktop) || status.Value.TrafficAnalysis.CaptureState != "passthrough" {
		t.Fatalf("traffic after blocked restore = %#v, want desktop_managed/passthrough (still retryable)", status.Value.TrafficAnalysis)
	}
	if status.Value.Recovery.ReconciliationStatus != string(recovery.StatusConfigConflict) || !status.Value.Recovery.RecoveryRequired {
		t.Fatalf("recovery after blocked restore = %#v, want config_conflict/recoveryRequired (still retryable)", status.Value.Recovery)
	}

	confirmed := app.RestoreRecovery(RestoreRecoveryInput{ConfirmConflict: true})
	if !confirmed.OK || confirmed.Value == nil || confirmed.Value.Recovery == nil || confirmed.Value.TrafficAnalysis == nil {
		t.Fatalf("confirmed RestoreRecovery() = %#v, want success", confirmed)
	}
	content, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "openai_base_url") {
		t.Fatalf("config after restore still carries openai_base_url: %s", content)
	}
	if confirmed.Value.Recovery.ReconciliationStatus != string(recovery.StatusAlreadyRestored) || confirmed.Value.Recovery.RecoveryRequired || confirmed.Value.Recovery.IntegrationActive {
		t.Fatalf("recovery after confirmed restore = %#v, want already_restored/not-required/inactive", confirmed.Value.Recovery)
	}
	if confirmed.Value.TrafficAnalysis.Mode != string(trafficanalysis.ModeCaptureOnly) || confirmed.Value.TrafficAnalysis.CaptureState != "passthrough" || confirmed.Value.TrafficAnalysis.IntegrationActive {
		t.Fatalf("traffic after confirmed restore = %#v, want capture_only/passthrough/inactive (ownership released)", confirmed.Value.TrafficAnalysis)
	}

	finished := app.FinishTrafficRelay(FinishTrafficRelayRequest{})
	if !finished.OK || finished.Value == nil || finished.Value.TrafficAnalysis == nil {
		t.Fatalf("FinishTrafficRelay() = %#v, want success", finished)
	}
	if finished.Value.TrafficAnalysis.Mode != string(trafficanalysis.ModeIdle) || finished.Value.TrafficAnalysis.Listening {
		t.Fatalf("finished traffic snapshot = %#v, want idle/not listening", finished.Value.TrafficAnalysis)
	}
	if finished.Value.Recovery == nil || finished.Value.Recovery.IntegrationActive || finished.Value.Recovery.RecoveryRequired {
		t.Fatalf("recovery after finish = %#v, want inactive/not-required", finished.Value.Recovery)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:38441")
	if err != nil {
		t.Fatalf("capture listener was not released after the resolution flow: %v", err)
	}
	_ = probe.Close()

	stopped := app.StopGateway(StopGatewayRequest{})
	if !stopped.OK {
		t.Fatalf("StopGateway() = %#v, want success", stopped)
	}
}

// TestTrafficStatusCountersReflectCapture drives the real Capture service
// through the App traffic bindings and sends one real request with sentinels.
// It verifies the Wails snapshot carries live HTTP/observation counters and the
// real RingBuffer capacity, and that the observations binding strips the
// sentinels at the backend boundary.
func TestTrafficStatusCountersReflectCapture(t *testing.T) {
	if listener, err := net.Listen("tcp", "127.0.0.1:38441"); err != nil {
		t.Skipf("capture listener 127.0.0.1:38441 is unavailable: %v", err)
	} else {
		_ = listener.Close()
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	app := NewApp(AppOptions{})
	if _, err := app.traffic.BindGatewayRun("binding-test-gateway", "127.0.0.1:38440"); err != nil {
		t.Fatalf("BindGatewayRun() error = %v", err)
	}
	started, err := app.traffic.StartCapture(trafficanalysis.StartOptions{
		UpstreamBase: upstream.URL,
		ListenAddr:   "127.0.0.1:38441",
	})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	if started.CaptureState != "capturing" {
		t.Fatalf("StartCapture() state = %q, want capturing", started.CaptureState)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = app.traffic.StopCapture(ctx)
	}()

	body := `{"prompt":"SENTINEL_PROMPT","model":"SENTINEL_MODEL","api_key":"SENTINEL_KEY"}`
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:38441/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer SENTINEL_AUTH")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("capture request error = %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if st := app.traffic.Status(); st.HTTPRequests >= 1 && st.ObservationCount >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture did not record the request: status = %#v", app.traffic.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}

	status := app.TrafficAnalysisStatus()
	if !status.OK || status.Value == nil || status.Value.TrafficAnalysis == nil {
		t.Fatalf("TrafficAnalysisStatus() = %#v, want success", status)
	}
	ta := status.Value.TrafficAnalysis
	if ta.HTTPRequests < 1 {
		t.Fatalf("snapshot HTTPRequests = %d, want >= 1", ta.HTTPRequests)
	}
	if ta.ObservationCount < 1 {
		t.Fatalf("snapshot ObservationCount = %d, want >= 1", ta.ObservationCount)
	}
	if ta.ObservationCapacity != uint64(trafficanalysis.DefaultRingCapacity) {
		t.Fatalf("snapshot ObservationCapacity = %d, want %d", ta.ObservationCapacity, trafficanalysis.DefaultRingCapacity)
	}

	observations := app.TrafficAnalysisObservations()
	if !observations.OK || observations.Value == nil {
		t.Fatalf("TrafficAnalysisObservations() = %#v, want success", observations)
	}
	if len(observations.Value.TrafficObservations) < 1 {
		t.Fatalf("TrafficAnalysisObservations() len = %d, want >= 1", len(observations.Value.TrafficObservations))
	}
	encoded, err := json.Marshal(observations)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"SENTINEL_PROMPT", "SENTINEL_MODEL", "SENTINEL_KEY", "SENTINEL_AUTH", "Bearer", "/responses"} {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("observations binding leaked sentinel %q: %s", sentinel, encoded)
		}
	}
}
