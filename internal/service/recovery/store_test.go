package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testStore builds a Store rooted at a temp dir. The returned dir is the
// "Moon Bridge" app root and also serves as CODEX_HOME for path tests.
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "recovery", "recovery-state-v2.json")
	s, err := NewStore(&Paths{RecoveryDir: filepath.Join(dir, "recovery"), CodexHome: dir}, path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, dir
}

func stateWithPhase(t *testing.T, s *Store, phase Phase) *State {
	t.Helper()
	st := New()
	st.Phase = phase
	if s != nil {
		if home := s.CodexHome(); home != "" {
			st.ConfigPath = "config.toml"
			st.CodexHomeFingerprint = boundHome(t, home)
		}
	}
	return st
}

func writeStateFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.IntegrationActive = true
	st.OperationID = "op-7"
	st.TransitionID = "550e8400-e29b-41d4-a716-446655440000"
	st.RoutePhase = "activating_deepseek"
	st.DesiredRoute = "deepseek"
	st.RouteEvidence = "none"
	st.AppliedOpenaiBaseURL = "http://127.0.0.1:38441/"
	st.ConfigHashBeforeApply = "deadbeef"
	st.ConfigHashAfterApply = "cafebabe"
	st.ReconciliationStatus = StringPtr("pending_restore")

	if err := s.Write(ctx, st); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil")
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d", got.SchemaVersion)
	}
	if !got.IntegrationActive || got.Phase != PhaseIntegrationApplied {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if got.TransitionID != st.TransitionID || got.RoutePhase != st.RoutePhase || got.DesiredRoute != st.DesiredRoute || got.RouteEvidence != st.RouteEvidence {
		t.Fatalf("round-trip lost route transition fields: %+v", got)
	}
	if got.ConfigPath != "config.toml" || got.AppliedOpenaiBaseURL != "http://127.0.0.1:38441/" {
		t.Fatalf("round-trip lost path/url: %+v", got)
	}
	if got.ReconciliationStatus == nil || *got.ReconciliationStatus != "pending_restore" {
		t.Fatalf("round-trip lost ptr field: %+v", got)
	}
	if got.UpdatedAt == nil {
		t.Fatal("Write must stamp UpdatedAt")
	}
}

func TestStoreLoadUnsafeURLPreservesRecoveryFile(t *testing.T) {
	ctx := context.Background()
	s, dir := testStore(t)
	path := s.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	unsafe := `{"schemaVersion":2,"phase":"integration_applied","previousOpenaiBaseUrlPresent":true,"previousOpenaiBaseUrl":"https://user:password@example.com","appliedOpenaiBaseUrl":"http://127.0.0.1:38441","configPath":"config.toml"}`
	if err := os.WriteFile(path, []byte(unsafe), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := HashBytes(before)
	beforeSize := len(before)
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"gpt\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configBefore, _ := os.ReadFile(configPath)
	if _, err := s.Load(ctx); err == nil {
		t.Fatal("unsafe recovery URL must be rejected")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if HashBytes(after) != beforeHash || len(after) != beforeSize {
		t.Fatal("recovery file changed")
	}
	configAfter, _ := os.ReadFile(configPath)
	if HashBytes(configAfter) != HashBytes(configBefore) {
		t.Fatal("config changed")
	}
}

func TestStoreLoadMissingIsNil(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load on missing: %v", err)
	}
	if got != nil {
		t.Fatalf("Load on missing returned non-nil: %+v", got)
	}
}

func TestStoreUnknownFieldIgnored(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	writeStateFile(t, s.Path(), []byte(`{
		"schemaVersion": 2,
		"integrationActive": true,
		"phase": "integration_applied",
		"someNewFieldThatGoDoesNotKnow": "ignored-ok",
		"configPath": "config.toml"
	}`))
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load should ignore unknown field: %v", err)
	}
	if got == nil || !got.IntegrationActive {
		t.Fatalf("unknown field lost integrationActive: %+v", got)
	}
}

func TestStoreUnsupportedSchemaVersionRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	for _, ver := range []string{"3", "1", "0"} {
		body := `{"schemaVersion":` + ver + `,"phase":"x"}`
		os.Remove(s.Path())
		writeStateFile(t, s.Path(), []byte(body))
		_, err := s.Load(ctx)
		var rerr *Error
		if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindStateUnsupportedVersion {
			t.Fatalf("schemaVersion=%s: expected unsupported_version, got %v", ver, err)
		}
	}
}

func TestStoreMissingSchemaVersionRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	writeStateFile(t, s.Path(), []byte(`{"integrationActive":true,"phase":"integration_applied"}`))
	_, err := s.Load(ctx)
	if err == nil {
		t.Fatal("missing schemaVersion must be rejected (no v2 defaulting)")
	}
}

func TestStoreTruncatedJSONFails(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	writeStateFile(t, s.Path(), []byte(`{"schemaVersion":2,"phase":`))
	_, err := s.Load(ctx)
	if err == nil {
		t.Fatal("expected parse failure on truncated JSON")
	}
}

func TestStoreUpdateMutatesAndPersists(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if err := s.Write(ctx, stateWithPhase(t, s, PhasePrepared)); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	err := s.Update(ctx, func(cur *State) error {
		if cur == nil {
			t.Fatal("Update got nil current")
		}
		cur.IntegrationActive = true
		cur.Phase = PhaseCaptureStarted
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Load(ctx)
	if got == nil || !got.IntegrationActive || got.Phase != PhaseCaptureStarted {
		t.Fatalf("Update did not persist: %+v", got)
	}
}

func TestStoreUpdateNilCurrentRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	// No prior state -> loadUnlocked returns nil. fn must build a state; a nil
	// result is rejected (deletion is via Delete, not nil).
	err := s.Update(ctx, func(cur *State) error {
		_ = cur
		return nil
	})
	if err == nil {
		t.Fatal("Update must reject a nil produced state")
	}
}

func TestStoreUpdateCallbackErrorPreservesOldState(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.IntegrationActive = true
	if err := s.Write(ctx, st); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	before, _ := os.ReadFile(s.Path())

	err := s.Update(ctx, func(cur *State) error {
		cur.IntegrationActive = false // would-be change
		return newError(KindStateParseFailed, "boom")
	})
	if err == nil {
		t.Fatal("expected callback error to propagate")
	}
	after, _ := os.ReadFile(s.Path())
	if string(before) != string(after) {
		t.Fatal("callback error must not persist partial changes")
	}
}

func TestClearCleanupPendingDeletesOnlyPendingState(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	pending := &CleanupPending{TransactionID: "tx", BackupID: "20260805T103040123Z-config.toml", RouteMutationResult: "applied", Status: "pending"}
	st := New()
	st.Phase = PhaseInactive
	st.CleanupPending = pending
	if err := s.Write(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearCleanupPending(ctx, "wrong", pending.BackupID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx); got == nil || got.CleanupPending == nil {
		t.Fatal("mismatched clear changed state")
	}
	if _, err := s.ClearCleanupPending(ctx, pending.TransactionID, pending.BackupID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx); got != nil {
		t.Fatalf("pending-only state remains: %#v", got)
	}
}

func TestCleanupPendingOnlySurvivesStoreRecreation(t *testing.T) {
	ctx := context.Background()
	s, dir := testStore(t)
	pending := &CleanupPending{TransactionID: "tx-restart", BackupID: "20260805T103040123Z-config.toml", RouteMutationResult: "applied", Status: "pending"}
	st := stateWithPhase(t, s, PhaseInactive)
	st.CleanupPending = pending
	if err := s.Write(ctx, st); err != nil {
		t.Fatal(err)
	}
	recreated, err := NewStore(&Paths{RecoveryDir: filepath.Join(dir, "recovery"), CodexHome: dir}, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	got, err := recreated.Load(ctx)
	if err != nil || got == nil || got.CleanupPending == nil || *got.CleanupPending != *pending {
		t.Fatalf("recreated pending = %#v, %v", got, err)
	}
}

func TestClearCleanupPendingKeepsRegularRecovery(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.OperationID = "op"
	st.CleanupPending = &CleanupPending{TransactionID: "tx", BackupID: "20260805T103040123Z-config.toml", RouteMutationResult: "applied", Status: "pending"}
	if err := s.Write(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearCleanupPending(ctx, "tx", st.CleanupPending.BackupID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil || got == nil || got.CleanupPending != nil {
		t.Fatalf("regular state after clear = %#v, %v", got, err)
	}
}

func TestStoreDeleteAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if err := s.Write(ctx, stateWithPhase(t, s, PhasePrepared)); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	if err := s.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatalf("state file still exists after Delete, err=%v", err)
	}
	if err := s.Delete(ctx); err != nil {
		t.Fatalf("repeated Delete should be idempotent success, got %v", err)
	}
	if _, err := s.Load(ctx); err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
}

func TestStoreWriteNilRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if err := s.Write(ctx, nil); err == nil {
		t.Fatal("Write(nil) should be rejected")
	}
}

func TestStoreWriteMissingPhaseRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if err := s.Write(ctx, New()); err == nil {
		t.Fatal("Write with empty phase must be rejected")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("rejection must not leave a file behind")
	}
}

func TestStoreUpdateStampsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	st := stateWithPhase(t, s, PhasePrepared)
	st.UpdatedAt = StringPtr("2020-01-01T00:00:00Z")
	if err := s.Write(ctx, st); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := s.Load(ctx)
	if got == nil || got.UpdatedAt == nil || *got.UpdatedAt == "2020-01-01T00:00:00Z" {
		t.Fatalf("UpdatedAt not restamped on write: %+v", got)
	}
}

func TestStoreWriteNormalizesAbsoluteConfigPath(t *testing.T) {
	ctx := context.Background()
	s, dir := testStore(t)
	// ConfigPath absolute inside the (existing) CODEX_HOME dir.
	abs := filepath.Join(dir, "config.toml")
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.ConfigPath = abs
	if err := s.Write(ctx, st); err != nil {
		t.Fatalf("Write with absolute inside-root configPath: %v", err)
	}
	got, _ := s.Load(ctx)
	if got == nil || got.ConfigPath == abs || filepath.IsAbs(got.ConfigPath) {
		t.Fatalf("configPath not normalized to relative: %q", got.ConfigPath)
	}
}

func TestStoreWriteRejectsAbsoluteConfigPathOutsideRoot(t *testing.T) {
	ctx := context.Background()
	s, dir := testStore(t)
	outside := filepath.Join(filepath.Dir(dir), "outside-config.toml")
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.ConfigPath = outside
	if err := s.Write(ctx, st); err == nil {
		t.Fatal("Write with absolute configPath outside codex home must be rejected")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("rejected write must not leave a file behind")
	}
}

func TestStoreWriteNormalizesBackupAndAutoLogPaths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backupDir := filepath.Join(dir, "backups", "codex-config")
	logDir := filepath.Join(dir, "logs", "traffic-analysis")
	for _, d := range []string{backupDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewStore(&Paths{
		RecoveryDir:   filepath.Join(dir, "recovery"),
		CodexHome:     dir,
		BackupDir:     backupDir,
		TrafficLogDir: logDir,
	}, filepath.Join(dir, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	backAbs := filepath.Join(backupDir, "b.json")
	logAbs := filepath.Join(logDir, "sess.log")
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.BackupPath = StringPtr(backAbs)
	st.AutoLog = &AutoLogRecoveryState{SessionID: "s-1", Path: logAbs, LastCheckpointSequence: 3, Finalized: false}
	if err := s.Write(ctx, st); err != nil {
		t.Fatalf("Write with absolute backup/log: %v", err)
	}
	got, _ := s.Load(ctx)
	if got == nil {
		t.Fatal("load nil")
	}
	if got.BackupPath == nil || filepath.IsAbs(*got.BackupPath) {
		t.Fatalf("backupPath not normalized: %v", got.BackupPath)
	}
	if got.AutoLog == nil || filepath.IsAbs(got.AutoLog.Path) {
		t.Fatalf("autoLog.path not normalized: %+v", got.AutoLog)
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir()) // real absolute root
	for _, bad := range []string{"..", "../x", "../../y", "a/../../z"} {
		if _, err := Resolve(root, bad); err == nil {
			t.Errorf("Resolve(%q) should reject", bad)
		}
	}
	got, err := Resolve(root, filepath.Join("config", "config.toml"))
	if err != nil {
		t.Fatalf("Resolve valid: %v", err)
	}
	if !strings.Contains(got, root) {
		t.Fatalf("Resolve returned %q", got)
	}
}

func TestToRelativeRejectsOutsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir())
	inside := filepath.Join(root, "recovery", "x.json")
	rel, err := ToRelative(root, inside)
	if err != nil {
		t.Fatalf("ToRelative inside: %v", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("ToRelative returned escape %q", rel)
	}
	sibling := filepath.Join(filepath.Dir(root), "other", "x.json")
	if _, err := ToRelative(root, sibling); err == nil {
		t.Fatal("ToRelative outside root should reject")
	}
	if _, err := ToRelative(root, "relative.txt"); err == nil {
		t.Fatal("ToRelative should require absolute input")
	}
}

func TestNewStoreNeverFallsBackToRelativePath(t *testing.T) {
	// With an empty recoveryStatePath, NewStore resolves the default root. It
	// must NEVER yield a relative path (rewriting the constructor would be
	// required for an env-injection seam), and it must return an error rather
	// than a cwd-relative fallback when a root cannot be resolved.
	s, err := NewStore(&Paths{}, "")
	if err != nil {
		// On machines where env is unavailable, this is the correct error path.
		// Either way we must not have a relative path.
		return
	}
	if s == nil || !filepath.IsAbs(s.Path()) {
		t.Fatalf("NewStore default path must be absolute, got %q", s.Path())
	}
	// With an explicit absolute path it must always succeed without env.
	abs := filepath.Join(t.TempDir(), "recovery", "recovery-state-v2.json")
	if _, err := NewStore(&Paths{}, abs); err != nil {
		t.Fatalf("NewStore with explicit absolute path: %v", err)
	}
}

func TestCodexHomeGetterMutex(t *testing.T) {
	s, dir := testStore(t)
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCodexHome(other); err != nil {
		t.Fatal(err)
	}
	if got := s.CodexHome(); got == "" {
		t.Fatal("CodexHome getter returned empty")
	}
}

// TestSetCodexHomeRejectsRelativePath: a relative codex home must be rejected
// outright (resolving it against cwd would make the root binding cwd-dependent,
// violating the NewStore absolute-root contract).
func TestSetCodexHomeRejectsRelativePath(t *testing.T) {
	s, _ := testStore(t)
	if err := s.SetCodexHome("relative/home"); err == nil {
		t.Fatal("relative codex home must be rejected")
	}
}

// TestSetCodexHomeRejectsMissingDirectory: a non-existent home cannot be a stable
// root (canonicalization requires an existing, physically resolvable directory).
func TestSetCodexHomeRejectsMissingDirectory(t *testing.T) {
	s, _ := testStore(t)
	if err := s.SetCodexHome(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("non-existent codex home must be rejected")
	}
}

// TestSetCodexHomeRejectsFilePath: a file is not a directory root.
func TestSetCodexHomeRejectsFilePath(t *testing.T) {
	s, _ := testStore(t)
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCodexHome(f); err == nil {
		t.Fatal("non-directory codex home must be rejected")
	}
}

// TestSetCodexHomeStoresCanonicalAbsolute: a valid absolute directory is stored
// in its canonical (CanonicalizeCodexHome) form, so the root identifier is the
// same value the fingerprint is computed from.
func TestSetCodexHomeStoresCanonicalAbsolute(t *testing.T) {
	s, _ := testStore(t)
	dir := t.TempDir()
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatalf("SetCodexHome(valid): %v", err)
	}
	canon, err := CanonicalizeCodexHome(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CodexHome(); got != canon {
		t.Fatalf("CodexHome = %q, want canonical %q", got, canon)
	}
}

// TestSetCodexHomeFailedKeepsPreviousRoot: a rejected set leaves the previously
// configured canonical root untouched.
func TestSetCodexHomeFailedKeepsPreviousRoot(t *testing.T) {
	s, _ := testStore(t)
	dir := t.TempDir()
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	canon, _ := CanonicalizeCodexHome(dir)
	if err := s.SetCodexHome("relative/x"); err == nil {
		t.Fatal("relative codex home must be rejected")
	}
	if got := s.CodexHome(); got != canon {
		t.Fatalf("failed set must keep the previous root, got %q", got)
	}
}

// TestStoreWriteRejectsRelativeTraversalAllFields verifies that every path field
// (configPath / backupPath / autoLog.path) rejects a relative `../` escape before
// it can be persisted.
func TestStoreWriteRejectsRelativeTraversalAllFields(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backupDir := filepath.Join(dir, "backups", "codex-config")
	logDir := filepath.Join(dir, "logs", "traffic-analysis")
	for _, d := range []string{backupDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewStore(&Paths{
		RecoveryDir:   filepath.Join(dir, "recovery"),
		CodexHome:     dir,
		BackupDir:     backupDir,
		TrafficLogDir: logDir,
	}, filepath.Join(dir, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Run("configPath", func(t *testing.T) {
		st := stateWithPhase(t, s, PhaseIntegrationApplied)
		st.ConfigPath = "../evil.toml"
		if err := s.Write(ctx, st); err == nil {
			t.Fatal("relative configPath traversal must be rejected")
		}
	})
	t.Run("backupPath", func(t *testing.T) {
		st := stateWithPhase(t, s, PhaseIntegrationApplied)
		st.BackupPath = StringPtr("../evil.json")
		if err := s.Write(ctx, st); err == nil {
			t.Fatal("relative backupPath traversal must be rejected")
		}
	})
	t.Run("autoLogPath", func(t *testing.T) {
		st := stateWithPhase(t, s, PhaseIntegrationApplied)
		st.AutoLog = &AutoLogRecoveryState{SessionID: "s-1", Path: "../evil.log", LastCheckpointSequence: 1}
		if err := s.Write(ctx, st); err == nil {
			t.Fatal("relative autoLog.path traversal must be rejected")
		}
	})
}

// TestStoreWriteNormalizationFailureLeavesInputUnchanged verifies normalizeForWrite
// deep-copies: neither a successful write nor a rejected one may mutate the
// caller's State (the AutoLog/Migration/*string pointer fields must not alias the
// normalized output).
func TestStoreWriteNormalizationFailureLeavesInputUnchanged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs", "traffic-analysis")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(&Paths{
		RecoveryDir:   filepath.Join(dir, "recovery"),
		CodexHome:     dir,
		TrafficLogDir: logDir,
	}, filepath.Join(dir, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	logAbs := filepath.Join(logDir, "sess.log")
	base := stateWithPhase(t, s, PhaseIntegrationApplied)
	base.AutoLog = &AutoLogRecoveryState{SessionID: "s-1", Path: logAbs, LastCheckpointSequence: 1}
	if err := s.Write(ctx, base); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if base.AutoLog.Path != logAbs {
		t.Fatalf("successful write mutated the input AutoLog in place: %q", base.AutoLog.Path)
	}
	// A rejected write must leave the input untouched too.
	bad := stateWithPhase(t, s, PhaseIntegrationApplied)
	bad.AutoLog = &AutoLogRecoveryState{SessionID: "s-1", Path: logAbs, LastCheckpointSequence: 1}
	bad.ConfigPath = filepath.Join(dir, "..", "outside.toml")
	if err := s.Write(ctx, bad); err == nil {
		t.Fatal("outside-root write must be rejected")
	}
	if bad.ConfigPath != filepath.Join(dir, "..", "outside.toml") || bad.AutoLog.Path != logAbs {
		t.Fatalf("rejected write mutated the input: configPath=%q autoLog=%q", bad.ConfigPath, bad.AutoLog.Path)
	}
}

// TestStoreWriteRejectsSchemaVersionNot2 verifies schemaVersion is validated, not
// force-written to 2.
func TestStoreWriteRejectsSchemaVersionNot2(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	for _, ver := range []int{0, 1, 99} {
		st := New()
		st.SchemaVersion = ver
		st.Phase = PhaseIntegrationApplied
		st.ConfigPath = "config.toml"
		if err := s.Write(ctx, st); err == nil {
			t.Errorf("schemaVersion=%d must be rejected", ver)
		}
		if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
			t.Fatalf("rejected write left a file behind (schemaVersion=%d)", ver)
		}
	}
	st := New()
	st.Phase = PhaseIntegrationApplied
	if err := s.Write(ctx, st); err != nil {
		t.Fatalf("v2 write: %v", err)
	}
}

// TestStoreLoadPhaseMissingDefaultsToIntegrationApplied verifies the Rust serde
// phase default is applied on read for a v2 file that omitted phase.
func TestStoreLoadPhaseMissingDefaultsToIntegrationApplied(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	writeStateFile(t, s.Path(), []byte(`{"schemaVersion":2,"integrationActive":true}`))
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.Phase != PhaseIntegrationApplied {
		t.Fatalf("phase = %q, want integration_applied (Rust serde default)", phaseOf(got))
	}
	writeStateFile(t, s.Path(), []byte(`{"schemaVersion":2,"phase":"prepared"}`))
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatalf("Load explicit: %v", err)
	}
	if got == nil || got.Phase != PhasePrepared {
		t.Fatalf("explicit phase = %q, want prepared", phaseOf(got))
	}
}

func phaseOf(st *State) Phase {
	if st == nil {
		return ""
	}
	return st.Phase
}

func TestNewStoreRejectsExplicitRelativePath(t *testing.T) {
	if _, err := NewStore(&Paths{}, "recovery/recovery-state-v2.json"); err == nil {
		t.Fatal("explicit relative recovery state path must be rejected")
	}
}

func TestNewStoreRejectsRelativeRoots(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "recovery", "recovery-state-v2.json")
	for _, paths := range []*Paths{
		{RecoveryDir: "recovery"},
		{CodexHome: "codex"},
		{BackupDir: "backups"},
		{TrafficLogDir: "logs"},
		{AppDataRoot: "appdata"},
	} {
		if _, err := NewStore(paths, abs); err == nil {
			t.Errorf("relative root must be rejected: %+v", paths)
		}
	}
}

func TestStoreWriteRejectsRelativeConfigPathWithoutFingerprint(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.CodexHomeFingerprint = ""
	if err := s.Write(ctx, st); err == nil {
		t.Fatal("a relative configPath without a fingerprint must be rejected (auto-set is forbidden)")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("rejected write must not leave a file behind")
	}
}

func TestStoreWriteRejectsRelativeConfigPathFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	st := stateWithPhase(t, s, PhaseIntegrationApplied)
	st.CodexHomeFingerprint = strings.Repeat("f", 64)
	if err := s.Write(ctx, st); err == nil {
		t.Fatal("a fingerprint that does not match the current codex home must be rejected")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("rejected write must not leave a file behind")
	}
}

// TestStoreLoadRejectsRelativeTraversalInAllPathFields verifies the load path
// rejects a relative `..` escape in every path field (configPath / backupPath /
// autoLog.path / migration.sourcePath) as parse_failed.
func TestStoreLoadRejectsRelativeTraversalInAllPathFields(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewStore(&Paths{
		RecoveryDir:   filepath.Join(dir, "recovery"),
		CodexHome:     dir,
		BackupDir:     filepath.Join(dir, "backups", "codex-config"),
		TrafficLogDir: filepath.Join(dir, "logs", "traffic-analysis"),
	}, filepath.Join(dir, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cases := map[string]string{
		"configPath": `{"schemaVersion":2,"phase":"x","configPath":"../evil.toml"}`,
		"backupPath": `{"schemaVersion":2,"phase":"x","backupPath":"../evil.json"}`,
		"autoLog":    `{"schemaVersion":2,"phase":"x","autoLog":{"sessionId":"s","path":"../evil.log"}}`,
		"migration":  `{"schemaVersion":2,"phase":"x","migration":{"sourcePath":"../evil.json","sourceSchemaVersion":1,"migratedAt":"z"}}`,
	}
	for name, body := range cases {
		os.Remove(s.Path())
		writeStateFile(t, s.Path(), []byte(body))
		_, err := s.Load(ctx)
		var rerr *Error
		if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindStateParseFailed {
			t.Errorf("%s: expected parse_failed, got %v", name, err)
		}
	}
}

// TestValidateNormalizedStateRejectsInvalidSourcePath verifies the shared
// pre-marshal validator enforces the migration.sourcePath logical-id grammar.
func TestValidateNormalizedStateRejectsInvalidSourcePath(t *testing.T) {
	s, _ := testStore(t)
	valid := map[string]bool{
		"integration-state.json":                  true,
		"unparseable-v1":                          true,
		"legacy-integration-state":                true,
		"traffic-analysis/integration-state.json": true, // AppDataRoot-relative
		"../evil.json":                            false,
		"..":                                      false,
		"C:foo":                                   false, // volume name
		"two words":                               false, // grammar: no spaces
		".hidden":                                 false, // grammar: no leading dot
		"x:y":                                     false, // volume-ish token
	}
	for p, wantOK := range valid {
		st := New()
		st.Phase = PhaseIntegrationApplied
		st.Migration = &RecoveryMigrationState{SourcePath: p, SourceSchemaVersion: 1, MigratedAt: "z"}
		err := s.validateNormalizedState(st)
		if wantOK && err != nil {
			t.Errorf("sourcePath %q should be valid, got %v", p, err)
		}
		if !wantOK && err == nil {
			t.Errorf("sourcePath %q should be rejected", p)
		}
	}
	// An absolute path must never pass either.
	absPath := filepath.Join(t.TempDir(), "integration-state.json")
	st := New()
	st.Phase = PhaseIntegrationApplied
	st.Migration = &RecoveryMigrationState{SourcePath: absPath, SourceSchemaVersion: 1, MigratedAt: "z"}
	if err := s.validateNormalizedState(st); err == nil {
		t.Errorf("absolute sourcePath %q must be rejected", absPath)
	}
}
