package publishrecovery

import (
	"context"
	"os"
	"path/filepath"
)

// fileState is the per-target reconciliation classification of §12 / R8-2: the
// current bytes of a target file are judged TARGET (match the post-publish
// expectation), PREVIOUS (match the pre-publish backout), or OTHER (neither —
// partial write or external modification).
type fileState int

const (
	fileTarget fileState = iota
	filePrevious
	fileOther
)

// classifyFile classifies one target file against its expected and previous
// states. An absent target is TARGET only when absence is expected, PREVIOUS
// only when it was previously absent, and OTHER otherwise (expected exist +
// previously existed, yet gone). An unreadable target that still exists is an
// I/O failure, not a classification.
func classifyFile(canon string, expected ExpectedFile, prev BackoutEntry) (fileState, error) {
	data, err := os.ReadFile(filepath.Join(canon, fileNameFor(expected.File)))
	if err != nil {
		if os.IsNotExist(err) {
			switch {
			case !expected.ExpectedExist:
				return fileTarget, nil
			case !prev.PreviousExists:
				return filePrevious, nil
			default:
				return fileOther, nil
			}
		}
		return fileOther, newError(KindBackoutFailed, "read target file failed")
	}
	if expected.ExpectedExist && sha256Hex(data) == expected.SHA256 {
		return fileTarget, nil
	}
	if prev.PreviousExists && sha256Hex(data) == prev.SHA256 {
		return filePrevious, nil
	}
	return fileOther, nil
}

// classifyFiles classifies the three publish targets in backoutOrder, pairing
// j.ExpectedFiles with manifest.Entries by index (both are fixed-order).
func classifyFiles(canon string, j *Journal, m *BackoutManifest) ([]fileState, error) {
	states := make([]fileState, 0, len(backoutOrder))
	for i := range backoutOrder {
		st, err := classifyFile(canon, j.ExpectedFiles[i], m.Entries[i])
		if err != nil {
			return nil, err
		}
		states = append(states, st)
	}
	return states, nil
}

// allTarget reports whether every target file matches its post-publish
// expectation (decision 4 of §12: the complete case, which needs no backout).
func allTarget(canon string, j *Journal) (bool, error) {
	for _, ef := range j.ExpectedFiles {
		ok, err := verifyTargetFile(canon, ef)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// rollbackFromFor maps a journal phase to the rollback branch point. Phases in
// rollbackFromAllowed map to themselves; verified maps to config_published
// because a verified journal has published all three targets and rolls back from
// the same restore set. Any other phase is not rollbackable.
func rollbackFromFor(p Phase) (Phase, bool) {
	if validRollbackFrom(p) {
		return p, true
	}
	if p == PhaseVerified {
		return PhaseConfigPublished, true
	}
	return "", false
}

// rollback restores the target files to their pre-publish state from the backout
// transaction. It is the shared rollback path for both the immediate in-process
// rollback after a real publish failure and startup reconciliation.
//
// Ordering contract (RB0/RB1, R8-3, and the rolled_back durability rule):
//
//  1. The journal is reloaded from disk: advance mutates the in-memory journal
//     before writing, so after a failed write the durable phase is what matters.
//  2. The target home fingerprint is re-verified; a mismatch changes nothing.
//  3. A terminal rollback journal (rolled_back/rollback_failed) is not
//     rollbackable again.
//  4. The backout manifest must be readable. A tamper (KindExternalModification)
//     aborts without writing rollback_failed. Any other read failure records
//     rollback_failed best-effort and fails.
//  5. Files are classified first (R8-3): any OTHER aborts before anything is
//     written — rollback never overwrites an external modification.
//  6. Unless already at rollback_required (an RB1 retry), the journal is advanced
//     to rollback_required (RollbackAttempted=true) and written durably BEFORE
//     any target is mutated. A write failure means no restore is started and the
//     journal stays at its forward phase for the next reconciliation to retry.
//  7. Files are restored in reverse publish order (config → auth → catalog),
//     each with a FaultDuringRollback seam. A restore failure records
//     rollback_failed best-effort.
//  8. After all files are restored the journal is advanced to rolled_back with a
//     DURABLE write, and only after that write succeeds is the terminal cleanup
//     performed. A rolled_back write failure keeps the journal at
//     rollback_required with the backout retained, so the next reconciliation
//     classifies all-PREVIOUS → PhaseDiscarded → cleanup.
//
// A best-effort rollback_failed write that itself fails is not treated as the
// failure having been durably recorded: the journal stays at rollback_required
// and the next reconciliation retries from RB1.
func (s *Service) rollback(ctx context.Context, canon string) error {
	j, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	if j == nil {
		return newError(KindRollbackFailed, "no publish journal to roll back")
	}
	if _, err := s.recheckTargetHome(canon, j.TargetHomeFingerprint); err != nil {
		return err
	}
	if j.Phase == PhaseRolledBack || j.Phase == PhaseRollbackFailed {
		return newError(KindRollbackFailed, "cannot roll back a terminal rollback journal")
	}
	m, err := s.store.ReadBackout(ctx, j.TransactionID, j.BackoutManifestSHA256)
	if err != nil {
		if asErrorKind(err) == KindExternalModification {
			return err
		}
		s.markRollbackFailed(ctx, j)
		return newError(KindRollbackFailed, "backout is unavailable for rollback")
	}
	states, err := classifyFiles(canon, j, m)
	if err != nil {
		return err
	}
	for _, st := range states {
		if st == fileOther {
			return newError(KindExternalModification, "external modification prevents rollback")
		}
	}
	if j.Phase != PhaseRollbackRequired {
		branchFrom, ok := rollbackFromFor(j.Phase)
		if !ok {
			return newError(KindRollbackFailed, "cannot derive a rollback branch from the current phase")
		}
		j.Phase = PhaseRollbackRequired
		j.RollbackFromPhase = &branchFrom
		j.RollbackAttempted = true
		j.UpdatedAt = s.stamp()
		if err := s.store.Write(ctx, j); err != nil {
			return err
		}
	}
	for i := len(backoutOrder) - 1; i >= 0; i-- {
		if err := s.deps.Fault.Hit(FaultDuringRollback); err != nil {
			return err
		}
		if err := s.restoreFile(ctx, j, canon, m.Entries[i]); err != nil {
			s.markRollbackFailed(ctx, j)
			return newError(KindRollbackFailed, "restoring the previous state failed")
		}
	}
	j.Phase = PhaseRolledBack
	j.RollbackAttempted = true
	j.UpdatedAt = s.stamp()
	if err := s.store.Write(ctx, j); err != nil {
		return err
	}
	return s.terminalCleanup(ctx, j)
}

// restoreFile restores one target file from its backout entry. A previously
// existing target is restored by writing the verified backup bytes back and
// re-reading them to confirm; a previously absent target is restored to absence
// via DurableRemove (the same durability boundary as the forward stale-auth
// removal), confirming the absence afterwards. A missing backup or a hash
// mismatch on the backup bytes is an external modification of the transaction
// directory.
func (s *Service) restoreFile(ctx context.Context, j *Journal, canon string, entry BackoutEntry) error {
	target := filepath.Join(canon, fileNameFor(entry.File))
	if !entry.PreviousExists {
		if err := s.deps.DurableRemove(target); err != nil && !os.IsNotExist(err) {
			return newError(KindBackoutFailed, "restore target absence failed")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			return newError(KindBackoutFailed, "restore absence verification failed")
		}
		return nil
	}
	data, err := s.store.ReadBackup(ctx, j.TransactionID, entry.File)
	if err != nil {
		return err
	}
	if sha256Hex(data) != entry.SHA256 {
		return newError(KindExternalModification, "backup content changed before restore")
	}
	if err := s.deps.AtomicWrite(target, data); err != nil {
		return newError(KindBackoutFailed, "restore target file failed")
	}
	cur, err := os.ReadFile(target)
	if err != nil {
		return newError(KindBackoutFailed, "re-read restored target failed")
	}
	if sha256Hex(cur) != entry.SHA256 {
		return newError(KindBackoutFailed, "restored target verification failed")
	}
	return nil
}

// markRollbackFailed advances the journal to rollback_failed as a best-effort
// write. The write may fail (or the branch point may be underivable); a failure
// is deliberately ignored — the rollback failure is not treated as durably
// recorded, the journal/backout are kept, and the next reconciliation retries
// from rollback_required.
func (s *Service) markRollbackFailed(ctx context.Context, j *Journal) {
	if j.RollbackFromPhase == nil {
		branchFrom, ok := rollbackFromFor(j.Phase)
		if !ok {
			return
		}
		j.RollbackFromPhase = &branchFrom
	}
	j.Phase = PhaseRollbackFailed
	j.RollbackAttempted = true
	j.UpdatedAt = s.stamp()
	_ = s.store.Write(ctx, j)
}
