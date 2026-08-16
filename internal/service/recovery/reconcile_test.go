package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func seedState(t *testing.T, s *Store, st *State) {
	t.Helper()
	if err := s.Write(context.Background(), st); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
}

// boundHome returns the codexHomeFingerprint that matches a Store whose CodexHome
// is dir — the same canonicalization ReconcileStartup performs at boot. dir must
// exist (a non-canonicalizable home is a test bug, not a runtime case).
func boundHome(t *testing.T, dir string) string {
	t.Helper()
	fp, err := CodexHomeFingerprint(dir)
	if err != nil {
		t.Fatalf("boundHome(%q): %v", dir, err)
	}
	return fp
}

// mustFingerprint is boundHome: the fingerprint of a directory that must exist.
func mustFingerprint(t *testing.T, dir string) string {
	t.Helper()
	return boundHome(t, dir)
}

// seedRaw writes st directly to the state file, bypassing Write's
// normalizeForWrite (used to reproduce a persisted legacy/raw state that a
// valid Write would refuse, e.g. an outside-root absolute configPath).
func seedRaw(t *testing.T, s *Store, st *State) {
	t.Helper()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeStateFile(t, s.Path(), data)
}

// runReconcile is a thin helper that runs ReconcileStartup with a readConfig
// returning data and returns the Result together with the persisted state.
func runReconcile(t *testing.T, s *Store, data []byte, readErr error) (*Result, *State) {
	t.Helper()
	ctx := context.Background()
	res, err := s.ReconcileStartup(ctx, func(path string) ([]byte, error) {
		if readErr != nil {
			return nil, readErr
		}
		return data, nil
	})
	if err != nil {
		t.Fatalf("ReconcileStartup error: %v", err)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	return res, st
}

func TestReconcilePendingRestore(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`[model_provider]` + "\nmodel = \"x\"\n")
	cfgHash := HashBytes(cfg)
	previousURL := "http://127.0.0.1:38441/"
	seedState(t, s, &State{
		SchemaVersion:                SchemaVersion,
		IntegrationActive:            true,
		Phase:                        PhaseIntegrationApplied,
		ConfigPath:                   "config.toml",
		CodexHomeFingerprint:         boundHome(t, dir),
		ConfigHashBeforeApply:        HashBytes([]byte("other"))[:16],
		ConfigHashAfterApply:         cfgHash,
		AppliedOpenaiBaseURL:         "http://127.0.0.1:38441/",
		PreviousOpenaiBaseURLPresent: true,
		PreviousOpenaiBaseURL:        &previousURL,
	})
	res, st := runReconcile(t, s, cfg, nil)
	if res.Status != StatusPendingRestore {
		t.Fatalf("status = %q, want pending_restore (detail=%s)", res.Status, res.Detail)
	}
	if !res.StatusReconciled {
		t.Fatal("StatusReconciled should be true")
	}
	if st == nil || !st.IntegrationActive {
		t.Fatal("integration must stay active while pending restore")
	}
	if st.ReconciliationStatus == nil || *st.ReconciliationStatus != "pending_restore" {
		t.Fatalf("reconciliationStatus not persisted: %+v", st)
	}
}

// TestReconcileIntegratedFrontDoor verifies the front-door model's coherent
// integrated state (config at :38440 with a matching applied hash) is classified
// as integrated — not recovery-required — so startup reopens the front door
// instead of demanding a restore.
func TestReconcileIntegratedFrontDoor(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte("openai_base_url = \"http://127.0.0.1:38440\"\n")
	cfgHash := HashBytes(cfg)
	seedState(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     true,
		Phase:                 PhaseIntegrationApplied,
		ConfigPath:            "config.toml",
		CodexHomeFingerprint:  boundHome(t, dir),
		IntegrationTarget:     TargetGateway,
		ConfigHashAfterApply:  cfgHash,
		AppliedOpenaiBaseURL:  "http://127.0.0.1:38440",
	})
	res, st := runReconcile(t, s, cfg, nil)
	if res.Status != StatusIntegrated {
		t.Fatalf("status = %q, want integrated (detail=%s)", res.Status, res.Detail)
	}
	if !res.StatusReconciled {
		t.Fatal("StatusReconciled should be true")
	}
	if st == nil || !st.IntegrationActive {
		t.Fatal("integration must stay active for the integrated front door")
	}
	if res.Phase == nil || *res.Phase != PhaseIntegrationApplied {
		t.Fatalf("phase = %v, want integration_applied", res.Phase)
	}
	if st.ReconciliationStatus == nil || *st.ReconciliationStatus != string(StatusIntegrated) {
		t.Fatalf("reconciliationStatus not persisted: %+v", st)
	}
}

func TestReconcileUnknownPhaseFailsClosedWithoutConfigSideEffect(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte("model = \"x\"\n")
	seedRaw(t, s, &State{
		SchemaVersion: SchemaVersion, IntegrationActive: true, Phase: Phase("finish_started"),
		ConfigPath: "config.toml", CodexHomeFingerprint: boundHome(t, dir),
		ConfigHashAfterApply: HashBytes(cfg), ConfigHashBeforeApply: "different",
		AppliedOpenaiBaseURL: "http://127.0.0.1:38441",
	})
	before := append([]byte(nil), cfg...)
	res, st := runReconcile(t, s, cfg, nil)
	if res.Status != StatusPendingRestore || res.Phase == nil || *res.Phase != PhaseReconciliationReq {
		t.Fatalf("result = %#v, want pending restore/reconciliation_required", res)
	}
	if st == nil || st.Phase != PhaseReconciliationReq || !st.IntegrationActive || HashBytes(before) != HashBytes(cfg) {
		t.Fatalf("state/config after unknown phase reconcile = %#v/%q, want recovery-required and unchanged config", st, cfg)
	}
}

func TestReconcileAlreadyRestored(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`[model_provider]` + "\nmodel = \"y\"\n")
	cfgHash := HashBytes(cfg)
	seedState(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     true,
		Phase:                 PhaseIntegrationApplied,
		ConfigPath:            "config.toml",
		CodexHomeFingerprint:  boundHome(t, dir),
		ConfigHashBeforeApply: cfgHash,
		ConfigHashAfterApply:  HashBytes([]byte("applied"))[:16],
		AppliedOpenaiBaseURL:  "http://127.0.0.1:38441/",
	})
	res, st := runReconcile(t, s, cfg, nil)
	if res.Status != StatusAlreadyRestored {
		t.Fatalf("status = %q, want already_restored", res.Status)
	}
	if st == nil || st.IntegrationActive {
		t.Fatal("already_restored must clear integrationActive")
	}
	if res.Phase != nil && *res.Phase != PhaseReconciledRestored {
		t.Fatalf("phase = %v, want reconciled_restored", *res.Phase)
	}
}

func TestReconcileConflict(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`[changed]` + "\nwhere = \"neither\"\n")
	seedState(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     true,
		Phase:                 PhaseIntegrationApplied,
		ConfigPath:            "config.toml",
		CodexHomeFingerprint:  boundHome(t, dir),
		ConfigHashBeforeApply: HashBytes([]byte("before"))[:16],
		ConfigHashAfterApply:  HashBytes([]byte("after"))[:16],
		AppliedOpenaiBaseURL:  "http://127.0.0.1:38441/",
	})
	res, st := runReconcile(t, s, cfg, nil)
	if res.Status != StatusConfigConflict {
		t.Fatalf("status = %q, want config_conflict", res.Status)
	}
	if st == nil || !st.IntegrationActive {
		t.Fatal("conflict must leave integrationActive untouched (no auto-restore)")
	}
}

func TestReconcileInactive(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`plain`)
	seedState(t, s, &State{
		SchemaVersion:         SchemaVersion,
		Phase:                 PhaseRecovered,
		ConfigPath:            "config.toml",
		CodexHomeFingerprint:  boundHome(t, dir),
		ConfigHashBeforeApply: HashBytes([]byte("b"))[:16],
		ConfigHashAfterApply:  HashBytes([]byte("a"))[:16],
	})
	res, st := runReconcile(t, s, cfg, nil)
	if res.Status != StatusInactive {
		t.Fatalf("status = %q, want inactive", res.Status)
	}
	if res.Phase != nil && *res.Phase != PhaseInactive {
		t.Fatalf("phase = %v, want inactive", *res.Phase)
	}
	_ = st
}

// TestReconcileInactiveClearsStaleDetail: an inactive classification has an empty
// detail, so any stale warning left by a previous boot (e.g. config_conflict)
// must be cleared from the persisted reconciliationDetail — Result.Detail and the
// persisted state stay consistent.
func TestReconcileInactiveClearsStaleDetail(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":false,"phase":"recovered","configPath":"config.toml","codexHomeFingerprint":"` + fp + `","configHashBeforeApply":"bbbb","configHashAfterApply":"aaaa","reconciliationStatus":"inactive","reconciledAt":"2026-08-01T00:00:00Z","reconciliationDetail":"codex設定は元の接続先へ戻されています"}`
	writeStateFile(t, s.Path(), []byte(raw))
	cfg := []byte("neither-before-nor-after")
	res, st := runReconcile(t, s, cfg, nil)
	if res.Status != StatusInactive {
		t.Fatalf("status = %q, want inactive", res.Status)
	}
	if res.Detail != "" {
		t.Fatalf("Result.Detail = %q, want empty", res.Detail)
	}
	if st == nil || st.ReconciliationDetail != nil {
		t.Fatalf("stale reconciliationDetail must be cleared on inactive, got %+v", st.ReconciliationDetail)
	}
}

func TestReconcileUnreadableConfig(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	seedState(t, s, &State{
		SchemaVersion:        SchemaVersion,
		IntegrationActive:    true,
		Phase:                PhaseIntegrationApplied,
		ConfigPath:           "config.toml",
		CodexHomeFingerprint: boundHome(t, dir),
	})
	res, _ := runReconcile(t, s, nil, errors.New("io error"))
	if res.Status != StatusConfigUnreadable {
		t.Fatalf("status = %q, want config_unreadable", res.Status)
	}
}

func TestReconcileConfigPathInvalid(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	// configPath absolute but outside the current CODEX_HOME -> path invalid.
	// The seed is written with normalizeForWrite-disabling raw bytes because a
	// valid Write would already reject an outside-root path.
	seedRaw(t, s, &State{
		SchemaVersion: SchemaVersion,
		Phase:         PhaseIntegrationApplied,
		ConfigPath:    filepath.Join(dir, "..", "outside", "config.toml"),
	})
	res, _ := runReconcile(t, s, []byte("x"), nil)
	if res.Status != StatusConfigPathInvalid {
		t.Fatalf("status = %q, want config_path_invalid", res.Status)
	}
}

func TestReconcileNoStateReturnsInactive(t *testing.T) {
	s, _ := testStore(t)
	res, err := s.ReconcileStartup(context.Background(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != StatusInactive || res.StatusReconciled {
		t.Fatalf("no-state result = %+v", res)
	}
}

// TestReconcileNeverRewritesConfig asserts ReconcileStartup does not touch the
// codex config file and does not boot capture/gateway (only the recovery JSON
// classification is updated). We verify by checking the config file bytes are
// unchanged before/after a classification run.
func TestReconcileNeverRewritesConfig(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`[model_provider]` + "\nmodel = \"z\"\n")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgHash := HashBytes(cfg)
	seedState(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     true,
		Phase:                 PhaseIntegrationApplied,
		ConfigPath:            "config.toml",
		CodexHomeFingerprint:  boundHome(t, dir),
		ConfigHashBeforeApply: cfgHash,
		ConfigHashAfterApply:  HashBytes([]byte("applied"))[:16],
	})
	before, _ := os.ReadFile(cfgPath)
	if _, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) {
		return os.ReadFile(path)
	}); err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatal("ReconcileStartup must not rewrite the codex config")
	}
}

// TestReconcileLegacyAbsoluteConfigPathInsideHome: a legacy Rust v2 file with an
// absolute configPath inside the current CODEX_HOME is readable.
func TestReconcileLegacyAbsoluteConfigPathInsideHome(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "config.toml")
	rel, err := s.resolveConfigPath(&State{ConfigPath: abs})
	if err != nil {
		t.Fatalf("absolute inside-home path rejected: %v", err)
	}
	if rel != filepath.Clean(abs) {
		t.Fatalf("resolved = %q, want %q", rel, filepath.Clean(abs))
	}
}

// TestReconcileLegacyAbsoluteConfigPathOutsideHome: legacy absolute configPath
// outside the current CODEX_HOME is rejected (config_path_invalid).
func TestReconcileLegacyAbsoluteConfigPathOutsideHome(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "..", "somewhere-else", "config.toml")
	if _, err := s.resolveConfigPath(&State{ConfigPath: outside}); err == nil {
		t.Fatal("absolute path outside home must be rejected")
	}
}

// TestReconcileAgainstConcurrentDelete: a Delete on another goroutine racing
// ReconcileStartup is serialized by the Store mutex — the classify callback
// either sees the persisted state or (if Delete won) returns inactive; neither
// path dereferences a nil state. This is the single-transaction guarantee.
func TestReconcileAgainstConcurrentDelete(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`[model_provider]` + "\nmodel = \"r\"\n")
	cfgHash := HashBytes(cfg)
	seedState(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     true,
		Phase:                 PhaseIntegrationApplied,
		ConfigPath:            "config.toml",
		CodexHomeFingerprint:  boundHome(t, dir),
		ConfigHashBeforeApply: HashBytes([]byte("b"))[:16],
		ConfigHashAfterApply:  cfgHash,
		AppliedOpenaiBaseURL:  "http://127.0.0.1:38441/",
	})
	done := make(chan struct{})
	// The competing writer tries to Delete; it blocks until ReconcileStartup's
	// Update releases the mutex. Because classification ran under the same mutex,
	// the callback never observes a nil state.
	go func() {
		defer close(done)
		_ = s.Delete(context.Background())
	}()
	_, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return cfg, nil })
	if err != nil {
		t.Fatalf("ReconcileStartup with racing delete: %v", err)
	}
	<-done
	// A final reconcile on the (now deleted) state must return inactive, not panic.
	res, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return cfg, nil })
	if err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	if res.Status != StatusInactive {
		t.Fatalf("post-delete reconcile = %q, want inactive", res.Status)
	}
}

// TestReconcileRelativeConfigPathMissingFingerprintInvalid: a relative configPath
// without codexHomeFingerprint cannot be root-bound -> config_path_invalid
// (conservative; no auto-restore).
func TestReconcileRelativeConfigPathMissingFingerprintInvalid(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	// A valid Write now requires a fingerprint for a relative configPath, so the
	// no-fingerprint legacy state is reproduced with raw bytes (readable as-is).
	seedRaw(t, s, &State{
		SchemaVersion:     SchemaVersion,
		IntegrationActive: true,
		Phase:             PhaseIntegrationApplied,
		ConfigPath:        "config.toml",
		// no CodexHomeFingerprint
	})
	res, _ := runReconcile(t, s, []byte("x"), nil)
	if res.Status != StatusConfigPathInvalid {
		t.Fatalf("status = %q, want config_path_invalid (detail=%s)", res.Status, res.Detail)
	}
}

// TestReconcileRelativeConfigPathFingerprintMismatchInvalid: a relative configPath
// bound to a DIFFERENT codex home (fingerprint mismatch) -> config_path_invalid.
func TestReconcileRelativeConfigPathFingerprintMismatchInvalid(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	otherHome := filepath.Join(t.TempDir(), "other-home")
	if err := os.MkdirAll(otherHome, 0o755); err != nil {
		t.Fatalf("mkdir other-home: %v", err)
	}
	// A valid Write now rejects a fingerprint that does not match the current
	// home, so the mismatched legacy state is reproduced with raw bytes.
	seedRaw(t, s, &State{
		SchemaVersion:        SchemaVersion,
		IntegrationActive:    true,
		Phase:                PhaseIntegrationApplied,
		ConfigPath:           "config.toml",
		CodexHomeFingerprint: boundHome(t, otherHome),
	})
	res, _ := runReconcile(t, s, []byte("x"), nil)
	if res.Status != StatusConfigPathInvalid {
		t.Fatalf("status = %q, want config_path_invalid (detail=%s)", res.Status, res.Detail)
	}
}

// TestReconcileRejectsRelativeConfigPathLeafSymlinkEscape: a stored relative
// configPath whose final component is a symlink/junction to a file OUTSIDE the
// codex home is rejected at use time with config_path_invalid. The path resolved
// lexically inside root, but its physical location escapes — the use-time
// pathWithinPhysical guard must catch it even though the state was saved with a
// fingerprint that still matches.
func TestReconcileRejectsRelativeConfigPathLeafSymlinkEscape(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.toml")
	if err := os.WriteFile(outside, []byte("[model_provider]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "config.toml")); err != nil {
		t.Skipf("symlink/junction creation unsupported: %v", err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"config.toml","codexHomeFingerprint":"` + fp + `"}`
	writeStateFile(t, s.Path(), []byte(raw))
	res, _ := runReconcile(t, s, []byte("x"), nil)
	if res.Status != StatusConfigPathInvalid {
		t.Fatalf("status = %q, want config_path_invalid (leaf symlink escape)", res.Status)
	}
}

// TestReconcileRejectsRelativeConfigPathMidDirJunctionEscape: a stored relative
// configPath whose intermediate directory is a symlink/junction to a directory
// OUTSIDE the codex home is rejected at use time. The target file itself does not
// exist yet, so the physical check must resolve the existing ancestor (the
// junctioned directory) and reject the escape.
func TestReconcileRejectsRelativeConfigPathMidDirJunctionEscape(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Skipf("symlink/junction creation unsupported: %v", err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"sub/config.toml","codexHomeFingerprint":"` + fp + `"}`
	writeStateFile(t, s.Path(), []byte(raw))
	res, _ := runReconcile(t, s, []byte("x"), nil)
	if res.Status != StatusConfigPathInvalid {
		t.Fatalf("status = %q, want config_path_invalid (mid-directory junction escape)", res.Status)
	}
}

// TestReconcileRejectsConfigPathSwappedToSymlinkAfterSave: a state that reconciled
// normally at save time is re-validated at USE time — after the config target is
// swapped for a symlink pointing outside the codex home, the next reconcile must
// reject it as config_path_invalid instead of reading the outside file.
func TestReconcileRejectsConfigPathSwappedToSymlinkAfterSave(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`[model_provider]`)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"config.toml","codexHomeFingerprint":"` + fp + `"}`
	writeStateFile(t, s.Path(), []byte(raw))
	res, _ := runReconcile(t, s, cfg, nil)
	if res.Status == StatusConfigPathInvalid {
		t.Fatalf("initial reconcile must not be config_path_invalid (plain file inside home): %+v", res)
	}
	// Swap the target: replace the plain file with a symlink to an outside file.
	outside := filepath.Join(t.TempDir(), "outside.toml")
	if err := os.WriteFile(outside, []byte("[changed]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cfgPath); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}
	res2, _ := runReconcile(t, s, []byte("x"), nil)
	if res2.Status != StatusConfigPathInvalid {
		t.Fatalf("post-swap status = %q, want config_path_invalid", res2.Status)
	}
}

// rawStateMap reads the state file back as a map of raw JSON fields, so tests can
// assert preservation of evidence/unknown fields across a reconcile.
func rawStateMap(t *testing.T, s *Store) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	return rawMapOf(t, data)
}

// rawMapOf decodes JSON bytes into per-field raw messages (a duplicate key in the
// input is a test bug — the classification writer rejects it before this point).
func rawMapOf(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// jsonSemanticEqual reports whether two raw JSON values are equal as JSON data.
// Scalar and string fields survive the patch byte-for-byte; nested objects are
// preserved as raw messages (never decoded/re-encoded through a struct) but
// json.MarshalIndent normalizes whitespace inside them, so nested values are
// compared semantically, not byte-for-byte.
func jsonSemanticEqual(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

// The patch-writer tests below exercise writeClassificationLocked via
// ReconcileStartup: the raw JSON is seeded directly (bypassing Write, whose
// normalizeForWrite would refuse legacy absolute paths / missing fingerprints),
// then Reconcile classifies and patches ONLY the classification fields.

// TestReconcilePatchPreservesLegacyAbsoluteConfigPath: a legacy Rust v2 absolute
// configPath (inside the current home) survives reconcile byte-for-byte — the
// classification writer must never re-encode or relativize it.
func TestReconcilePatchPreservesLegacyAbsoluteConfigPath(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "config.toml")
	absJSON, err := json.Marshal(abs)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":` + string(absJSON) + `}`
	writeStateFile(t, s.Path(), []byte(raw))
	if _, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return []byte("x"), nil }); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	m := rawStateMap(t, s)
	if !bytes.Equal(m["configPath"], absJSON) {
		t.Fatalf("legacy absolute configPath not preserved byte-for-byte: %s vs %s", m["configPath"], absJSON)
	}
}

// TestReconcilePatchPreservesCodexHomeFingerprint: the root-binding fingerprint is
// evidence and must survive reconcile unchanged.
func TestReconcilePatchPreservesCodexHomeFingerprint(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"config.toml","codexHomeFingerprint":"` + fp + `"}`
	writeStateFile(t, s.Path(), []byte(raw))
	if _, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return []byte("x"), nil }); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	m := rawStateMap(t, s)
	var got string
	if err := json.Unmarshal(m["codexHomeFingerprint"], &got); err != nil {
		t.Fatal(err)
	}
	if got != fp {
		t.Fatalf("codexHomeFingerprint changed: %q vs %q", got, fp)
	}
}

// TestReconcilePatchPreservesUnknownFields: unknown/future fields survive
// reconcile as JSON values (scalar fields and null stay identical, nested
// objects are preserved semantically per the classification-writer contract,
// with the null-vs-missing distinction intact), and an absent field is never
// invented by the patch writer.
func TestReconcilePatchPreservesUnknownFields(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"config.toml","futureField":{"nested":1,"x":null},"nullField":null}`
	writeStateFile(t, s.Path(), []byte(raw))
	if _, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return []byte("x"), nil }); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	m := rawStateMap(t, s)
	if !jsonSemanticEqual(t, m["futureField"], json.RawMessage(`{"nested":1,"x":null}`)) {
		t.Fatalf("unknown nested field not preserved: %s", m["futureField"])
	}
	if !bytes.Equal(m["nullField"], json.RawMessage("null")) {
		t.Fatalf("null field not preserved: %s", m["nullField"])
	}
	if _, present := m["someAbsentField"]; present {
		t.Fatal("patch must not invent absent fields")
	}
}

// TestReconcilePatchPreservesEvidenceFields: every evidence field (operationId,
// config hashes, backupPath, autoLog, migration) survives reconcile unchanged.
func TestReconcilePatchPreservesEvidenceFields(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"config.toml","codexHomeFingerprint":"` + fp + `","operationId":"op-9","configHashBeforeApply":"bbbb","configHashAfterApply":"aaaa","backupPath":"b.json","autoLog":{"sessionId":"s-1","path":"sess.log","lastCheckpointSequence":3},"migration":{"sourcePath":"integration-state.json","sourceSchemaVersion":1,"migratedAt":"z"}}`
	writeStateFile(t, s.Path(), []byte(raw))
	if _, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return []byte("x"), nil }); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	before := rawMapOf(t, []byte(raw))
	after := rawStateMap(t, s)
	for _, key := range []string{"operationId", "configPath", "codexHomeFingerprint", "configHashBeforeApply", "configHashAfterApply", "backupPath"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Errorf("evidence field %s changed: %s -> %s", key, before[key], after[key])
		}
	}
	for _, key := range []string{"autoLog", "migration"} { // nested objects: semantic equality
		if !jsonSemanticEqual(t, after[key], before[key]) {
			t.Errorf("evidence field %s changed: %s -> %s", key, before[key], after[key])
		}
	}
}

// TestReconcilePatchRejectsNonClassificationChange: a callback that mutates a
// non-classification field (configPath here) is rejected with reconcile_failed
// and the file is left byte-for-byte untouched.
func TestReconcilePatchRejectsNonClassificationChange(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"config.toml","codexHomeFingerprint":"` + fp + `"}`
	writeStateFile(t, s.Path(), []byte(raw))
	err := s.updateReconciled(context.Background(), func(cur *State) error {
		cur.ConfigPath = "hacked.toml"
		return nil
	})
	var rerr *Error
	if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindReconcileFailed {
		t.Fatalf("expected reconcile_failed, got %v", err)
	}
	after, _ := os.ReadFile(s.Path())
	if string(after) != raw {
		t.Fatal("a rejected reconcile must not touch the state file")
	}
}

// TestReconcileConfigPathInvalidTwiceKeepsEvidence: repeated config_path_invalid
// reconciles keep the legacy absolute configPath and unknown fields intact.
func TestReconcileConfigPathInvalidTwiceKeepsEvidence(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "..", "outside", "config.toml")
	absJSON, err := json.Marshal(abs)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":` + string(absJSON) + `,"operationId":"op-keep","futureField":{"n":1}}`
	writeStateFile(t, s.Path(), []byte(raw))
	for i := 0; i < 2; i++ {
		if _, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return []byte("x"), nil }); err != nil {
			t.Fatalf("reconcile #%d: %v", i+1, err)
		}
		m := rawStateMap(t, s)
		if !bytes.Equal(m["configPath"], absJSON) {
			t.Fatalf("configPath not preserved after reconcile #%d: %s", i+1, m["configPath"])
		}
		if !jsonSemanticEqual(t, m["futureField"], json.RawMessage(`{"n":1}`)) {
			t.Fatalf("futureField not preserved after reconcile #%d: %s", i+1, m["futureField"])
		}
	}
}

// TestReconcilePatchUpdatesUpdatedAt: a classification stamps reconciledAt and
// updatedAt with the same timestamp (single generation).
func TestReconcilePatchUpdatesUpdatedAt(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	fp := boundHome(t, dir)
	raw := `{"schemaVersion":2,"integrationActive":true,"phase":"integration_applied","configPath":"config.toml","codexHomeFingerprint":"` + fp + `"}`
	writeStateFile(t, s.Path(), []byte(raw))
	if _, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return []byte("x"), nil }); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	st, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.ReconciledAt == nil || st.UpdatedAt == nil {
		t.Fatal("reconcile must stamp reconciledAt and updatedAt")
	}
	if *st.ReconciledAt != *st.UpdatedAt {
		t.Fatalf("reconciledAt (%q) and updatedAt (%q) must share one timestamp", *st.ReconciledAt, *st.UpdatedAt)
	}
}

// TestReconcilePatchRejectsDuplicateKey: a duplicate top-level JSON key is
// rejected with recovery_state_parse_failed and the original bytes are kept.
func TestReconcilePatchRejectsDuplicateKey(t *testing.T) {
	s, _ := testStore(t)
	raw := `{"schemaVersion":2,"phase":"integration_applied","phase":"prepared"}`
	writeStateFile(t, s.Path(), []byte(raw))
	_, err := s.ReconcileStartup(context.Background(), func(path string) ([]byte, error) { return []byte("x"), nil })
	var rerr *Error
	if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindStateParseFailed {
		t.Fatalf("duplicate key must be parse_failed, got %v", err)
	}
	after, _ := os.ReadFile(s.Path())
	if string(after) != raw {
		t.Fatal("duplicate-key rejection must not modify the file")
	}
}

// TestReconcileDuplicateKeyNeverCallsConfigReader: a duplicate top-level key is
// rejected BEFORE the recovery state is decoded to a State, so the external
// readConfig callback (which is driven by the decoded content) is never invoked
// with an ambiguous document. The file is left untouched.
func TestReconcileDuplicateKeyNeverCallsConfigReader(t *testing.T) {
	s, _ := testStore(t)
	raw := `{"schemaVersion":2,"phase":"integration_applied","phase":"prepared"}`
	writeStateFile(t, s.Path(), []byte(raw))
	called := false
	_, err := s.ReconcileStartup(context.Background(), func(string) ([]byte, error) {
		called = true
		return nil, nil
	})
	var rerr *Error
	if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindStateParseFailed {
		t.Fatalf("duplicate key must be parse_failed before any callback, got %v", err)
	}
	if called {
		t.Fatal("readConfig must not be called for a duplicate-key JSON")
	}
	after, _ := os.ReadFile(s.Path())
	if string(after) != raw {
		t.Fatal("duplicate-key rejection must not modify the file")
	}
}

// TestReconcileNilConfigReaderReturnsErrorWhenStateExists: a persisted state with
// a nil config reader is reconcile_failed (never a panic), while a missing state
// with a nil reader stays inactive (TestReconcileNoStateReturnsInactive).
func TestReconcileNilConfigReaderReturnsErrorWhenStateExists(t *testing.T) {
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	seedState(t, s, &State{
		SchemaVersion:        SchemaVersion,
		IntegrationActive:    true,
		Phase:                PhaseIntegrationApplied,
		ConfigPath:           "config.toml",
		CodexHomeFingerprint: boundHome(t, dir),
	})
	_, err := s.ReconcileStartup(context.Background(), nil)
	var rerr *Error
	if err == nil || !errors.As(err, &rerr) || rerr.Kind != KindReconcileFailed {
		t.Fatalf("nil readConfig with a persisted state must be reconcile_failed, got %v", err)
	}
}
