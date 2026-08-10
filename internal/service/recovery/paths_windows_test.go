package recovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// verbatimDrive converts an absolute path into its Windows extended-length
// (`\\?\`) spelling. `\\?\C:\...` and `C:\...` denote the same physical file;
// a legacy writer persisted the verbatim form, which the verbatim fix must
// resolve as being inside CodexHome.
func verbatimDrive(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return `\\?\` + abs
}

// TestPathWithinPhysicalVerbatimDrive verifies pathWithinPhysical treats the
// verbatim spelling of an inside-home path as contained and the verbatim
// spelling of an outside-home path as not contained.
func TestPathWithinPhysicalVerbatimDrive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows verbatim path semantics")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !pathWithinPhysical(dir, verbatimDrive(t, configPath)) {
		t.Fatalf("verbatim %q should resolve inside %q", verbatimDrive(t, configPath), dir)
	}
	if pathWithinPhysical(dir, verbatimDrive(t, filepath.Join(filepath.Dir(dir), "elsewhere"))) {
		t.Fatal("verbatim path outside home must not be within")
	}
}

// TestReconcileLegacyVerbatimPathCleanInactive is the regression test for the
// cold-start bug: a stale legacy state whose configPath is a verbatim absolute
// path must now resolve against the real config, classify as inactive (the
// config is neither applied nor the pre-apply original), and self-heal. A
// second ReconcileStartup must keep it inactive (no phase oscillation).
func TestReconcileLegacyVerbatimPathCleanInactive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows verbatim path semantics")
	}
	ctx := context.Background()
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte("model = \"user-edited\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	seedRaw(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     false,
		Phase:                 PhaseReconciliationConf,
		ConfigPath:            verbatimDrive(t, filepath.Join(dir, "config.toml")),
		ConfigHashBeforeApply: HashBytes([]byte("stale-before")),
		ConfigHashAfterApply:  HashBytes([]byte("stale-after")),
	})
	first, err := s.ReconcileStartup(ctx, os.ReadFile)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if first.Status != StatusInactive || first.Phase == nil || *first.Phase != PhaseInactive {
		t.Fatalf("first reconcile = %#v, want inactive/PhaseInactive", first)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Phase != PhaseInactive ||
		st.ReconciliationStatus == nil || *st.ReconciliationStatus != string(StatusInactive) {
		t.Fatalf("persisted state = %+v, want PhaseInactive/inactive", st)
	}
	second, err := s.ReconcileStartup(ctx, os.ReadFile)
	if err != nil {
		t.Fatalf("second ReconcileStartup: %v", err)
	}
	if second.Phase == nil || *second.Phase != PhaseInactive {
		t.Fatalf("second reconcile = %#v, want PhaseInactive (no oscillation)", second)
	}
	st2, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st2 == nil || st2.Phase != PhaseInactive {
		t.Fatalf("state after second reconcile = %+v, want PhaseInactive", st2)
	}
}

// TestReconcileLegacyVerbatimPathStillAppliedPendingRestore verifies the fix
// does not weaken recovery safety: a verbatim path whose config is still
// capture-applied (hash == configHashAfterApply) must stay pending_restore.
func TestReconcileLegacyVerbatimPathStillAppliedPendingRestore(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows verbatim path semantics")
	}
	ctx := context.Background()
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte("openai_base_url = \"http://127.0.0.1:38441/\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	seedRaw(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     false,
		Phase:                 PhaseReconciliationConf,
		ConfigPath:            verbatimDrive(t, filepath.Join(dir, "config.toml")),
		ConfigHashBeforeApply: HashBytes([]byte("stale-before")),
		ConfigHashAfterApply:  HashBytes(cfg),
		AppliedOpenaiBaseURL:  "http://127.0.0.1:38441/",
	})
	res, err := s.ReconcileStartup(ctx, os.ReadFile)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if res.Status != StatusPendingRestore || res.Phase == nil || *res.Phase != PhaseReconciliationReq {
		t.Fatalf("reconcile = %#v, want pending_restore/reconciliation_required", res)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Phase != PhaseReconciliationReq {
		t.Fatalf("persisted state = %+v, want PhaseReconciliationReq", st)
	}
}

// TestReconcileLegacyVerbatimPathOutsideHomeConfigPathInvalid verifies a
// verbatim path pointing outside CodexHome still fails closed.
func TestReconcileLegacyVerbatimPathOutsideHomeConfigPathInvalid(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows verbatim path semantics")
	}
	ctx := context.Background()
	s, dir := testStore(t)
	if err := s.SetCodexHome(dir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(dir), "other", "config.toml")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedRaw(t, s, &State{
		SchemaVersion:         SchemaVersion,
		IntegrationActive:     false,
		Phase:                 PhaseReconciliationConf,
		ConfigPath:            verbatimDrive(t, outside),
		ConfigHashBeforeApply: HashBytes([]byte("stale-before")),
		ConfigHashAfterApply:  HashBytes([]byte("stale-after")),
	})
	res, err := s.ReconcileStartup(ctx, os.ReadFile)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if res.Status != StatusConfigPathInvalid {
		t.Fatalf("reconcile = %#v, want config_path_invalid", res)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Phase != PhaseReconciliationConf ||
		st.ReconciliationStatus == nil || *st.ReconciliationStatus != string(StatusConfigPathInvalid) {
		t.Fatalf("persisted state = %+v, want PhaseReconciliationConf/config_path_invalid", st)
	}
}
