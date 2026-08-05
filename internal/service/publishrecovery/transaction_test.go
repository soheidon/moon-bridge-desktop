package publishrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	testCatalog = []byte(`{"models":[]}`)
	testAuth    = []byte(`{"tokens":["sk-secret"]}`)
	testConfig  = []byte("openai_base_url=\"http://127.0.0.1:38441/v1\"\n")
)

func testInput(home string) PublishInput {
	return PublishInput{
		TargetHome:    home,
		ModelsCatalog: testCatalog,
		AuthRequired:  true,
		AuthJSON:      testAuth,
		ConfigTOML:    testConfig,
	}
}

// newTestService builds a Service backed by a fresh temp recovery dir, with
// deterministic time and a unique transactionID per Publish call.
func newTestService(t *testing.T, deps Dependencies) *Service {
	t.Helper()
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	}
	if deps.NewID == nil {
		n := 0
		deps.NewID = func() string {
			n++
			return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
		}
	}
	svc, err := New(ServiceOptions{RecoveryDir: t.TempDir(), Dependencies: deps})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func assertFile(t *testing.T, home string, id FileID, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(home, fileNameFor(id)))
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s: got %q want %q", id, got, want)
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still present: %v", path, err)
	}
}

func TestPublishCompletesThreeFiles(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	if err := svc.Publish(context.Background(), testInput(home)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	assertFile(t, home, FileModelsCatalog, testCatalog)
	assertFile(t, home, FileAuth, testAuth)
	assertFile(t, home, FileConfig, testConfig)
	// Journal and backout are cleaned up.
	if j, err := svc.store.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	} else if j != nil {
		t.Fatalf("journal still present after completed publish: %+v", j)
	}
	entries, err := os.ReadDir(svc.store.TransactionRoot())
	if err == nil && len(entries) != 0 {
		t.Fatalf("backout transactions left after completed publish: %v", entries)
	}
}

func TestPublishAuthRequiredFalseRemovesStaleAuth(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	stale := filepath.Join(home, fileNameFor(FileAuth))
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := testInput(home)
	in.AuthRequired = false
	in.AuthJSON = nil
	if err := svc.Publish(context.Background(), in); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	assertGone(t, stale)
	assertFile(t, home, FileModelsCatalog, testCatalog)
	assertFile(t, home, FileConfig, testConfig)
}

func TestPublishAuthRequiredFalseFreshHome(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	in := testInput(home)
	in.AuthRequired = false
	in.AuthJSON = nil
	if err := svc.Publish(context.Background(), in); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	assertGone(t, filepath.Join(home, fileNameFor(FileAuth)))
	assertFile(t, home, FileConfig, testConfig)
}

func TestPublishTwiceSequential(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	for i := 0; i < 2; i++ {
		if err := svc.Publish(context.Background(), testInput(home)); err != nil {
			t.Fatalf("publish %d: %v", i+1, err)
		}
	}
	assertFile(t, home, FileConfig, testConfig)
}

func TestPublishRejectsInvalidTargetHome(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	for _, home := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		if err := svc.Publish(context.Background(), testInput(home)); asErrorKind(err) != KindConfigPathInvalid {
			t.Fatalf("target home %q: expected config_path_invalid, got %v", home, err)
		}
	}
}

func TestPublishRejectsUnfinishedTransaction(t *testing.T) {
	svc := newTestService(t, Dependencies{Fault: faultFunc(func(p FaultPoint) error {
		if p == FaultAfterCatalogWrite {
			return errors.New("fault")
		}
		return nil
	})})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	if err := svc.Publish(context.Background(), testInput(home)); err == nil {
		t.Fatalf("expected fault")
	}
	if err := svc.Publish(context.Background(), testInput(home)); asErrorKind(err) != KindTransactionActive {
		t.Fatalf("expected transaction_active, got %v", err)
	}
}

func TestPublishCleanupFailureStillSucceeds(t *testing.T) {
	svc := newTestService(t, Dependencies{
		RemoveAll: func(string) error { return errors.New("cleanup fault") },
	})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	if err := svc.Publish(context.Background(), testInput(home)); err != nil {
		t.Fatalf("Publish failed despite only cleanup failing: %v", err)
	}
	assertFile(t, home, FileModelsCatalog, testCatalog)
	assertFile(t, home, FileAuth, testAuth)
	assertFile(t, home, FileConfig, testConfig)
	// The completed journal is kept so the next startup retries the cleanup.
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j == nil {
		t.Fatalf("expected a journal after failed cleanup")
	}
	if j.Phase != PhaseCompleted {
		t.Fatalf("phase = %s, want completed", j.Phase)
	}
	if j.CompletedAt == nil || *j.CompletedAt == "" {
		t.Fatalf("completed journal missing completedAt")
	}
}

func TestPublishFaultLeavesDurableJournalState(t *testing.T) {
	cases := []struct {
		name        string
		point       FaultPoint
		phase       Phase
		published   []FileID
		marker      bool
		wantCatalog bool
		wantAuth    bool
		wantConfig  bool
	}{
		{name: "after prepared journal", point: FaultAfterPreparedJournal, phase: PhasePrepared, published: nil, marker: false},
		{name: "after backout copy", point: FaultAfterBackoutCopy, phase: PhasePrepared, published: nil, marker: false},
		{name: "after catalog write", point: FaultAfterCatalogWrite, phase: PhaseBackoutCopied, published: nil, marker: false, wantCatalog: true},
		{name: "after catalog journal", point: FaultAfterCatalogJournal, phase: PhaseCatalogPublished, published: []FileID{FileModelsCatalog}, marker: false, wantCatalog: true},
		{name: "after auth write", point: FaultAfterAuthWrite, phase: PhaseCatalogPublished, published: []FileID{FileModelsCatalog}, marker: false, wantCatalog: true, wantAuth: true},
		{name: "after auth journal", point: FaultAfterAuthJournal, phase: PhaseAuthPublished, published: []FileID{FileModelsCatalog, FileAuth}, marker: false, wantCatalog: true, wantAuth: true},
		{name: "after config write", point: FaultAfterConfigWrite, phase: PhaseAuthPublished, published: []FileID{FileModelsCatalog, FileAuth}, marker: false, wantCatalog: true, wantAuth: true, wantConfig: true},
		{name: "after config journal", point: FaultAfterConfigJournal, phase: PhaseConfigPublished, published: []FileID{FileModelsCatalog, FileAuth, FileConfig}, marker: true, wantCatalog: true, wantAuth: true, wantConfig: true},
		{name: "after verified", point: FaultAfterVerified, phase: PhaseVerified, published: []FileID{FileModelsCatalog, FileAuth, FileConfig}, marker: true, wantCatalog: true, wantAuth: true, wantConfig: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t, Dependencies{Fault: faultFunc(func(p FaultPoint) error {
				if p == tc.point {
					return errors.New("fault injected")
				}
				return nil
			})})
			home := canonicalHome(t, t.TempDir(), "codex-home")
			if err := svc.Publish(context.Background(), testInput(home)); err == nil {
				t.Fatalf("expected fault at %s", tc.point)
			}
			j, err := svc.store.Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if j == nil {
				t.Fatalf("journal missing after fault at %s", tc.point)
			}
			if j.Phase != tc.phase {
				t.Fatalf("phase = %s, want %s", j.Phase, tc.phase)
			}
			if !fileIDsEqual(j.PublishedFiles, tc.published) {
				t.Fatalf("publishedFiles = %v, want %v", j.PublishedFiles, tc.published)
			}
			if j.CommitMarkerPublished != tc.marker {
				t.Fatalf("commitMarkerPublished = %v, want %v", j.CommitMarkerPublished, tc.marker)
			}
			if j.RollbackAttempted {
				t.Fatalf("forward phase must not set rollbackAttempted")
			}
			// The on-disk target state matches the durable phase: only the
			// mutations whose journal advance has completed are visible.
			if tc.wantCatalog {
				assertFile(t, home, FileModelsCatalog, testCatalog)
			} else {
				assertGone(t, filepath.Join(home, fileNameFor(FileModelsCatalog)))
			}
			if tc.wantAuth {
				assertFile(t, home, FileAuth, testAuth)
			} else {
				assertGone(t, filepath.Join(home, fileNameFor(FileAuth)))
			}
			if tc.wantConfig {
				assertFile(t, home, FileConfig, testConfig)
			} else {
				assertGone(t, filepath.Join(home, fileNameFor(FileConfig)))
			}
		})
	}
}

func TestPublishJournalNeverContainsSecrets(t *testing.T) {
	svc := newTestService(t, Dependencies{Fault: faultFunc(func(p FaultPoint) error {
		if p == FaultAfterConfigJournal {
			return errors.New("fault")
		}
		return nil
	})})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	if err := svc.Publish(context.Background(), testInput(home)); err == nil {
		t.Fatalf("expected fault")
	}
	data, err := os.ReadFile(svc.store.JournalPath())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if strings.Contains(string(data), "sk-secret") {
		t.Fatalf("journal leaks the auth secret")
	}
	if strings.Contains(string(data), home) {
		t.Fatalf("journal leaks the target home path")
	}
}

func TestVerifyTargetFile(t *testing.T) {
	home := canonicalHome(t, t.TempDir(), "codex-home")
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), testConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := verifyTargetFile(home, ExpectedFile{File: FileConfig, ExpectedExist: true, SHA256: sha256Hex(testConfig)})
	if err != nil || !ok {
		t.Fatalf("expected match: ok=%v err=%v", ok, err)
	}
	ok, err = verifyTargetFile(home, ExpectedFile{File: FileConfig, ExpectedExist: true, SHA256: sha256Hex([]byte("other"))})
	if err != nil || ok {
		t.Fatalf("expected mismatch: ok=%v err=%v", ok, err)
	}
	ok, err = verifyTargetFile(home, ExpectedFile{File: FileAuth, ExpectedExist: false})
	if err != nil || !ok {
		t.Fatalf("expected absent-ok: ok=%v err=%v", ok, err)
	}
	ok, err = verifyTargetFile(home, ExpectedFile{File: FileAuth, ExpectedExist: true, SHA256: sha256Hex(testAuth)})
	if err != nil || ok {
		t.Fatalf("expected absent-mismatch: ok=%v err=%v", ok, err)
	}
}

func TestPublishAuthRemovalDurableSeamFailure(t *testing.T) {
	svc := newTestService(t, Dependencies{
		DurableRemove: func(string) error { return errors.New("durable remove fault") },
	})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	stale := filepath.Join(home, fileNameFor(FileAuth))
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := testInput(home)
	in.AuthRequired = false
	in.AuthJSON = nil
	if err := svc.Publish(context.Background(), in); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	// The durable removal seam failed, so the auth_published advance never ran:
	// the journal stays at catalog_published and the stale auth.json is untouched.
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j == nil || j.Phase != PhaseCatalogPublished {
		t.Fatalf("expected journal at catalog_published, got %+v", j)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale auth.json was removed despite the durable remove fault")
	}
}

func TestPublishRejectsTargetHomeChangeBeforeMutation(t *testing.T) {
	// A home swapped right after each mutation-adjacent journal write must be
	// caught by the next pre-mutation re-check (KindTargetHomeChanged), with
	// nothing further written and the journal/backout retained at the reached
	// phase.
	cases := []struct {
		name   string
		swapAt FaultPoint
		phase  Phase
	}{
		{name: "before backout", swapAt: FaultAfterPreparedJournal, phase: PhasePrepared},
		{name: "before catalog write", swapAt: FaultAfterBackoutCopy, phase: PhaseBackoutCopied},
		{name: "before auth write", swapAt: FaultAfterCatalogJournal, phase: PhaseCatalogPublished},
		{name: "before config write", swapAt: FaultAfterAuthJournal, phase: PhaseAuthPublished},
		{name: "before final verify", swapAt: FaultAfterConfigJournal, phase: PhaseConfigPublished},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := canonicalHome(t, t.TempDir(), "codex-home")
			svc := newTestService(t, Dependencies{Fault: faultFunc(func(p FaultPoint) error {
				if p == tc.swapAt {
					// Replace the home with a regular file so re-canonicalization
					// fails: the next pre-mutation re-check must abort.
					if err := os.RemoveAll(home); err != nil {
						t.Errorf("swap home (remove): %v", err)
					}
					if err := os.WriteFile(home, []byte("not a directory"), 0o600); err != nil {
						t.Errorf("swap home (write): %v", err)
					}
				}
				return nil
			})})
			if err := svc.Publish(context.Background(), testInput(home)); asErrorKind(err) != KindTargetHomeChanged {
				t.Fatalf("expected target_home_changed, got %v", err)
			}
			j, err := svc.store.Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if j == nil || j.Phase != tc.phase {
				t.Fatalf("expected journal at %s, got %+v", tc.phase, j)
			}
		})
	}
}

func TestPublishRejectsWhenStaleBackoutCleanupFails(t *testing.T) {
	svc := newTestService(t, Dependencies{
		RemoveAll: func(string) error { return errors.New("cleanup fault") },
	})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	// The first publish cannot remove its backout, so it leaves a completed
	// journal plus the stale transaction directory behind.
	if err := svc.Publish(context.Background(), testInput(home)); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j == nil || j.Phase != PhaseCompleted {
		t.Fatalf("expected a completed journal after failed cleanup, got %+v", j)
	}
	// The uncleaned backout blocks the next publish; no new prepared journal is
	// written and the old completed journal survives.
	if err := svc.Publish(context.Background(), testInput(home)); asErrorKind(err) != KindTransactionActive {
		t.Fatalf("expected transaction_active, got %v", err)
	}
	after, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after == nil || after.Phase != PhaseCompleted {
		t.Fatalf("old completed journal not preserved: %+v", after)
	}
	if _, err := os.Stat(filepath.Join(svc.store.TransactionRoot(), after.TransactionID)); err != nil {
		t.Fatalf("stale backout directory was removed: %v", err)
	}
}

func TestPublishRejectsWhenStaleJournalDeleteFails(t *testing.T) {
	svc := newTestService(t, Dependencies{
		Remove: func(string) error { return errors.New("remove fault") },
	})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	// The first publish completes and removes its backout, but the best-effort
	// journal delete at the end is faulted, so a completed journal stays.
	if err := svc.Publish(context.Background(), testInput(home)); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// The stale journal blocks the next publish: the slot is only released when
	// the journal delete succeeds, so no new prepared journal is written.
	if err := svc.Publish(context.Background(), testInput(home)); asErrorKind(err) != KindTransactionActive {
		t.Fatalf("expected transaction_active, got %v", err)
	}
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j == nil || j.Phase != PhaseCompleted {
		t.Fatalf("old completed journal not preserved: %+v", j)
	}
}

func TestPublishZeroValueDependenciesAuthRequiredFalse(t *testing.T) {
	// A fully zero-value Dependencies is normalized by NewStore, so the Service
	// never holds a nil seam. This is the production configuration, and a nil
	// DurableRemove here would panic in applyAuth when removing the stale auth.
	svc, err := New(ServiceOptions{RecoveryDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	home := canonicalHome(t, t.TempDir(), "codex-home")
	stale := filepath.Join(home, fileNameFor(FileAuth))
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := testInput(home)
	in.AuthRequired = false
	in.AuthJSON = nil
	if err := svc.Publish(context.Background(), in); err != nil {
		t.Fatalf("Publish with zero-value Dependencies: %v", err)
	}
	assertGone(t, stale)
	assertFile(t, home, FileModelsCatalog, testCatalog)
	assertFile(t, home, FileConfig, testConfig)
}

func TestNewServiceStoreShareNormalizedDependencies(t *testing.T) {
	svc, err := New(ServiceOptions{RecoveryDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svcDeps, storeDeps := svc.deps, svc.store.deps
	if svcDeps.AtomicWrite == nil || svcDeps.Remove == nil || svcDeps.RemoveAll == nil ||
		svcDeps.DurableRemove == nil || svcDeps.Now == nil || svcDeps.NewID == nil || svcDeps.Fault == nil {
		t.Fatalf("Service holds a nil seam after New with zero-value Dependencies: %+v", svcDeps)
	}
	// Service and Store reference the same function instances: Service keeps no
	// raw copy of opts.Dependencies, so a nil guard on one side is the guarantee
	// on both.
	if reflect.ValueOf(svcDeps.AtomicWrite).Pointer() != reflect.ValueOf(storeDeps.AtomicWrite).Pointer() {
		t.Fatal("Service and Store use different AtomicWrite instances")
	}
	if reflect.ValueOf(svcDeps.Remove).Pointer() != reflect.ValueOf(storeDeps.Remove).Pointer() {
		t.Fatal("Service and Store use different Remove instances")
	}
	if reflect.ValueOf(svcDeps.RemoveAll).Pointer() != reflect.ValueOf(storeDeps.RemoveAll).Pointer() {
		t.Fatal("Service and Store use different RemoveAll instances")
	}
	if reflect.ValueOf(svcDeps.DurableRemove).Pointer() != reflect.ValueOf(storeDeps.DurableRemove).Pointer() {
		t.Fatal("Service and Store use different DurableRemove instances")
	}
	if reflect.ValueOf(svcDeps.Now).Pointer() != reflect.ValueOf(storeDeps.Now).Pointer() {
		t.Fatal("Service and Store use different Now instances")
	}
	if reflect.ValueOf(svcDeps.NewID).Pointer() != reflect.ValueOf(storeDeps.NewID).Pointer() {
		t.Fatal("Service and Store use different NewID instances")
	}
	if !reflect.DeepEqual(svcDeps.Fault, storeDeps.Fault) {
		t.Fatal("Service and Store use different Fault instances")
	}
}

func TestPublishAuthRemovalParentSyncFailure(t *testing.T) {
	// Partial success on the DurableRemove seam: os.Remove succeeds (auth.json is
	// already gone on disk) but the parent-directory sync fails afterwards.
	// Publish must error and the journal must stay at catalog_published — this is
	// the W8 crash-window state that Step 3D reconciles from.
	svc := newTestService(t, Dependencies{
		DurableRemove: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return errors.New("simulated parent sync failure")
		},
	})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	stale := filepath.Join(home, fileNameFor(FileAuth))
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := testInput(home)
	in.AuthRequired = false
	in.AuthJSON = nil
	if err := svc.Publish(context.Background(), in); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	// auth.json is gone (os.Remove succeeded); the journal never advanced to
	// auth_published, so the durable record still says catalog_published with
	// only the catalog published and config.toml never written.
	assertGone(t, stale)
	assertFile(t, home, FileModelsCatalog, testCatalog)
	assertGone(t, filepath.Join(home, fileNameFor(FileConfig)))
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j == nil || j.Phase != PhaseCatalogPublished {
		t.Fatalf("expected journal at catalog_published, got %+v", j)
	}
	if !fileIDsEqual(j.PublishedFiles, []FileID{FileModelsCatalog}) {
		t.Fatalf("publishedFiles = %v, want [models_catalog]", j.PublishedFiles)
	}
	// The backout transaction is retained so Step 3D can restore from it.
	if _, err := os.Stat(filepath.Join(svc.store.TransactionRoot(), j.TransactionID)); err != nil {
		t.Fatalf("backout not retained: %v", err)
	}
}

func TestDeleteBackoutRefusesJunctionAtTxDir(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	completed := "2000-01-02T03:04:05Z"
	j := &Journal{
		SchemaVersion:         SchemaVersion,
		TransactionID:         testTransactionID,
		Phase:                 PhaseCompleted,
		StartedAt:             completed,
		UpdatedAt:             completed,
		CompletedAt:           &completed,
		TargetHomeFingerprint: strings.Repeat("a", 64),
		ExpectedFiles: []ExpectedFile{
			{File: FileModelsCatalog, ExpectedExist: true, SHA256: strings.Repeat("b", 64)},
			{File: FileAuth, ExpectedExist: false},
			{File: FileConfig, ExpectedExist: true, SHA256: strings.Repeat("c", 64)},
		},
		PublishedFiles:        []FileID{FileModelsCatalog, FileAuth, FileConfig},
		AuthRequired:          false,
		CommitMarkerPublished: true,
		BackoutManifestSHA256: strings.Repeat("d", 64),
	}
	if err := s.Write(context.Background(), j); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	// The transaction root must exist so a junction can be planted at the
	// transaction directory path.
	if err := os.MkdirAll(s.TransactionRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "victim.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	txDir := filepath.Join(s.TransactionRoot(), testTransactionID)
	if err := linkDir(txDir, target); err != nil {
		t.Skipf("junction creation not supported: %v", err)
	}
	if err := s.DeleteBackout(context.Background(), testTransactionID); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	// Nothing was removed through the link, and the link itself was not removed.
	if _, err := os.Stat(filepath.Join(target, "victim.txt")); err != nil {
		t.Fatalf("victim file removed through junction: %v", err)
	}
	if _, err := os.Lstat(txDir); err != nil {
		t.Fatalf("junction at txDir was removed: %v", err)
	}
}
