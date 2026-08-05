package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newMigrateStore builds a Store whose APPDATA resolves to appdata (for the v1
// source) and whose v2 path lives under appdataRoot.
func newMigrateStore(t *testing.T, appdata string, paths *Paths) (*Store, string) {
	t.Helper()
	appdataRoot := filepath.Join(appdata, "Moon Bridge")
	if paths == nil {
		paths = &Paths{AppDataRoot: appdataRoot}
	}
	v2 := filepath.Join(appdataRoot, "recovery", "recovery-state-v2.json")
	s, err := NewStore(paths, v2)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.env = func(k string) string {
		if k == "APPDATA" {
			return appdata
		}
		return os.Getenv(k)
	}
	return s, appdataRoot
}

func writeV1(t *testing.T, appdata string, content string) string {
	t.Helper()
	dir := filepath.Join(appdata, "Moon Bridge Desktop", "traffic-analysis")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "integration-state.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// realV1Fixture is the canonical Rust v1 shape (see
// desktop/src-tauri/src/traffic_analysis.rs test recovery_v1_is_migrated_without_losing_original_bytes):
// schemaVersion:1, integrationActive (NOT integrationApplied), configPath is an
// absolute Windows path.
const realV1Fixture = `{
	"schemaVersion": 1,
	"integrationActive": false,
	"phase": "integration_applied",
	"operationId": "legacy-op",
	"configPath": "C:\\fixture\\codex\\config.toml",
	"previousOpenaiBaseUrlPresent": false,
	"appliedOpenaiBaseUrl": "http://127.0.0.1:38441/",
	"configHashBeforeApply": "before",
	"configHashAfterApply": "after",
	"startedAt": "2026-08-03T00:00:00Z"
}`

func migrateArchiveNames(t *testing.T, appdataRoot string) []string {
	t.Helper()
	archives, err := filepath.Glob(filepath.Join(appdataRoot, "recovery", "migrated-v1", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return archives
}

func TestMigrateRealV1Fixture(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, appdataRoot := newMigrateStore(t, appdata, nil)
	v1 := writeV1(t, appdata, realV1Fixture)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := os.Stat(v1); err != nil {
		t.Fatalf("v1 source must not be deleted: %v", err)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load v2: %v", err)
	}
	if st == nil {
		t.Fatal("v2 not created")
	}
	if st.SchemaVersion != 2 {
		t.Fatalf("schemaVersion = %d, want 2", st.SchemaVersion)
	}
	// integrationActive=false with an active-looking phase must map to recovered,
	// matching the Rust migrate_recovery_state (phase from integration_active).
	if st.IntegrationActive || st.Phase != PhaseRecovered {
		t.Fatalf("phase/active = %q/%v, want recovered/false", st.Phase, st.IntegrationActive)
	}
	if st.Migration == nil || st.Migration.SourceSchemaVersion != 1 {
		t.Fatalf("migration metadata missing: %+v", st.Migration)
	}
	if st.Migration.SourcePath == "" || filepath.IsAbs(st.Migration.SourcePath) {
		t.Fatalf("migration.sourcePath must be relative/logical, got %q", st.Migration.SourcePath)
	}
	// The fixture's absolute configPath cannot be bound (no CODEX_HOME here): it
	// must be cleared and diagnosed config_path_invalid, never stored.
	if st.ConfigPath != "" {
		t.Fatalf("unbindable absolute configPath must be cleared, got %q", st.ConfigPath)
	}
	if st.CodexHomeFingerprint != "" {
		t.Fatalf("no fingerprint without a bound codex home, got %q", st.CodexHomeFingerprint)
	}
	if st.ReconciliationStatus == nil || *st.ReconciliationStatus != string(StatusConfigPathInvalid) {
		t.Fatalf("reconciliationStatus = %v, want config_path_invalid", st.ReconciliationStatus)
	}
	if st.ReconciliationDetail == nil || *st.ReconciliationDetail == "" {
		t.Fatal("reconciliationDetail must explain the invalid path")
	}
	if st.AppliedOpenaiBaseURL != "http://127.0.0.1:38441/" {
		t.Fatalf("appliedOpenaiBaseUrl must be preserved, got %q", st.AppliedOpenaiBaseURL)
	}
	if len(migrateArchiveNames(t, appdataRoot)) != 1 {
		t.Fatal("expected exactly 1 archive")
	}
}

func TestMigrateIntegrationAppliedConversion(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, _ := newMigrateStore(t, appdata, nil)
	// integrationActive:true must map to phase integration_applied.
	writeV1(t, appdata, `{"schemaVersion":1,"integrationActive":true,"phase":"y","configPath":"c"}`)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, _ := s.Load(ctx)
	if st == nil || !st.IntegrationActive || st.Phase != PhaseIntegrationApplied {
		t.Fatalf("integrationActive=true -> integration_applied failed: %+v", st)
	}
}

func TestMigrateArchiveDoesNotProliferate(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, appdataRoot := newMigrateStore(t, appdata, nil)
	writeV1(t, appdata, realV1Fixture)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// v2 now exists; a second Migrate returns early without re-archiving.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if n := len(migrateArchiveNames(t, appdataRoot)); n != 1 {
		t.Fatalf("archive must not proliferate, got %d", n)
	}
}

// TestMigrateUnparseableV1ReturnsErrorAndArchivesOnce: archiving unparseable v1
// is not success — Migrate must return migration_failed, write no v2, and never
// double-archive the same bytes on re-run.
func TestMigrateUnparseableV1ReturnsErrorAndArchivesOnce(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, appdataRoot := newMigrateStore(t, appdata, nil)
	writeV1(t, appdata, `{not valid json`)

	err := s.Migrate(ctx)
	var rerr *Error
	if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindMigrationFailed {
		t.Fatalf("unparseable v1 must return migration_failed, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(appdataRoot, "recovery", "recovery-state-v2.json")); !os.IsNotExist(serr) {
		t.Fatal("unparseable v1 must not create a v2")
	}
	// Re-run must still error (archiving is not success) but never double-archive.
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("second Migrate must still report migration_failed")
	}
	if n := len(migrateArchiveNames(t, appdataRoot)); n != 1 {
		t.Fatalf("unparseable v1 must be archived exactly once, got %d archives: %v", n, migrateArchiveNames(t, appdataRoot))
	}
}

func TestMigrateNoV1Noop(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, appdataRoot := newMigrateStore(t, appdata, nil)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate without v1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appdataRoot, "recovery", "recovery-state-v2.json")); !os.IsNotExist(err) {
		t.Fatal("no v2 should be created when v1 is absent")
	}
}

func TestMigrateV1PathResolution(t *testing.T) {
	appdata := t.TempDir()
	s, _ := newMigrateStore(t, appdata, nil)
	p := v1SourcePath(s.env)
	want := filepath.Join(appdata, "Moon Bridge Desktop", "traffic-analysis", "integration-state.json")
	if p != want {
		t.Fatalf("v1SourcePath = %q, want %q", p, want)
	}
}

func TestUniqueMigrationArchiveNameFormat(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "integration-state-x")
	p, err := uniqueMigrationArchivePath(base)
	if err != nil {
		t.Fatal(err)
	}
	// First candidate is <base>.json (no suffix).
	if want := base + ".json"; p != want {
		t.Fatalf("first archive = %q, want %q", p, want)
	}
	// Simulate a collision to force the -001.json form.
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := uniqueMigrationArchivePath(base)
	if err != nil {
		t.Fatal(err)
	}
	want2 := base + "-001.json"
	if p2 != want2 {
		t.Fatalf("collision archive = %q, want %q (suffix before .json)", p2, want2)
	}
}

func TestMigrateRelativizesAbsoluteConfigPathInsideHome(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	home := filepath.Join(appdata, "codex-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := newMigrateStore(t, appdata, &Paths{
		AppDataRoot: filepath.Join(appdata, "Moon Bridge"),
		CodexHome:   home,
	})
	cfgAbs := filepath.Join(home, "config.toml")
	v1rec := legacyV1State{SchemaVersion: 1, ConfigPath: cfgAbs}
	data, err := json.Marshal(v1rec)
	if err != nil {
		t.Fatal(err)
	}
	writeV1(t, appdata, string(data))
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, _ := s.Load(ctx)
	if st == nil {
		t.Fatal("v2 not created")
	}
	if st.ConfigPath != "config.toml" {
		t.Fatalf("configPath = %q, want config.toml", st.ConfigPath)
	}
	if st.CodexHomeFingerprint != boundHome(t, home) {
		t.Fatalf("codexHomeFingerprint = %q, want %q", st.CodexHomeFingerprint, boundHome(t, home))
	}
	if st.ReconciliationStatus != nil {
		t.Fatalf("no diagnosis when configPath relativizes, got %v", *st.ReconciliationStatus)
	}
}

// TestMigrateRejectsAbsoluteConfigPathEscapingViaJunction: a legacy absolute
// configPath that is lexically inside the codex home but physically escapes
// through a symlink/junction to an outside directory must NOT be relativized into
// v2. The migration absolute-path check is physical (pathWithinPhysical), so the
// path is cleared and diagnosed config_path_invalid — no absolute or
// outside-resolving path reaches v2.
func TestMigrateRejectsAbsoluteConfigPathEscapingViaJunction(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	home := filepath.Join(appdata, "codex-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(appdata, "outside-home")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "linked")); err != nil {
		t.Skipf("symlink/junction creation unsupported: %v", err)
	}
	s, _ := newMigrateStore(t, appdata, &Paths{
		AppDataRoot: filepath.Join(appdata, "Moon Bridge"),
		CodexHome:   home,
	})
	// Lexically under home, physically outside via the junction.
	v1rec := legacyV1State{SchemaVersion: 1, ConfigPath: filepath.Join(home, "linked", "config.toml")}
	data, err := json.Marshal(v1rec)
	if err != nil {
		t.Fatal(err)
	}
	writeV1(t, appdata, string(data))
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, _ := s.Load(ctx)
	if st == nil {
		t.Fatal("v2 not created")
	}
	if st.ConfigPath != "" {
		t.Fatalf("junction-escaped configPath must be cleared, got %q", st.ConfigPath)
	}
	if st.CodexHomeFingerprint != "" {
		t.Fatalf("no fingerprint for an escaped configPath, got %q", st.CodexHomeFingerprint)
	}
	if st.ReconciliationStatus == nil || *st.ReconciliationStatus != string(StatusConfigPathInvalid) {
		t.Fatalf("reconciliationStatus = %v, want config_path_invalid", st.ReconciliationStatus)
	}
}

func TestMigrateAbsoluteConfigPathUnknownHomeDiagnosesConfigPathInvalid(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, _ := newMigrateStore(t, appdata, nil) // CODEX_HOME unset
	v1rec := legacyV1State{SchemaVersion: 1, ConfigPath: filepath.Join(appdata, "elsewhere", "config.toml")}
	data, err := json.Marshal(v1rec)
	if err != nil {
		t.Fatal(err)
	}
	writeV1(t, appdata, string(data))
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, _ := s.Load(ctx)
	if st == nil {
		t.Fatal("v2 not created")
	}
	if st.ConfigPath != "" || st.CodexHomeFingerprint != "" {
		t.Fatalf("unbound absolute configPath must be cleared, got %q/%q", st.ConfigPath, st.CodexHomeFingerprint)
	}
	if st.ReconciliationStatus == nil || *st.ReconciliationStatus != string(StatusConfigPathInvalid) {
		t.Fatalf("reconciliationStatus = %v, want config_path_invalid", st.ReconciliationStatus)
	}
}

func TestMigrateNoAbsolutePathInMigratedV2(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	home := filepath.Join(appdata, "codex-home")
	backupDir := filepath.Join(appdata, "backups", "codex-config")
	logDir := filepath.Join(appdata, "logs", "traffic-analysis")
	for _, d := range []string{home, backupDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, appdataRoot := newMigrateStore(t, appdata, &Paths{
		AppDataRoot:   filepath.Join(appdata, "Moon Bridge"),
		CodexHome:     home,
		BackupDir:     backupDir,
		TrafficLogDir: logDir,
	})
	// All three paths point OUTSIDE their allowed roots, so none can be relativized.
	cfgAbs := filepath.Join(appdata, "elsewhere", "config.toml")
	bAbs := filepath.Join(appdata, "elsewhere", "backup.json")
	lAbs := filepath.Join(appdata, "elsewhere", "sess.log")
	v1rec := legacyV1State{
		SchemaVersion: 1,
		ConfigPath:    cfgAbs,
		BackupPath:    &bAbs,
		AutoLog:       &AutoLogRecoveryState{SessionID: "s-1", Path: lAbs, LastCheckpointSequence: 2},
	}
	data, err := json.Marshal(v1rec)
	if err != nil {
		t.Fatal(err)
	}
	writeV1(t, appdata, string(data))
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(appdataRoot, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	norm := filepath.ToSlash(string(raw))
	for _, abs := range []string{cfgAbs, bAbs, lAbs, home} {
		if strings.Contains(norm, filepath.ToSlash(abs)) {
			t.Fatalf("v2 contains an absolute path %q", abs)
		}
	}
	st, _ := s.Load(ctx)
	if st == nil {
		t.Fatal("v2 not created")
	}
	if st.BackupPath != nil {
		t.Fatalf("outside-root backupPath must be dropped, got %q", *st.BackupPath)
	}
	if st.AutoLog == nil || st.AutoLog.Path != "" {
		t.Fatalf("outside-root autoLog.path must be dropped, got %+v", st.AutoLog)
	}
	if st.AutoLog == nil || st.AutoLog.SessionID != "s-1" {
		t.Fatal("autoLog.sessionId must be preserved even when the path is dropped")
	}
}

// TestMigrateV1PreservesAllRustFields migrates a v1 fixture that sets every
// Rust RecoveryState field and asserts each survives into v2 (paths relativized,
// optional pointers preserved, appliedOpenaiBaseUrl NOT fabricated).
func TestMigrateV1PreservesAllRustFields(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	home := filepath.Join(appdata, "codex-home")
	backupDir := filepath.Join(appdata, "backups", "codex-config")
	logDir := filepath.Join(appdata, "logs", "traffic-analysis")
	for _, d := range []string{home, backupDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := newMigrateStore(t, appdata, &Paths{
		AppDataRoot:   filepath.Join(appdata, "Moon Bridge"),
		CodexHome:     home,
		BackupDir:     backupDir,
		TrafficLogDir: logDir,
	})
	prev := "https://api.openai.com/v1"
	updatedAt := "2026-08-04T01:00:00Z"
	autoLogStatus := "running"
	recStatus := "pending_restore"
	reconciledAt := "2026-08-04T00:00:00Z"
	recDetail := "kept"
	backup := filepath.Join(backupDir, "legacy.json")
	autolog := filepath.Join(logDir, "legacy.log")
	cfgAbs := filepath.Join(home, "config.toml")
	v1rec := legacyV1State{
		SchemaVersion:                1,
		IntegrationActive:            true,
		Phase:                        "integration_applied",
		OperationID:                  "op-keep",
		ConfigPath:                   cfgAbs,
		PreviousOpenaiBaseURLPresent: true,
		PreviousOpenaiBaseURL:        &prev,
		AppliedOpenaiBaseURL:         "",
		ConfigHashBeforeApply:        "bbbb",
		ConfigHashAfterApply:         "aaaa",
		BackupPath:                   &backup,
		StartedAt:                    "2026-08-03T00:00:00Z",
		UpdatedAt:                    &updatedAt,
		AutoLog:                      &AutoLogRecoveryState{SessionID: "s-1", Path: autolog, LastCheckpointSequence: 7, Finalized: true},
		AutoLogStatus:                &autoLogStatus,
		UnsavedObservationsMayRemain: true,
		UnsavedDiscardConfirmed:      true,
		CaptureStateLastKnown:        "capturing",
		RelayActiveLastKnown:         true,
		ReconciliationStatus:         &recStatus,
		ReconciledAt:                 &reconciledAt,
		ReconciliationDetail:         &recDetail,
		RestartAttempted:             true,
	}
	data, err := json.Marshal(v1rec)
	if err != nil {
		t.Fatal(err)
	}
	writeV1(t, appdata, string(data))
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, _ := s.Load(ctx)
	if st == nil {
		t.Fatal("v2 not created")
	}
	check := func(name, got, want string) {
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	check("operationId", st.OperationID, "op-keep")
	if !st.PreviousOpenaiBaseURLPresent || st.PreviousOpenaiBaseURL == nil || *st.PreviousOpenaiBaseURL != prev {
		t.Errorf("previousOpenaiBaseUrl not preserved: present=%v url=%v", st.PreviousOpenaiBaseURLPresent, st.PreviousOpenaiBaseURL)
	}
	check("appliedOpenaiBaseUrl", st.AppliedOpenaiBaseURL, "")
	check("configHashBeforeApply", st.ConfigHashBeforeApply, "bbbb")
	check("configHashAfterApply", st.ConfigHashAfterApply, "aaaa")
	if st.BackupPath == nil || *st.BackupPath != "legacy.json" {
		t.Errorf("backupPath not relativized: %v", st.BackupPath)
	}
	check("startedAt", st.StartedAt, "2026-08-03T00:00:00Z")
	if st.AutoLog == nil {
		t.Fatal("autoLog not preserved")
	}
	check("autoLog.sessionId", st.AutoLog.SessionID, "s-1")
	check("autoLog.path", st.AutoLog.Path, "legacy.log")
	if st.AutoLog.LastCheckpointSequence != 7 || !st.AutoLog.Finalized {
		t.Errorf("autoLog checkpoint/finalized not preserved: %+v", st.AutoLog)
	}
	if st.AutoLogStatus == nil || *st.AutoLogStatus != "running" {
		t.Errorf("autoLogStatus = %v", st.AutoLogStatus)
	}
	if !st.UnsavedObservationsMayRemain || !st.UnsavedDiscardConfirmed {
		t.Errorf("unsaved flags not preserved: %+v", st)
	}
	check("captureStateLastKnown", st.CaptureStateLastKnown, "capturing")
	if !st.RelayActiveLastKnown {
		t.Errorf("relayActiveLastKnown not preserved")
	}
	if st.ReconciliationStatus == nil || *st.ReconciliationStatus != "pending_restore" {
		t.Errorf("reconciliationStatus = %v", st.ReconciliationStatus)
	}
	if st.ReconciledAt == nil || *st.ReconciledAt != reconciledAt {
		t.Errorf("reconciledAt = %v", st.ReconciledAt)
	}
	if st.ReconciliationDetail == nil || *st.ReconciliationDetail != "kept" {
		t.Errorf("reconciliationDetail = %v", st.ReconciliationDetail)
	}
	if !st.RestartAttempted {
		t.Errorf("restartAttempted not preserved")
	}
	check("configPath", st.ConfigPath, "config.toml")
	if st.CodexHomeFingerprint != boundHome(t, home) {
		t.Errorf("codexHomeFingerprint = %q, want %q", st.CodexHomeFingerprint, boundHome(t, home))
	}
	if st.Migration == nil || st.Migration.SourceSchemaVersion != 1 {
		t.Errorf("migration metadata missing/wrong: %+v", st.Migration)
	}
}

func TestMigrateV1RejectsSchemaVersion2AndUnknown(t *testing.T) {
	ctx := context.Background()
	for _, ver := range []int{2, 3} {
		appdata := t.TempDir()
		s, appdataRoot := newMigrateStore(t, appdata, nil)
		writeV1(t, appdata, fmt.Sprintf(`{"schemaVersion":%d,"phase":"x"}`, ver))
		err := s.Migrate(ctx)
		var rerr *Error
		if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindMigrationFailed {
			t.Fatalf("schemaVersion=%d: expected migration_failed, got %v", ver, err)
		}
		if _, serr := os.Stat(filepath.Join(appdataRoot, "recovery", "recovery-state-v2.json")); !os.IsNotExist(serr) {
			t.Fatalf("schemaVersion=%d: must not write v2", ver)
		}
	}
}

func TestMigrateDoesNotFabricateAppliedOpenaiBaseURL(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, _ := newMigrateStore(t, appdata, nil)
	writeV1(t, appdata, `{"schemaVersion":1,"integrationActive":true,"configPath":"c","appliedOpenaiBaseUrl":""}`)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, _ := s.Load(ctx)
	if st == nil {
		t.Fatal("v2 not created")
	}
	if st.AppliedOpenaiBaseURL != "" {
		t.Fatalf("appliedOpenaiBaseUrl must not be fabricated, got %q", st.AppliedOpenaiBaseURL)
	}
}

func TestMigrateConcurrentSerialized(t *testing.T) {
	appdata := t.TempDir()
	s, appdataRoot := newMigrateStore(t, appdata, nil)
	writeV1(t, appdata, realV1Fixture)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Migrate(context.Background()); err != nil {
				t.Errorf("Migrate: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := len(migrateArchiveNames(t, appdataRoot)); n != 1 {
		t.Fatalf("concurrent Migrate must archive exactly once, got %d", n)
	}
}

func TestMigrateSetsCodexHomeFingerprintWhenRelativizable(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	home := filepath.Join(appdata, "codex-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := newMigrateStore(t, appdata, &Paths{
		AppDataRoot: filepath.Join(appdata, "Moon Bridge"),
		CodexHome:   home,
	})
	v1rec := legacyV1State{SchemaVersion: 1, IntegrationActive: true, ConfigPath: filepath.Join(home, "config.toml")}
	data, err := json.Marshal(v1rec)
	if err != nil {
		t.Fatal(err)
	}
	writeV1(t, appdata, string(data))
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, _ := s.Load(ctx)
	want := boundHome(t, home)
	if st == nil || st.CodexHomeFingerprint != want {
		t.Fatalf("codexHomeFingerprint = %q, want %q", codexFpOf(st), want)
	}
	// When the home is unknown the fingerprint stays unset (config_path_invalid
	// diagnosis instead, never an absolute path).
	appdata2 := t.TempDir()
	s2, _ := newMigrateStore(t, appdata2, nil)
	writeV1(t, appdata2, `{"schemaVersion":1,"integrationActive":true,"configPath":"C:\\x\\config.toml"}`)
	if err := s2.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (no home): %v", err)
	}
	st2, _ := s2.Load(ctx)
	if st2 == nil || st2.CodexHomeFingerprint != "" {
		t.Fatalf("fingerprint must be unset when home is unknown, got %q", codexFpOf(st2))
	}
}

func codexFpOf(st *State) string {
	if st == nil {
		return ""
	}
	return st.CodexHomeFingerprint
}

// TestMigrateArchiveFailureAfterV2ReturnsNil: once the v2 write is durable, an
// archive failure is only a sanitized warning — Migrate returns nil, the v2 state
// is readable, the v1 source is never deleted, and a re-run is a no-op.
func TestMigrateArchiveFailureAfterV2ReturnsNil(t *testing.T) {
	ctx := context.Background()
	appdata := t.TempDir()
	s, appdataRoot := newMigrateStore(t, appdata, nil)
	v1 := writeV1(t, appdata, realV1Fixture)
	// Block the archive step: recovery/migrated-v1 exists as a FILE, so
	// archiveV1Named's MkdirAll fails — but only after the v2 write succeeded.
	recoveryDir := filepath.Join(appdataRoot, "recovery")
	if err := os.MkdirAll(recoveryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recoveryDir, "migrated-v1"), []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("archive failure after durable v2 must return nil, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(recoveryDir, "recovery-state-v2.json")); err != nil {
		t.Fatalf("v2 must be durable despite archive failure: %v", err)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load v2: %v", err)
	}
	if st == nil || st.SchemaVersion != 2 {
		t.Fatalf("v2 not readable: %+v", st)
	}
	if _, err := os.Stat(v1); err != nil {
		t.Fatalf("v1 source must never be deleted: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate must also return nil, got %v", err)
	}
}
