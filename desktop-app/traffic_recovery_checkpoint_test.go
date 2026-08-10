package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/traffictransaction"
)

func strPtr(s string) *string { return &s }

// seedLegacyRecovery builds a store whose roots mirror ensureRecoveryStore and
// seeds it with a self-healed inactive state that still carries legacy evidence,
// most importantly an absolute autoLog.path OUTSIDE the TrafficLogDir root (the
// shape that made the live checkpoint write fail with
// recovery_state_parse_failed "invalid recovery autoLog.path").
func seedLegacyRecovery(t *testing.T, mutate func(*recovery.State)) (*recovery.Store, string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	recoveryDir := filepath.Join(root, "recovery")
	backupDir := filepath.Join(root, "backups", "codex-config")
	logDir := filepath.Join(root, "logs", "traffic-analysis")
	for _, dir := range []string{home, backupDir, logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	legacyLog := filepath.Join(root, "legacy-project", "logs", "traffic-analysis-20260804-071631.log")
	if err := os.MkdirAll(filepath.Dir(legacyLog), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   recoveryDir,
		CodexHome:     home,
		BackupDir:     backupDir,
		TrafficLogDir: logDir,
		AppDataRoot:   filepath.Join(root, "appdata"),
	}, filepath.Join(recoveryDir, "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	st := &recovery.State{
		SchemaVersion:                recovery.SchemaVersion,
		Phase:                        recovery.PhaseInactive,
		OperationID:                  "legacy-op",
		ConfigPath:                   filepath.Join(home, "config.toml"),
		AppliedOpenaiBaseURL:         "http://127.0.0.1:38441",
		ConfigHashBeforeApply:        strings.Repeat("a", 64),
		ConfigHashAfterApply:         strings.Repeat("b", 64),
		BackupPath:                   strPtr(filepath.Join(root, "legacy-backup", "config.toml.bak")),
		StartedAt:                    "2026-08-03T22:16:31Z",
		AutoLog:                      &recovery.AutoLogRecoveryState{SessionID: "s-legacy", Path: legacyLog, LastCheckpointSequence: 0, Finalized: false},
		AutoLogStatus:                strPtr("finalized"),
		CaptureStateLastKnown:        "capturing",
		RelayActiveLastKnown:         true,
		UnsavedObservationsMayRemain: false,
		UnsavedDiscardConfirmed:      false,
	}
	if mutate != nil {
		mutate(st)
	}
	writeRecoveryJSON(t, store, st)
	return store, home, backupDir
}

func requireRecoveryParseError(t *testing.T, err error) {
	t.Helper()
	var recErr *recovery.Error
	if !errors.As(err, &recErr) {
		t.Fatalf("expected *recovery.Error, got %v", err)
	}
	if recErr.Kind != recovery.KindStateParseFailed {
		t.Fatalf("kind = %s, want recovery_state_parse_failed", recErr.Kind)
	}
	if recErr.Message != "invalid recovery autoLog.path" {
		t.Fatalf("message = %q, want %q", recErr.Message, "invalid recovery autoLog.path")
	}
}

// T1 (KEY regression): a self-healed inactive state carrying stale outside-root
// AutoLog evidence is superseded by a fresh Enable prepared checkpoint: the
// stale AutoLog/AutoLogStatus are dropped and the new journal evidence (op-1,
// backup b1, relative config.toml) survives.
func TestCheckpointClearsStaleAutoLogOnFreshEnable(t *testing.T) {
	ctx := context.Background()
	store, home, backupDir := seedLegacyRecovery(t, nil)
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}

	err := writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID: "op-1",
		Phase:       traffictransaction.PhasePrepared,
		BackupID:    "b1",
	})
	if err != nil {
		t.Fatalf("fresh Enable prepared checkpoint failed: %v", err)
	}

	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != recovery.PhasePrepared {
		t.Fatalf("phase = %s, want prepared", st.Phase)
	}
	if st.AutoLog != nil {
		t.Fatalf("stale AutoLog not cleared: %+v", st.AutoLog)
	}
	if st.AutoLogStatus != nil {
		t.Fatalf("stale AutoLogStatus not cleared: %+v", *st.AutoLogStatus)
	}
	if st.ConfigPath != "config.toml" {
		t.Fatalf("configPath = %q, want config.toml", st.ConfigPath)
	}
	if st.OperationID != "op-1" {
		t.Fatalf("operationId = %q, want op-1", st.OperationID)
	}
	if st.BackupPath == nil || *st.BackupPath != "b1" {
		t.Fatalf("backupPath = %v, want b1", st.BackupPath)
	}
	if st.IntegrationActive {
		t.Fatal("integrationActive must stay false for a prepared checkpoint")
	}
}

// T2: the adopt path skips prepared; its first activating checkpoint
// (capture_started) must also clear the stale AutoLog evidence.
func TestCheckpointClearsStaleAutoLogOnAdopt(t *testing.T) {
	ctx := context.Background()
	store, home, backupDir := seedLegacyRecovery(t, nil)
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}

	err := writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID:       "op-2",
		Phase:             traffictransaction.PhaseCaptureAdopted,
		IntegrationActive: true,
		CaptureState:      "capturing",
		RelayActive:       true,
		BackupID:          "b2",
	})
	if err != nil {
		t.Fatalf("adopt checkpoint failed: %v", err)
	}
	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != recovery.PhaseCaptureStarted {
		t.Fatalf("phase = %s, want capture_started", st.Phase)
	}
	if st.AutoLog != nil {
		t.Fatalf("stale AutoLog not cleared on adopt: %+v", st.AutoLog)
	}
	if !st.IntegrationActive {
		t.Fatal("integrationActive must be true after adopt")
	}
}

// T3 (safety): a non-activating write (Disable/Finish-style inactive journal)
// must NOT clear the stale AutoLog — path containment is not relaxed, the write
// stays rejected, and the evidence is retained.
func TestCheckpointKeepsStaleAutoLogOnNonActivatingWrite(t *testing.T) {
	ctx := context.Background()
	store, home, backupDir := seedLegacyRecovery(t, nil)
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}

	err := writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID:  "op-1",
		Phase:        traffictransaction.PhaseInactive,
		DurablePhase: traffictransaction.DurableInactive,
	})
	requireRecoveryParseError(t, err)

	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.AutoLog == nil {
		t.Fatal("AutoLog evidence must be retained on a rejected write")
	}
	if st.Phase != recovery.PhaseInactive {
		t.Fatalf("phase = %s, want the seeded inactive", st.Phase)
	}
}

// T3b (safety): an unresolved state (reconciliation_required) with stale AutoLog
// is never cleared even by an activating checkpoint — fail-closed.
func TestCheckpointDoesNotClearUnresolvedState(t *testing.T) {
	ctx := context.Background()
	store, home, backupDir := seedLegacyRecovery(t, func(st *recovery.State) {
		st.Phase = recovery.PhaseReconciliationReq
		st.ReconciliationStatus = strPtr("pending_restore")
	})
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}

	err := writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID: "op-1",
		Phase:       traffictransaction.PhasePrepared,
	})
	requireRecoveryParseError(t, err)

	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.AutoLog == nil {
		t.Fatal("unresolved state AutoLog evidence must not be cleared")
	}
}

// T4 (fail-closed): a state with unsaved observations that may remain must not
// have its AutoLog cleared, so the checkpoint stays rejected.
func TestCheckpointDoesNotClearUnsavedState(t *testing.T) {
	ctx := context.Background()
	store, home, backupDir := seedLegacyRecovery(t, func(st *recovery.State) {
		st.UnsavedObservationsMayRemain = true
	})
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}

	err := writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID: "op-1",
		Phase:       traffictransaction.PhasePrepared,
	})
	requireRecoveryParseError(t, err)

	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.AutoLog == nil || !st.UnsavedObservationsMayRemain {
		t.Fatalf("unsaved evidence must be retained, got AutoLog=%+v", st.AutoLog)
	}
}

// T5: a legacy state whose configPath is a verbatim `\\?\` absolute path (the
// Plan 4g self-heal input) must still be superseded by a fresh Enable prepared
// checkpoint on Windows.
func TestCheckpointFreshEnableWithVerbatimConfigPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows verbatim path semantics")
	}
	ctx := context.Background()
	store, home, backupDir := seedLegacyRecovery(t, nil)
	// Persist a verbatim configPath directly (bypasses normalize, mirroring the
	// live Plan 4g self-heal input) and let Load tolerate it, as the real store does.
	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.ConfigPath = `\\?\` + filepath.Join(home, "config.toml")
	writeRecoveryJSON(t, store, st)
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}

	err = writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID: "op-1",
		Phase:       traffictransaction.PhasePrepared,
		BackupID:    "b1",
	})
	if err != nil {
		t.Fatalf("fresh Enable over verbatim legacy state failed: %v", err)
	}
	st, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ConfigPath != "config.toml" {
		t.Fatalf("configPath = %q, want config.toml", st.ConfigPath)
	}
	if st.AutoLog != nil {
		t.Fatalf("stale AutoLog not cleared: %+v", st.AutoLog)
	}
}

// S2: a completely fresh store (no recovery state) accepts a prepared checkpoint.
func TestCheckpointFreshStateEnable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backups", "codex-config")
	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   filepath.Join(root, "recovery"),
		CodexHome:     home,
		BackupDir:     backupDir,
		TrafficLogDir: filepath.Join(root, "logs", "traffic-analysis"),
		AppDataRoot:   filepath.Join(root, "appdata"),
	}, filepath.Join(root, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}
	if err := writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID: "op-1",
		Phase:       traffictransaction.PhasePrepared,
		BackupID:    "b1",
	}); err != nil {
		t.Fatalf("fresh-state Enable checkpoint failed: %v", err)
	}
	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != recovery.PhasePrepared || st.OperationID != "op-1" {
		t.Fatalf("state = %+v, want prepared/op-1", st)
	}
}

// T6: classifyCheckpointFailure extracts only safe diagnostic fields — never the
// raw error text (which may carry paths or secrets).
func TestClassifyCheckpointFailure(t *testing.T) {
	fields := classifyCheckpointFailure(&recovery.Error{Kind: recovery.KindStateParseFailed, Message: "invalid recovery autoLog.path"})
	if fields.Cause != "recovery_state_parse_failed" || fields.Field != "autoLog.path" {
		t.Fatalf("fields = %+v, want cause=recovery_state_parse_failed field=autoLog.path", fields)
	}

	fields = classifyCheckpointFailure(errors.New(`boom: C:\Users\Sohei\.codex\config.toml api_key=sk-abc`))
	if fields.Cause != "" || fields.Field != "" {
		t.Fatalf("plain error must yield empty fields, got %+v", fields)
	}

	fields = classifyCheckpointFailure(&recovery.Error{Kind: recovery.KindStateNotFound, Message: "recovery state not found"})
	if fields.Cause != "recovery_state_not_found" || fields.Field != "" {
		t.Fatalf("fields = %+v, want cause only", fields)
	}
}

// T7: shouldClearStaleAutoLog admits exactly the resolved + activating case.
func TestShouldClearStaleAutoLog(t *testing.T) {
	activating := []recovery.Phase{recovery.PhasePrepared, recovery.PhaseCaptureStarted, recovery.PhaseIntegrationApplied}
	nonActivating := []recovery.Phase{recovery.PhaseInactive, recovery.PhaseRecovered, recovery.PhaseReconciliationReq}
	for _, p := range activating {
		if !shouldClearStaleAutoLog(&recovery.State{Phase: recovery.PhaseInactive}, p) {
			t.Fatalf("activating %s over inactive must clear", p)
		}
		if !shouldClearStaleAutoLog(&recovery.State{Phase: recovery.PhaseRecovered}, p) {
			t.Fatalf("activating %s over recovered must clear", p)
		}
		if !shouldClearStaleAutoLog(&recovery.State{}, p) {
			t.Fatalf("activating %s over a fresh state must clear", p)
		}
		if shouldClearStaleAutoLog(&recovery.State{Phase: recovery.PhaseReconciliationReq}, p) {
			t.Fatalf("activating %s over reconciliation must NOT clear", p)
		}
		if shouldClearStaleAutoLog(&recovery.State{Phase: recovery.PhaseInactive, UnsavedObservationsMayRemain: true}, p) {
			t.Fatalf("activating %s over unsaved must NOT clear", p)
		}
	}
	for _, p := range nonActivating {
		if shouldClearStaleAutoLog(&recovery.State{Phase: recovery.PhaseInactive}, p) {
			t.Fatalf("non-activating %s must NOT clear", p)
		}
	}
	if shouldClearStaleAutoLog(nil, recovery.PhasePrepared) {
		t.Fatal("nil state must NOT clear")
	}
}

// S3: a Disable restore-conflict checkpoint records ReconciliationStatus so the
// GUI can surface the live dead-end without a process restart.
func TestCheckpointWritesReconciliationStatus(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backups", "codex-config")
	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   filepath.Join(root, "recovery"),
		CodexHome:     home,
		BackupDir:     backupDir,
		TrafficLogDir: filepath.Join(root, "logs", "traffic-analysis"),
		AppDataRoot:   filepath.Join(root, "appdata"),
	}, filepath.Join(root, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	writer := trafficRecoveryWriter{store: store, configHome: home, backupDir: backupDir}
	err = writer.Checkpoint(ctx, traffictransaction.Checkpoint{
		OperationID:           "op-1",
		Phase:                 traffictransaction.PhaseDisableStarted,
		DurablePhase:          traffictransaction.DurableReconciliationRequired,
		IntegrationActive:     true,
		ReconciliationStatus:  traffictransaction.ReconciliationStatusConfigConflict,
	})
	if err != nil {
		t.Fatalf("conflict checkpoint failed: %v", err)
	}
	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReconciliationStatus == nil || *st.ReconciliationStatus != string(recovery.StatusConfigConflict) {
		t.Fatalf("ReconciliationStatus = %v, want %q", st.ReconciliationStatus, recovery.StatusConfigConflict)
	}
	if st.Phase != recovery.PhaseReconciliationReq || !st.IntegrationActive {
		t.Fatalf("state = phase %q active %t, want reconciliation_req/true", st.Phase, st.IntegrationActive)
	}
}

// S4: checkpointFromRecovery round-trips the ReconciliationStatus so Current()
// reflects the live conflict classification.
func TestCheckpointFromRecoveryRoundTripsReconciliationStatus(t *testing.T) {
	cp := checkpointFromRecovery(&recovery.State{
		Phase: recovery.PhaseReconciliationConf, IntegrationActive: true,
		OperationID: "op-1", ReconciliationStatus: strPtr(string(recovery.StatusConfigConflict)),
	})
	if cp.ReconciliationStatus != traffictransaction.ReconciliationStatusConfigConflict {
		t.Fatalf("round-trip ReconciliationStatus = %q, want %q", cp.ReconciliationStatus, traffictransaction.ReconciliationStatusConfigConflict)
	}
}
