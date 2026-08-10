package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/trafficanalysis"
)

// emitCapture records every (name, payload) passed to a.safeEmit, mirroring the
// Wails event sink. Unlike app_test.go's eventRecorder it captures any payload
// type (ExitConfirmationPayload included), not just *GatewaySnapshot.
type emitCapture struct {
	mu     sync.Mutex
	events []struct {
		name    string
		payload any
	}
}

func (c *emitCapture) emit(name string, payload any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, struct {
		name    string
		payload any
	}{name, payload})
}

func (c *emitCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (c *emitCapture) lastExit() (ExitConfirmationPayload, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].name == "desktop-exit-confirmation-requested" {
			if p, ok := c.events[i].payload.(ExitConfirmationPayload); ok {
				return p, true
			}
		}
	}
	return ExitConfirmationPayload{}, false
}

// writeRecoveryJSON persists a State directly to the store file, bypassing
// Write's normalizeForWrite (reproduces a persisted legacy/raw state that a
// valid Write would refuse, e.g. a verbatim absolute configPath).
func writeRecoveryJSON(t *testing.T, store *recovery.Store, st *recovery.State) {
	t.Helper()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRecoveryStore(t *testing.T) *recovery.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := recovery.NewStore(&recovery.Paths{RecoveryDir: dir, CodexHome: dir}, filepath.Join(dir, "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// B1: no recovery state + traffic stopped → immediate close, no event.
func TestBeforeCloseNoStateAllowsClose(t *testing.T) {
	cap := new(emitCapture)
	a := NewApp(AppOptions{Recovery: newRecoveryStore(t), RecoveryHome: t.TempDir(), EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); allow {
		t.Fatal("clean close must be allowed")
	}
	if n := cap.count(); n != 0 {
		t.Fatalf("emitted %d events, want 0", n)
	}
}

// B2: a normally-finished state (recovered) → immediate close, no event.
func TestBeforeCloseRecoveredStateAllowsClose(t *testing.T) {
	cap := new(emitCapture)
	store := newRecoveryStore(t)
	writeRecoveryJSON(t, store, &recovery.State{
		SchemaVersion: recovery.SchemaVersion,
		Phase:         recovery.PhaseRecovered,
	})
	a := NewApp(AppOptions{Recovery: store, RecoveryHome: t.TempDir(), EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); allow {
		t.Fatal("clean close must be allowed")
	}
	if n := cap.count(); n != 0 {
		t.Fatalf("emitted %d events, want 0", n)
	}
}

// B3 (regression for the cold-start bug): a stale legacy state whose configPath
// is a verbatim `\\?\` absolute path must be reconciled to inactive at startup
// (real config read, neither applied nor original), stay inactive on a second
// reconcile, and then close immediately without a dialog.
func TestBeforeCloseStaleVerbatimStateSelfHealsToInactive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows verbatim path semantics")
	}
	ctx := context.Background()
	home := t.TempDir()
	cfg := []byte("model = \"user-edited\"\n")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := recovery.NewStore(&recovery.Paths{RecoveryDir: home, CodexHome: home}, filepath.Join(home, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCodexHome(home); err != nil {
		t.Fatal(err)
	}
	writeRecoveryJSON(t, store, &recovery.State{
		SchemaVersion:         recovery.SchemaVersion,
		IntegrationActive:     false,
		Phase:                 recovery.PhaseReconciliationConf,
		ConfigPath:            `\\?\` + filepath.Join(home, "config.toml"),
		ConfigHashBeforeApply: recovery.HashBytes([]byte("legacy-before")),
		ConfigHashAfterApply:  recovery.HashBytes([]byte("legacy-after")),
	})
	first, err := store.ReconcileStartup(ctx, os.ReadFile)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if first.Phase == nil || *first.Phase != recovery.PhaseInactive {
		t.Fatalf("first reconcile = %#v, want PhaseInactive", first)
	}
	second, err := store.ReconcileStartup(ctx, os.ReadFile)
	if err != nil {
		t.Fatalf("second ReconcileStartup: %v", err)
	}
	if second.Phase == nil || *second.Phase != recovery.PhaseInactive {
		t.Fatalf("second reconcile = %#v, want PhaseInactive (no oscillation)", second)
	}
	cap := new(emitCapture)
	a := NewApp(AppOptions{Recovery: store, RecoveryHome: home, BackupDir: home, EmitEvents: cap.emit})
	if allow := a.beforeClose(ctx); allow {
		t.Fatal("self-healed clean state must close immediately")
	}
	if n := cap.count(); n != 0 {
		t.Fatalf("emitted %d events, want 0", n)
	}
}

// B4: unsaved observations (discard not confirmed) → block with reason
// unsaved_observations.
func TestBeforeCloseUnsavedObservationsBlocks(t *testing.T) {
	cap := new(emitCapture)
	store := newRecoveryStore(t)
	writeRecoveryJSON(t, store, &recovery.State{
		SchemaVersion:              recovery.SchemaVersion,
		Phase:                      recovery.PhaseInactive,
		UnsavedObservationsMayRemain: true,
	})
	a := NewApp(AppOptions{Recovery: store, RecoveryHome: t.TempDir(), EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); !allow {
		t.Fatal("unsaved observations must block close")
	}
	payload, ok := cap.lastExit()
	if !ok {
		t.Fatal("expected desktop-exit-confirmation-requested event")
	}
	if payload.Reason != "unsaved_observations" || !payload.UnsavedObservations {
		t.Fatalf("payload = %+v, want unsaved_observations", payload)
	}
}

// B5: a genuine restore target (reconciliation_required) → block with reason
// recovery_required.
func TestBeforeCloseRecoveryRequiredBlocks(t *testing.T) {
	cap := new(emitCapture)
	store := newRecoveryStore(t)
	writeRecoveryJSON(t, store, &recovery.State{
		SchemaVersion: recovery.SchemaVersion,
		Phase:         recovery.PhaseReconciliationReq,
	})
	a := NewApp(AppOptions{Recovery: store, RecoveryHome: t.TempDir(), EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); !allow {
		t.Fatal("recovery-required state must block close")
	}
	payload, ok := cap.lastExit()
	if !ok {
		t.Fatal("expected desktop-exit-confirmation-requested event")
	}
	if payload.Reason != "recovery_required" || !payload.RecoveryRequired {
		t.Fatalf("payload = %+v, want recovery_required", payload)
	}
}

// B6: traffic analysis running (ModeRecovery) → block with reason traffic_active.
func TestBeforeCloseTrafficActiveBlocks(t *testing.T) {
	cap := new(emitCapture)
	traffic := trafficanalysis.NewService()
	if _, err := traffic.MarkRecovery(); err != nil {
		t.Fatal(err)
	}
	a := NewApp(AppOptions{Traffic: traffic, EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); !allow {
		t.Fatal("running traffic must block close")
	}
	payload, ok := cap.lastExit()
	if !ok {
		t.Fatal("expected desktop-exit-confirmation-requested event")
	}
	if payload.Reason != "traffic_active" || !payload.TrafficActive {
		t.Fatalf("payload = %+v, want traffic_active", payload)
	}
}

// B7: an unknown persisted phase fails closed → block with reason recovery_required.
func TestBeforeCloseUnknownPhaseBlocks(t *testing.T) {
	cap := new(emitCapture)
	store := newRecoveryStore(t)
	writeRecoveryJSON(t, store, &recovery.State{
		SchemaVersion: recovery.SchemaVersion,
		Phase:         "bogus",
	})
	a := NewApp(AppOptions{Recovery: store, RecoveryHome: t.TempDir(), EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); !allow {
		t.Fatal("unknown phase must block close")
	}
	payload, ok := cap.lastExit()
	if !ok {
		t.Fatal("expected desktop-exit-confirmation-requested event")
	}
	if payload.Reason != "recovery_required" {
		t.Fatalf("payload = %+v, want recovery_required", payload)
	}
}

// B8: gateway running only → block with reason gateway_active.
func TestBeforeCloseGatewayActiveBlocks(t *testing.T) {
	cap := new(emitCapture)
	a := NewApp(AppOptions{Service: newScriptedController(gateway.State{Status: gateway.StatusRunning}), EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); !allow {
		t.Fatal("running gateway must block close")
	}
	payload, ok := cap.lastExit()
	if !ok {
		t.Fatal("expected desktop-exit-confirmation-requested event")
	}
	if payload.Reason != "gateway_active" || !payload.GatewayActive || payload.TrafficActive {
		t.Fatalf("payload = %+v, want gateway_active", payload)
	}
}

// B9: gateway + traffic running → traffic_active takes priority (the gateway is
// still stopped in the shutdown flow, so only the reason must change).
func TestBeforeCloseTrafficBeatsGateway(t *testing.T) {
	cap := new(emitCapture)
	traffic := trafficanalysis.NewService()
	if _, err := traffic.MarkRecovery(); err != nil {
		t.Fatal(err)
	}
	a := NewApp(AppOptions{
		Service:    newScriptedController(gateway.State{Status: gateway.StatusRunning}),
		Traffic:    traffic,
		EmitEvents: cap.emit,
	})
	if allow := a.beforeClose(context.Background()); !allow {
		t.Fatal("running traffic must block close")
	}
	payload, ok := cap.lastExit()
	if !ok {
		t.Fatal("expected desktop-exit-confirmation-requested event")
	}
	if payload.Reason != "traffic_active" || !payload.TrafficActive || !payload.GatewayActive {
		t.Fatalf("payload = %+v, want traffic_active with gateway active", payload)
	}
}

// B10: gateway running + unsaved observations → unsaved_observations takes priority.
func TestBeforeCloseUnsavedBeatsGateway(t *testing.T) {
	cap := new(emitCapture)
	store := newRecoveryStore(t)
	writeRecoveryJSON(t, store, &recovery.State{
		SchemaVersion:              recovery.SchemaVersion,
		Phase:                      recovery.PhaseInactive,
		UnsavedObservationsMayRemain: true,
	})
	a := NewApp(AppOptions{
		Service:     newScriptedController(gateway.State{Status: gateway.StatusRunning}),
		Recovery:    store,
		RecoveryHome: t.TempDir(),
		EmitEvents:  cap.emit,
	})
	if allow := a.beforeClose(context.Background()); !allow {
		t.Fatal("unsaved observations must block close")
	}
	payload, ok := cap.lastExit()
	if !ok {
		t.Fatal("expected desktop-exit-confirmation-requested event")
	}
	if payload.Reason != "unsaved_observations" || !payload.UnsavedObservations {
		t.Fatalf("payload = %+v, want unsaved_observations", payload)
	}
}

// B11: gateway stopped + nothing else → immediate close, no event.
func TestBeforeCloseStoppedGatewayAllowsClose(t *testing.T) {
	cap := new(emitCapture)
	a := NewApp(AppOptions{Service: newScriptedController(gateway.State{Status: gateway.StatusStopped}), EmitEvents: cap.emit})
	if allow := a.beforeClose(context.Background()); allow {
		t.Fatal("clean close must be allowed")
	}
	if n := cap.count(); n != 0 {
		t.Fatalf("emitted %d events, want 0", n)
	}
}
