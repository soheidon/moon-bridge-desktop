package publishrecovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- helpers ---

// reconcileService creates a Service whose journal was left by a fault-publish at
// the given point, with the fingerprint set to match `home`. The journal is
// repositioned to targetPhase (if non-zero) after the fault-publish.
func reconcileService(t *testing.T, faultPoint FaultPoint, targetPhase Phase, rollbackFrom *Phase) (*Service, string) {
	t.Helper()
	svc, home, _ := faultPublishAt(t, faultPoint)
	if targetPhase != "" {
		j, err := svc.store.Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		j.Phase = targetPhase
		j.RollbackFromPhase = rollbackFrom
		j.RollbackAttempted = isRollbackPhase(targetPhase)
		j.UpdatedAt = svc.stamp()
		if err := svc.store.Write(context.Background(), j); err != nil {
			t.Fatalf("reposition Write: %v", err)
		}
	}
	setJournalFingerprint(t, svc, home)
	return svc, home
}

// --- tests ---

func TestReconcileNoJournal(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeNone {
		t.Fatalf("expected none, got %s", out)
	}
}

func TestReconcilePreparedDiscard(t *testing.T) {
	svc := newTestService(t, Dependencies{Fault: faultFunc(func(p FaultPoint) error {
		if p == FaultAfterPreparedJournal {
			return errors.New("fault")
		}
		return nil
	})})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	_ = svc.Publish(context.Background(), testInput(home))
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeDiscarded {
		t.Fatalf("expected discarded, got %s", out)
	}
	// Journal and backout should be cleaned up.
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j != nil {
		t.Fatalf("journal still present: %+v", j)
	}
}

func TestReconcileCompletedTerminalCleanup(t *testing.T) {
	svc := newTestService(t, Dependencies{
		RemoveAll: func(string) error { return errors.New("cleanup fault") },
	})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	if err := svc.Publish(context.Background(), testInput(home)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Journal should be completed (cleanup failed).
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j == nil || j.Phase != PhaseCompleted {
		t.Fatalf("expected completed, got %+v", j)
	}
	// Reconcile should clean up and return completed.
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeCompleted {
		t.Fatalf("expected completed, got %s", out)
	}
}

func TestReconcileRolledBackTerminalCleanup(t *testing.T) {
	svc, home, _ := faultPublishRepositioned(t, FaultAfterAuthJournal, PhaseRolledBack, ptr(PhaseAuthPublished))
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeDiscarded {
		t.Fatalf("expected discarded, got %s", out)
	}
}

func TestReconcileDiscardedTerminalCleanup(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	// Write a valid discarded journal directly (terminal state, no backout needed for reconcile).
	j := &Journal{
		SchemaVersion:         SchemaVersion,
		TransactionID:         testTransactionID,
		Phase:                 PhaseDiscarded,
		StartedAt:             "2026-08-06T00:00:00Z",
		UpdatedAt:             "2026-08-06T00:00:00Z",
		TargetHomeFingerprint: testFingerprint(),
		ExpectedFiles:         expectedAllFiles(),
		AuthRequired:          true,
	}
	if err := svc.store.Write(context.Background(), j); err != nil {
		t.Fatalf("Write: %v", err)
	}
	home := canonicalHome(t, t.TempDir(), "codex-home")
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeDiscarded {
		t.Fatalf("expected discarded, got %s", out)
	}
}

func TestReconcileRollbackFailedRecoveryRequired(t *testing.T) {
	svc, home, _ := faultPublishRepositioned(t, FaultAfterAuthJournal, PhaseRollbackFailed, ptr(PhaseAuthPublished))
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeRecoveryRequired {
		t.Fatalf("expected recovery_required, got %s", out)
	}
}

func TestReconcileTargetHomeChangedCanonicalizeFail(t *testing.T) {
	svc, home, _ := faultPublishRepositioned(t, FaultAfterAuthJournal, PhaseAuthPublished, nil)
	setJournalFingerprint(t, svc, home)
	// Pass a non-existent home → canonicalization fails → target_home_changed
	out, err := svc.ReconcileStartup(context.Background(), filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeTargetHomeChanged {
		t.Fatalf("expected target_home_changed, got %s", out)
	}
}

func TestReconcileTargetHomeChangedFingerprintMismatch(t *testing.T) {
	svc, home := reconcileService(t, FaultAfterAuthJournal, PhaseAuthPublished, nil)
	_ = home // fingerprint is set to match home; use a different one.
	other := canonicalHome(t, t.TempDir(), "other-home")
	out, err := svc.ReconcileStartup(context.Background(), other)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeTargetHomeChanged {
		t.Fatalf("expected target_home_changed, got %s", out)
	}
}

func TestReconcileBackoutTamperedConflict(t *testing.T) {
	svc, home := reconcileService(t, FaultAfterAuthJournal, PhaseAuthPublished, nil)
	// Tamper with the backout manifest.
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	txDir := filepath.Join(svc.store.TransactionRoot(), j.TransactionID)
	manifestPath := filepath.Join(txDir, backoutManifestFileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	data[0] ^= 0x01
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeConflict {
		t.Fatalf("expected conflict, got %s", out)
	}
}

func TestReconcileBackoutUnreadableAllTargetComplete(t *testing.T) {
	svc, home := reconcileService(t, FaultAfterConfigJournal, PhaseConfigPublished, nil)
	// Remove the backout directory → ReadBackout fails (not ExternalModification).
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	txDir := filepath.Join(svc.store.TransactionRoot(), j.TransactionID)
	if err := os.RemoveAll(txDir); err != nil {
		t.Fatalf("remove backout: %v", err)
	}
	// All files at published state (TARGET for config_published expectations).
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeCompleted {
		t.Fatalf("expected completed, got %s", out)
	}
}

func TestReconcileBackoutUnreadableNotAllTargetRecoveryRequired(t *testing.T) {
	svc, home := reconcileService(t, FaultAfterAuthJournal, PhaseAuthPublished, nil)
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	txDir := filepath.Join(svc.store.TransactionRoot(), j.TransactionID)
	if err := os.RemoveAll(txDir); err != nil {
		t.Fatalf("remove backout: %v", err)
	}
	// Corrupt catalog so it's not TARGET → recovery_required.
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileModelsCatalog)), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeRecoveryRequired {
		t.Fatalf("expected recovery_required, got %s", out)
	}
}

func TestReconcileAllTargetComplete(t *testing.T) {
	// All files at published state → TARGET → complete.
	svc, home := reconcileService(t, FaultAfterConfigJournal, PhaseConfigPublished, nil)
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeCompleted {
		t.Fatalf("expected completed, got %s", out)
	}
	// Targets should still be the published values.
	assertFile(t, home, FileModelsCatalog, testCatalog)
	assertFile(t, home, FileAuth, testAuth)
	assertFile(t, home, FileConfig, testConfig)
}

func TestReconcileAllPreviousDiscard(t *testing.T) {
	// All files absent (matching the pre-publish backout state) → all PREVIOUS → discard.
	// Strategy: fault-publish on a home with pre-existing files, then remove them all,
	// so the on-disk state is "absent" = PREVIOUS (backout says they previously existed).
	svc := newTestService(t, Dependencies{Fault: faultFunc(func(p FaultPoint) error {
		if p == FaultAfterCatalogJournal {
			return errors.New("fault")
		}
		return nil
	})})
	home := canonicalHome(t, t.TempDir(), "codex-home")
	// Pre-populate with old files so the backout records them as "previously existed".
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileModelsCatalog)), []byte("old-catalog"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileAuth)), []byte("old-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), []byte("old-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = svc.Publish(context.Background(), testInput(home))
	j, err := svc.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j == nil {
		t.Fatal("journal missing")
	}
	// Reposition to catalog_published with proper published files.
	j.Phase = PhaseCatalogPublished
	j.PublishedFiles = []FileID{FileModelsCatalog}
	j.CommitMarkerPublished = false
	j.RollbackAttempted = false
	j.RollbackFromPhase = nil
	j.UpdatedAt = svc.stamp()
	if err := svc.store.Write(context.Background(), j); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Remove all target files → absent. Since backout says they previously existed,
	// absent = neither TARGET (hash match) nor PREVIOUS (previous exist) → actually OTHER.
	// Wait — for the "all PREVIOUS" case, the files must match the previous state.
	// If previous existed with old-catalog, then PREVIOUS means hash(old-current) == hash(old-backup).
	// So restore the old files instead of removing.
	// Actually: "all PREVIOUS" means the current bytes match the backout's SHA256.
	// The backout captured the old bytes. So write them back.
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileModelsCatalog)), []byte("old-catalog"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileAuth)), []byte("old-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), []byte("old-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	setJournalFingerprint(t, svc, home)
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeDiscarded {
		t.Fatalf("expected discarded, got %s", out)
	}
}

func TestReconcileMixedTargetPreviousRollback(t *testing.T) {
	// Some files TARGET, some PREVIOUS → rollback.
	svc, home, _ := faultPublishAt(t, FaultAfterAuthJournal)
	setJournalFingerprint(t, svc, home)
	// catalog=TARGET (published), auth=TARGET (published), config=PREVIOUS (absent, was absent before).
	// To get mixed state: remove auth to make it PREVIOUS (was absent before, backout says absent).
	// But auth was written by the publish, and the backout says auth was previously absent.
	// So: auth absent = PREVIOUS, catalog present with published hash = TARGET, config absent = PREVIOUS.
	// That's TARGET + PREVIOUS + PREVIOUS → hasTarget && hasPrevious → rollback.
	os.Remove(filepath.Join(home, fileNameFor(FileAuth)))
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeRolledBack {
		t.Fatalf("expected rolled_back, got %s", out)
	}
}

func TestReconcileOtherConflict(t *testing.T) {
	svc, home := reconcileService(t, FaultAfterAuthJournal, PhaseAuthPublished, nil)
	// Write garbage to catalog → OTHER (not expected hash, not previous hash).
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileModelsCatalog)), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeConflict {
		t.Fatalf("expected conflict, got %s", out)
	}
}

func TestReconcileRollbackExternalModConflict(t *testing.T) {
	// Mixed TARGET/PREVIOUS → rollback attempt → OTHER in classify → Conflict.
	svc, home, _ := faultPublishAt(t, FaultAfterAuthJournal)
	setJournalFingerprint(t, svc, home)
	// Remove auth → PREVIOUS. Write garbage to config → OTHER.
	os.Remove(filepath.Join(home, fileNameFor(FileAuth)))
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeConflict {
		t.Fatalf("expected conflict, got %s", out)
	}
}

func TestReconcileRollbackFailureRecoveryRequired(t *testing.T) {
	svc, home, _ := faultPublishAt(t, FaultAfterAuthJournal)
	setJournalFingerprint(t, svc, home)
	// Make auth PREVIOUS (remove it) so we get TARGET+PREVIOUS → rollback path.
	os.Remove(filepath.Join(home, fileNameFor(FileAuth)))
	// Inject a DurableRemove failure for the rollback's restoreFile of auth (PREVIOUS → remove).
	svc.deps.DurableRemove = func(string) error { return errors.New("restore fault") }
	svc.store.deps.DurableRemove = svc.deps.DurableRemove
	out, err := svc.ReconcileStartup(context.Background(), home)
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if out != OutcomeRecoveryRequired {
		t.Fatalf("expected recovery_required, got %s", out)
	}
}

func TestReconcileJournalLoadError(t *testing.T) {
	svc := newTestService(t, Dependencies{})
	// Write invalid JSON to journal path.
	if err := os.WriteFile(svc.store.JournalPath(), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	home := canonicalHome(t, t.TempDir(), "codex-home")
	_, err := svc.ReconcileStartup(context.Background(), home)
	if asErrorKind(err) != KindJournalParseFailed {
		t.Fatalf("expected journal_parse_failed, got %v", err)
	}
}

func TestReconcilePhasesHandledBeforeFingerprint(t *testing.T) {
	// Phases that never touch the target (prepared, completed, rolled_back,
	// discarded, rollback_failed) must be handled BEFORE canonicalization.
	// A nonexistent home should not block their cleanup.
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	for _, tc := range []struct {
		name  string
		phase Phase
		from  *Phase
		want  Outcome
	}{
		{"prepared", PhasePrepared, nil, OutcomeDiscarded},
		{"completed", PhaseCompleted, nil, OutcomeCompleted},
		{"rolled_back", PhaseRolledBack, ptr(PhaseConfigPublished), OutcomeDiscarded},
		{"discarded", PhaseDiscarded, nil, OutcomeDiscarded},
		{"rollback_failed", PhaseRollbackFailed, ptr(PhaseConfigPublished), OutcomeRecoveryRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t, Dependencies{})
			j := journalFor(tc.phase, tc.from)
			if err := svc.store.Write(context.Background(), j); err != nil {
				t.Fatalf("Write: %v", err)
			}
			out, err := svc.ReconcileStartup(context.Background(), nonexistent)
			if err != nil {
				t.Fatalf("ReconcileStartup: %v", err)
			}
			if out != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, out)
			}
		})
	}
}
