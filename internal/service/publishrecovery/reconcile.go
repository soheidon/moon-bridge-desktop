package publishrecovery

import (
	"context"

	"moonbridge/internal/service/recovery"
)

// Outcome is the result of startup reconciliation (§12).
type Outcome string

const (
	OutcomeNone              Outcome = "none"
	OutcomeDiscarded         Outcome = "discarded"
	OutcomeCompleted         Outcome = "completed"
	OutcomeRolledBack        Outcome = "rolled_back"
	OutcomeRecoveryRequired  Outcome = "recovery_required"
	OutcomeTargetHomeChanged Outcome = "target_home_changed"
	OutcomeConflict          Outcome = "conflict"
)

// ReconcileStartup resolves a journal left by a crash or a failed publish
// (§12 decision table, R8-1). The journal stores only a fingerprint, never the
// target home path, so the caller passes the home it resolved for this startup
// as an argument.
//
// Phases that never touch the target (prepared / completed / rolled_back /
// discarded / rollback_failed) are handled before the home is canonicalized, so
// a vanished or changed home cannot wedge their cleanup. Only target-accessing
// phases canonicalize the home and verify the fingerprint: nothing inside the
// target home is read or modified before the fingerprint matches (R8-1).
func (s *Service) ReconcileStartup(ctx context.Context, targetHome string) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, err := s.store.Load(ctx)
	if err != nil {
		return OutcomeNone, err
	}
	if j == nil {
		return OutcomeNone, nil
	}

	switch j.Phase {
	case PhaseRollbackFailed:
		return OutcomeRecoveryRequired, nil
	case PhaseCompleted:
		_ = s.terminalCleanup(ctx, j)
		return OutcomeCompleted, nil
	case PhaseRolledBack:
		_ = s.terminalCleanup(ctx, j)
		return OutcomeDiscarded, nil
	case PhaseDiscarded:
		_ = s.terminalCleanup(ctx, j)
		return OutcomeDiscarded, nil
	case PhasePrepared:
		if err := s.discardPrepared(ctx, j); err != nil {
			return OutcomeNone, err
		}
		return OutcomeDiscarded, nil
	}

	canon, err := recovery.CanonicalizeCodexHome(targetHome)
	if err != nil {
		return OutcomeTargetHomeChanged, nil
	}
	if recovery.HashBytes([]byte(canon)) != j.TargetHomeFingerprint {
		return OutcomeTargetHomeChanged, nil
	}

	m, err := s.store.ReadBackout(ctx, j.TransactionID, j.BackoutManifestSHA256)
	if err != nil {
		if asErrorKind(err) == KindExternalModification {
			return OutcomeConflict, nil
		}
		if ok, verr := allTarget(canon, j); verr != nil {
			return OutcomeNone, verr
		} else if ok {
			if cerr := s.complete(ctx, canon, j); cerr != nil {
				return OutcomeNone, cerr
			}
			return OutcomeCompleted, nil
		}
		return OutcomeRecoveryRequired, nil
	}

	states, err := classifyFiles(canon, j, m)
	if err != nil {
		return OutcomeNone, err
	}
	var hasTarget, hasPrevious, hasOther bool
	for _, st := range states {
		switch st {
		case fileTarget:
			hasTarget = true
		case filePrevious:
			hasPrevious = true
		case fileOther:
			hasOther = true
		}
	}
	if hasOther {
		return OutcomeConflict, nil
	}
	if hasTarget && !hasPrevious {
		if err := s.complete(ctx, canon, j); err != nil {
			return OutcomeNone, err
		}
		return OutcomeCompleted, nil
	}
	if hasPrevious && !hasTarget {
		if err := s.discardAfterBackout(ctx, j); err != nil {
			return OutcomeNone, err
		}
		return OutcomeDiscarded, nil
	}
	if err := s.rollback(ctx, canon); err != nil {
		switch asErrorKind(err) {
		case KindExternalModification:
			return OutcomeConflict, nil
		case KindRollbackFailed:
			return OutcomeRecoveryRequired, nil
		default:
			return OutcomeNone, err
		}
	}
	return OutcomeRolledBack, nil
}

// complete promotes the journal to completed after re-verifying every target
// file matches its post-publish expectation (decision 4 of §12; the forward case
// that needs no backout). The journal is first advanced to verified with the
// full publish state and rollback fields cleared — a mid-publish or
// rollback_required journal would otherwise fail validation — then to completed
// with CompletedAt, then the terminal cleanup runs.
func (s *Service) complete(ctx context.Context, canon string, j *Journal) error {
	for _, ef := range j.ExpectedFiles {
		ok, err := verifyTargetFile(canon, ef)
		if err != nil {
			return err
		}
		if !ok {
			return newError(KindExternalModification, "target files changed during completion")
		}
	}
	j.Phase = PhaseVerified
	j.PublishedFiles = []FileID{FileModelsCatalog, FileAuth, FileConfig}
	j.CommitMarkerPublished = true
	j.RollbackAttempted = false
	j.RollbackFromPhase = nil
	j.UpdatedAt = s.stamp()
	if err := s.store.Write(ctx, j); err != nil {
		return err
	}
	completedAt := s.stamp()
	j.Phase = PhaseCompleted
	j.CompletedAt = &completedAt
	j.UpdatedAt = completedAt
	if err := s.store.Write(ctx, j); err != nil {
		return err
	}
	return s.terminalCleanup(ctx, j)
}

// discardPrepared discards a prepared journal: the transaction never published
// anything, so only the backout (absent or partial) and the journal are removed.
// The target home is never touched, so an external change is never destroyed.
// The journal is deleted only after the backout removal succeeds, so no orphaned
// backout outlives its journal.
func (s *Service) discardPrepared(ctx context.Context, j *Journal) error {
	if err := s.store.DeleteBackout(ctx, j.TransactionID); err != nil {
		return err
	}
	return s.store.Delete(ctx)
}

// discardAfterBackout discards a journal whose target files are all at their
// previous state (decision 5 of §12). It first advances the journal to the
// terminal PhaseDiscarded with the rollback fields cleared — so a crash
// mid-cleanup resumes from PhaseDiscarded rather than leaving an orphaned
// backout — then removes the backout and finally the journal.
func (s *Service) discardAfterBackout(ctx context.Context, j *Journal) error {
	j.Phase = PhaseDiscarded
	j.PublishedFiles = nil
	j.CommitMarkerPublished = false
	j.RollbackAttempted = false
	j.RollbackFromPhase = nil
	j.CompletedAt = nil
	j.UpdatedAt = s.stamp()
	if err := s.store.Write(ctx, j); err != nil {
		return err
	}
	return s.terminalCleanup(ctx, j)
}

// terminalCleanup removes the backout transaction and then the journal. The
// backout goes first so a stale backout (which can hold the previous auth.json)
// is never orphaned; a failed backout removal keeps the terminal journal so the
// next startup or publish retries the cleanup.
func (s *Service) terminalCleanup(ctx context.Context, j *Journal) error {
	if err := s.store.DeleteBackout(ctx, j.TransactionID); err != nil {
		return err
	}
	return s.store.Delete(ctx)
}
