package codexlauncher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"moonbridge/internal/service/publishrecovery"
)

const testAuthToken = "sk-test-publish-token-12345678"

func writeStagedHome(t *testing.T, staging string) {
	t.Helper()
	files := map[string]string{
		"config.toml":         "model = \"deepseek-v4-pro\"\nmodel_provider = \"deepseek\"\n",
		"models_catalog.json": `{"models":[{"id":"deepseek-v4-pro"}]}`,
		"auth.json":           fmt.Sprintf(`{"openai_api_key":%q}`, testAuthToken),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeStagedHomeNoAuth writes a token-less staging set (two files: no auth.json).
func writeStagedHomeNoAuth(t *testing.T, staging string) {
	t.Helper()
	files := map[string]string{
		"config.toml":         "model = \"deepseek-v4-pro\"\nmodel_provider = \"deepseek\"\n",
		"models_catalog.json": `{"models":[{"id":"deepseek-v4-pro"}]}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func newStagingFor(t *testing.T, targetHome string) string {
	t.Helper()
	staging, err := CreateStagingHome(targetHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(staging) })
	return staging
}

func readTargetFile(t *testing.T, home, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// recoveryPublisherSvc builds a publishrecovery.Service rooted at a temp recovery
// dir, with the given dependencies (nil deps → fully defaulted production seams,
// including no fault injection). It is the test publisher for the launcher.
// It returns the Service and its recovery dir.
func recoveryPublisherSvc(t *testing.T, deps publishrecovery.Dependencies) (*publishrecovery.Service, string) {
	t.Helper()
	dir := t.TempDir()
	return recoverySvcAt(t, dir, deps), dir
}

// recoverySvcAt builds a publishrecovery.Service rooted at a specific recovery
// dir (shared across calls so two services observe the same journal slot).
func recoverySvcAt(t *testing.T, dir string, deps publishrecovery.Dependencies) *publishrecovery.Service {
	t.Helper()
	svc, err := publishrecovery.New(publishrecovery.ServiceOptions{
		RecoveryDir:  dir,
		Dependencies: deps,
	})
	if err != nil {
		t.Fatalf("publishrecovery.New: %v", err)
	}
	return svc
}

// assertNoResidualTransaction checks that a completed publish left no backout
// transaction under the given recovery root (the journal itself is deleted by a
// successful publish; a residual transaction dir signals a cleanup breach).
func assertNoResidualTransaction(t *testing.T, recDir string) {
	t.Helper()
	txRoot := filepath.Join(recDir, "publish-transactions")
	entries, err := os.ReadDir(txRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("residual publish transactions left: %v", names)
	}
}

func assertJournalGone(t *testing.T, recDir string) {
	t.Helper()
	journalPath := filepath.Join(recDir, "codex-home-publish-journal.json")
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("journal still present after completed publish: %v", err)
	}
}

func TestPublishFirstRunCreatesThreeFiles(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)
	svc, recDir := recoveryPublisherSvc(t, publishrecovery.Dependencies{})

	if err := publishStaged(context.Background(), svc, staging, targetHome, true); err != nil {
		t.Fatalf("publishStaged failed: %v", err)
	}
	for _, name := range codexHomeFiles {
		if _, err := os.Stat(filepath.Join(targetHome, name)); err != nil {
			t.Fatalf("published file missing: %s", name)
		}
	}
	if got := readTargetFile(t, targetHome, "auth.json"); !strings.Contains(got, testAuthToken) {
		t.Fatal("auth.json did not carry the token")
	}
	assertJournalGone(t, recDir)
	assertNoResidualTransaction(t, recDir)
}

func TestPublishReplacesExistingFiles(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range codexHomeFiles {
		if err := os.WriteFile(filepath.Join(targetHome, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)
	svc, recDir := recoveryPublisherSvc(t, publishrecovery.Dependencies{})

	if err := publishStaged(context.Background(), svc, staging, targetHome, true); err != nil {
		t.Fatalf("publishStaged failed: %v", err)
	}
	if got := readTargetFile(t, targetHome, "config.toml"); !strings.Contains(got, "deepseek-v4-pro") {
		t.Fatalf("config.toml not replaced: %q", got)
	}
	assertJournalGone(t, recDir)
	assertNoResidualTransaction(t, recDir)
}

func TestPublishTokenlessUsesTwoFileSet(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging)
	svc, recDir := recoveryPublisherSvc(t, publishrecovery.Dependencies{})

	if err := publishStaged(context.Background(), svc, staging, targetHome, false); err != nil {
		t.Fatalf("publishStaged(tokenless) failed: %v", err)
	}
	for _, name := range []string{"config.toml", "models_catalog.json"} {
		if _, err := os.Stat(filepath.Join(targetHome, name)); err != nil {
			t.Fatalf("published file missing: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(targetHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth.json must be absent on a token-less publish, got err=%v", err)
	}
	assertJournalGone(t, recDir)
	assertNoResidualTransaction(t, recDir)
}

func TestPublishTokenlessRemovesStaleAuthJSON(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	oldAuth := `{"openai_api_key":"sk-stale-token-1234567890"}`
	if err := os.WriteFile(filepath.Join(targetHome, "auth.json"), []byte(oldAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging)
	svc, recDir := recoveryPublisherSvc(t, publishrecovery.Dependencies{})

	if err := publishStaged(context.Background(), svc, staging, targetHome, false); err != nil {
		t.Fatalf("publishStaged(tokenless, stale auth) failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("stale auth.json must be removed on a token-less publish, err=%v", err)
	}
	assertJournalGone(t, recDir)
	assertNoResidualTransaction(t, recDir)
}

// A token-less publish that removes a stale auth.json must, on an ordinary
// in-process I/O failure, roll its state back: the stale auth.json is restored
// from the backout and any half-written catalog/config revert to their previous
// (absent) state. This mirrors the Step 3D contract through the launcher path.
func TestPublishTokenlessStaleAuthRollbackOnMidPublishFailure(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	oldAuth := `{"openai_api_key":"sk-stale-token-1234567890"}`
	if err := os.WriteFile(filepath.Join(targetHome, "auth.json"), []byte(oldAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging)

	// DurableRemove fails only on the publish-time stale-auth removal; the rollback
	// restore path uses a working DurableRemove so the rollback completes.
	var calls int
	svc, recDir := recoveryPublisherSvc(t, publishrecovery.Dependencies{
		DurableRemove: func(path string) error {
			calls++
			if calls == 1 {
				return errors.New("durable remove fault")
			}
			// Real removal for rollback restores (confirms the file is gone).
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		},
	})

	err := publishStaged(context.Background(), svc, staging, targetHome, false)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	// Safe-side policy: non-rollback_failed kind → rolledBack omitted (the error
	// alone does not prove rollback ran; this case did roll back successfully, but
	// the safe-side contract omits the field for ambiguous kinds).
	if _, ok := le.Details["rolledBack"]; ok {
		t.Fatalf("rolledBack must be omitted on non-rollback_failed kinds, got %v", le.Details["rolledBack"])
	}
	// The stale auth.json was restored; catalog/config reverted to absent.
	if got := readTargetFile(t, targetHome, "auth.json"); got != oldAuth {
		t.Fatalf("stale auth.json not restored on rollback: %q", got)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "models_catalog.json")); !os.IsNotExist(err) {
		t.Fatalf("half-written catalog left after rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("half-written config left after rollback: %v", err)
	}
	assertJournalGone(t, recDir)
	assertNoResidualTransaction(t, recDir)
}

// testHomePublisher is a homePublisher test double that records inputs and lets
// tests force a publishrecovery-style failure.
type testHomePublisher struct {
	inputs []publishrecovery.PublishInput
	err    error
}

func (p *testHomePublisher) Publish(ctx context.Context, in publishrecovery.PublishInput) error {
	p.inputs = append(p.inputs, in)
	return p.err
}

func TestPublishBuildsInputAndForwardsToPublisher(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)
	pub := &testHomePublisher{}

	if err := publishStaged(context.Background(), pub, staging, targetHome, true); err != nil {
		t.Fatalf("publishStaged: %v", err)
	}
	if len(pub.inputs) != 1 {
		t.Fatalf("expected 1 Publish input, got %d", len(pub.inputs))
	}
	in := pub.inputs[0]
	if in.TargetHome != targetHome {
		t.Fatalf("TargetHome = %q, want %q", in.TargetHome, targetHome)
	}
	if !in.AuthRequired {
		t.Fatal("expected AuthRequired=true")
	}
	if string(in.AuthJSON) != fmt.Sprintf(`{"openai_api_key":%q}`, testAuthToken) {
		t.Fatalf("AuthJSON not forwarded: %q", in.AuthJSON)
	}
	if !strings.Contains(string(in.ConfigTOML), "deepseek-v4-pro") {
		t.Fatalf("ConfigTOML not forwarded: %q", in.ConfigTOML)
	}
}

func TestPublishFailsWhenStagedMissingFile(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	pub := &testHomePublisher{}

	err := publishStaged(context.Background(), pub, staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	if strings.Contains(le.Error(), testAuthToken) {
		t.Fatal("publish error leaked the token")
	}
	// Since the input could not be assembled, no publish was attempted.
	if len(pub.inputs) != 0 {
		t.Fatalf("publisher was called with a missing staging file")
	}
}

func TestPublishDoesNotInferRollbackSuccess(t *testing.T) {
	// A publishrecovery failure that is NOT rollback_failed means the immediate
	// rollback completed (rollbackAndReturn returns the original publish cause).
	// Since the error kind alone does not unambiguously prove rollback ran,
	// rolledBack is omitted (safe-side policy).
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging)
	pub := &testHomePublisher{err: &publishrecovery.Error{Kind: publishrecovery.KindBackoutFailed, Message: "write catalog failed"}}

	err := publishStaged(context.Background(), pub, staging, targetHome, false)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	// Safe-side policy: rolledBack is only set for KindRollbackFailed (ambiguous
	// on any other kind — the error alone does not prove a rollback ran).
	if _, ok := le.Details["rolledBack"]; ok {
		t.Fatalf("rolledBack must be omitted on non-rollback_failed kinds, got %v", le.Details["rolledBack"])
	}
	if le.Details["cause"] != string(publishrecovery.KindBackoutFailed) {
		t.Fatalf("expected cause kind, got %v", le.Details)
	}
	if strings.Contains(fmt.Sprint(le.Details), testAuthToken) || strings.Contains(le.Error(), testAuthToken) {
		t.Fatal("error leaked the token")
	}
}

func TestPublishMapsRollbackFailureToRolledBackFalse(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)
	pub := &testHomePublisher{err: &publishrecovery.Error{Kind: publishrecovery.KindRollbackFailed, Message: "rollback did not complete"}}

	err := publishStaged(context.Background(), pub, staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	if le.Details["rolledBack"] != false {
		t.Fatalf("expected rolledBack=false, got %v", le.Details)
	}
	if _, ok := le.Details["rollbackError"]; !ok {
		t.Fatalf("expected rollbackError detail: %v", le.Details)
	}
	// rollbackError must be a sanitized literal, never the raw error string.
	if strings.Contains(fmt.Sprint(le.Details["rollbackError"]), "rollback did not complete original") {
		t.Fatalf("rollbackError leaks the raw error: %v", le.Details["rollbackError"])
	}
	if strings.Contains(le.Error(), testAuthToken) {
		t.Fatal("error leaked the token")
	}
}

func TestGenerateAndVerifyRejectsInvalidConfig(t *testing.T) {
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "config.toml"), []byte("model = \"unterminated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "models_catalog.json"), []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "auth.json"), []byte(`{"openai_api_key":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAndVerify(staging, func(string) error { return nil }, true); err == nil {
		t.Fatal("expected invalid config to be rejected")
	}
}

func TestGenerateAndVerifyRejectsMissingAuthKey(t *testing.T) {
	staging := t.TempDir()
	files := map[string]string{
		"config.toml":         "model = \"a\"\n",
		"models_catalog.json": `{"models":[]}`,
		"auth.json":           `{"foo":"bar"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := GenerateAndVerify(staging, func(string) error { return nil }, true); err == nil {
		t.Fatal("expected missing openai_api_key to be rejected")
	}
}

func TestVerifyHomeRejectsPermissiveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX permission bits")
	}
	staging := t.TempDir()
	writeStagedHome(t, staging)
	if err := os.Chmod(filepath.Join(staging, "config.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyHome(staging, true); err == nil {
		t.Fatal("expected permissive mode to be rejected")
	}
	if err := os.Chmod(filepath.Join(staging, "config.toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyHome(staging, true); err != nil {
		t.Fatalf("0600 mode should verify: %v", err)
	}
}

// testFaultInjector errors once at a named publishrecovery fault point.
type testFaultInjector struct {
	point publishrecovery.FaultPoint
	fired bool
}

func (f *testFaultInjector) Hit(p publishrecovery.FaultPoint) error {
	if p == f.point && !f.fired {
		f.fired = true
		return errors.New("fault injected")
	}
	return nil
}

// A crash (fault seam) mid-publish leaves a durable journal and backout that
// startup reconciliation resolves — the launcher never rolls back in-process and
// never falls back to the old transient-backout publish.
func TestPublishFaultLeavesJournalForReconcile(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)
	fault := &testFaultInjector{point: publishrecovery.FaultAfterCatalogWrite}
	svc, recDir := recoveryPublisherSvc(t, publishrecovery.Dependencies{Fault: fault})

	err := publishStaged(context.Background(), svc, staging, targetHome, true)
	if err == nil {
		t.Fatal("expected the fault to abort the publish")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	// A durable journal + backout remain for startup reconciliation.
	journalPath := filepath.Join(recDir, "codex-home-publish-journal.json")
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("expected a residual journal after a publish fault: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recDir, "publish-transactions")); err != nil {
		t.Fatalf("expected a backout transaction after a publish fault: %v", err)
	}
	// Startup reconciliation resolves the journal. The fault left catalog written
	// but not journalled (mixed TARGET/PREVIOUS), so reconcile rolls back the half
	// publish rather than completing it.
	outcome, oerr := svc.ReconcileStartup(context.Background(), targetHome)
	if oerr != nil {
		t.Fatalf("ReconcileStartup: %v", oerr)
	}
	if outcome != publishrecovery.OutcomeRolledBack {
		t.Fatalf("expected rolled_back after reconcile, got %s", outcome)
	}
	// The half-written catalog is restored to its pre-publish (absent) state.
	if _, err := os.Stat(filepath.Join(targetHome, "models_catalog.json")); !os.IsNotExist(err) {
		t.Fatalf("reconciled rollback left a half-written catalog: %v", err)
	}
	assertJournalGone(t, recDir)
	assertNoResidualTransaction(t, recDir)
}

// An unfinished journal blocks a fresh publish (never a fallback to the old
// non-journal path), and the target is left untouched.
func TestPublishRejectedByUnfinishedJournal(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	recDir := t.TempDir()
	fault := &testFaultInjector{point: publishrecovery.FaultAfterCatalogWrite}

	// First service leaves an unfinished journal + backout (a prior crash).
	first := recoverySvcAt(t, recDir, publishrecovery.Dependencies{Fault: fault})
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)
	if err := publishStaged(context.Background(), first, staging, targetHome, true); err == nil {
		t.Fatal("expected the fault to leave an unfinished journal")
	}
	// Capture the target state as the crash left it.
	catalogPublished, _ := os.ReadFile(filepath.Join(targetHome, "models_catalog.json"))

	// A fresh publish through a second service (same recovery dir) must be
	// rejected as transaction_active, and the target must not further change.
	second := recoverySvcAt(t, recDir, publishrecovery.Dependencies{})
	staging2 := newStagingFor(t, targetHome)
	writeStagedHome(t, staging2)
	err := publishStaged(context.Background(), second, staging2, targetHome, true)
	if err == nil {
		t.Fatal("expected a fresh publish to be rejected by the unfinished journal")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	// Safe-side policy: transaction_active means no rollback was attempted, so
	// rolledBack must be absent.
	if _, ok := le.Details["rolledBack"]; ok {
		t.Fatalf("rolledBack must be omitted on transaction_active, got %v", le.Details["rolledBack"])
	}
	// The target is unchanged from the crash state (no auth/config written, and
	// the fresh publish never touched it).
	if cur, _ := os.ReadFile(filepath.Join(targetHome, "models_catalog.json")); string(cur) != string(catalogPublished) {
		t.Fatalf("target catalog changed across the rejected publish")
	}
	if _, err := os.Stat(filepath.Join(targetHome, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("config.toml unexpectedly present after a rejected publish: %v", err)
	}
}

// A publishrecovery init failure (a Service that cannot be built, e.g. a
// relative recovery root) surfaces as a publish failure — the launcher must
// never fall back to the old non-journal publish.
func TestPublishInputBuildSurfacesNewInitFailure(t *testing.T) {
	// Verify the failure path that an init failure maps through: publishStaged
	// surfaces any publisher error as KindConfigPublishFailed without leaking it.
	pub := &testHomePublisher{err: errors.New("resolve recovery root failed: unavailable")}
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)
	err := publishStaged(context.Background(), pub, staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	if strings.Contains(le.Error(), "resolve recovery root failed") {
		t.Fatalf("raw init error leaked into the publish error: %v", le.Error())
	}
	// Safe-side policy: init failure means no rollback was attempted, so
	// rolledBack must be absent.
	if _, ok := le.Details["rolledBack"]; ok {
		t.Fatalf("rolledBack must be omitted on init failure, got %v", le.Details["rolledBack"])
	}
}
