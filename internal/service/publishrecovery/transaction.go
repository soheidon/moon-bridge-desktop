package publishrecovery

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"moonbridge/internal/service/recovery"
)

// ServiceOptions configures a publish transaction Service.
type ServiceOptions struct {
	// RecoveryDir is %LOCALAPPDATA%\Moon Bridge\recovery, absolute and required.
	// The journal and the backout transaction root live beneath it.
	RecoveryDir string
	// Dependencies are the filesystem/time/fault seams. Unset fields default to
	// the production implementations (see NewStore).
	Dependencies Dependencies
}

// Service runs durable publish transactions. A single internal mutex serializes
// Publish (and, in later steps, reconcile/rollback), so the journal and target
// files are never touched by two transactions at once.
type Service struct {
	mu    sync.Mutex
	store *Store
	deps  Dependencies
}

// New builds a publish transaction Service rooted at RecoveryDir.
//
// Service does not keep its own copy of opts.Dependencies: it shares the
// Store's value, which NewStore has already run through normalizeDependencies.
// A zero-value Dependencies therefore yields a fully defaulted seam set on both
// Service and Store — e.g. Service.deps.DurableRemove is never nil in a
// production configuration, so applyAuth cannot nil-panic.
func New(opts ServiceOptions) (*Service, error) {
	store, err := NewStore(Options{RecoveryDir: opts.RecoveryDir}, opts.Dependencies)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, deps: store.deps}, nil
}

// PublishInput carries the post-publish bytes for the three publish targets.
// Target file names are derived internally from the fixed FileID enums; no
// caller-supplied path is ever used.
type PublishInput struct {
	TargetHome    string
	ModelsCatalog []byte
	AuthRequired  bool
	AuthJSON      []byte
	ConfigTOML    []byte
}

// Publish performs a crash-safe publish in the plan's fixed order: journal
// prepared → backout copy → journal backout_copied → catalog → journal
// catalog_published → auth → journal auth_published → config (commit marker) →
// journal config_published → verify → journal verified → journal completed →
// cleanup. Every phase is journalled before the next target is mutated, so a
// crash at any point leaves a durable record for startup reconciliation.
//
// The target home fingerprint recorded at start is re-verified immediately before
// every mutation (backout, catalog, auth, config, final verify). A home swapped
// mid-publish is KindTargetHomeChanged and nothing is written or removed — the
// journal and backout are left intact. This is not a full TOCTOU elimination,
// but it keeps every write rooted at the home the journal was bound to.
//
// A fault-seam hit aborts Publish immediately (simulated crash) with the journal
// at its last durable phase. A real operation failure likewise returns an error
// with the journal at its last durable phase. IMPORTANT: immediate in-process
// rollback on a real failure (journal rollback_required → restore from backout)
// is implemented in Step 3D (rollback.go); until Step 3D lands, Publish must NOT
// be connected to any production publish path. A successful publish removes the
// backout directory and the completed journal.
func (s *Service) Publish(ctx context.Context, in PublishInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.rejectUnfinished(ctx); err != nil {
		return err
	}

	canon, err := recovery.CanonicalizeCodexHome(in.TargetHome)
	if err != nil {
		return newError(KindConfigPathInvalid, "target home is not a valid codex home")
	}
	txID := s.deps.NewID()
	if err := ValidateTransactionID(txID); err != nil {
		return newError(KindTransactionInvalid, "generated transaction id is invalid")
	}

	started := s.stamp()
	j := &Journal{
		SchemaVersion:         SchemaVersion,
		TransactionID:         txID,
		Phase:                 PhasePrepared,
		StartedAt:             started,
		UpdatedAt:             started,
		TargetHomeFingerprint: recovery.HashBytes([]byte(canon)),
		ExpectedFiles: []ExpectedFile{
			{File: FileModelsCatalog, ExpectedExist: true, SHA256: recovery.HashBytes(in.ModelsCatalog)},
			{File: FileAuth, ExpectedExist: in.AuthRequired, SHA256: authHash(in.AuthRequired, in.AuthJSON)},
			{File: FileConfig, ExpectedExist: true, SHA256: recovery.HashBytes(in.ConfigTOML)},
		},
		AuthRequired: in.AuthRequired,
	}
	if err := s.store.Write(ctx, j); err != nil {
		return err
	}
	if err := s.deps.Fault.Hit(FaultAfterPreparedJournal); err != nil {
		return err
	}

	canon, err = s.recheckTargetHome(in.TargetHome, j.TargetHomeFingerprint)
	if err != nil {
		return err
	}
	manifestHash, err := s.store.CreateBackout(ctx, CreateBackoutOptions{TransactionID: txID, TargetHome: canon})
	if err != nil {
		return err
	}
	if err := s.deps.Fault.Hit(FaultAfterBackoutCopy); err != nil {
		return err
	}
	if err := s.advance(ctx, j, PhaseBackoutCopied, nil, func(j *Journal) { j.BackoutManifestSHA256 = manifestHash }); err != nil {
		return err
	}

	canon, err = s.recheckTargetHome(in.TargetHome, j.TargetHomeFingerprint)
	if err != nil {
		return err
	}
	if err := s.deps.AtomicWrite(filepath.Join(canon, fileNameFor(FileModelsCatalog)), in.ModelsCatalog); err != nil {
		return newError(KindBackoutFailed, "write models catalog failed")
	}
	if err := s.deps.Fault.Hit(FaultAfterCatalogWrite); err != nil {
		return err
	}
	if err := s.advance(ctx, j, PhaseCatalogPublished, []FileID{FileModelsCatalog}, nil); err != nil {
		return err
	}
	if err := s.deps.Fault.Hit(FaultAfterCatalogJournal); err != nil {
		return err
	}

	canon, err = s.recheckTargetHome(in.TargetHome, j.TargetHomeFingerprint)
	if err != nil {
		return err
	}
	if err := s.applyAuth(ctx, canon, in); err != nil {
		return err
	}
	if err := s.deps.Fault.Hit(FaultAfterAuthWrite); err != nil {
		return err
	}
	if err := s.advance(ctx, j, PhaseAuthPublished, []FileID{FileModelsCatalog, FileAuth}, nil); err != nil {
		return err
	}
	if err := s.deps.Fault.Hit(FaultAfterAuthJournal); err != nil {
		return err
	}

	canon, err = s.recheckTargetHome(in.TargetHome, j.TargetHomeFingerprint)
	if err != nil {
		return err
	}
	if err := s.deps.AtomicWrite(filepath.Join(canon, fileNameFor(FileConfig)), in.ConfigTOML); err != nil {
		return newError(KindBackoutFailed, "write config failed")
	}
	if err := s.deps.Fault.Hit(FaultAfterConfigWrite); err != nil {
		return err
	}
	if err := s.advance(ctx, j, PhaseConfigPublished, []FileID{FileModelsCatalog, FileAuth, FileConfig}, func(j *Journal) { j.CommitMarkerPublished = true }); err != nil {
		return err
	}
	if err := s.deps.Fault.Hit(FaultAfterConfigJournal); err != nil {
		return err
	}

	canon, err = s.recheckTargetHome(in.TargetHome, j.TargetHomeFingerprint)
	if err != nil {
		return err
	}
	for _, ef := range j.ExpectedFiles {
		if ok, err := verifyTargetFile(canon, ef); err != nil {
			return err
		} else if !ok {
			return newError(KindBackoutFailed, "publish verification failed")
		}
	}
	if err := s.advance(ctx, j, PhaseVerified, []FileID{FileModelsCatalog, FileAuth, FileConfig}, nil); err != nil {
		return err
	}
	if err := s.deps.Fault.Hit(FaultAfterVerified); err != nil {
		return err
	}

	completedAt := s.stamp()
	j.Phase = PhaseCompleted
	j.CompletedAt = &completedAt
	j.UpdatedAt = completedAt
	if err := s.store.Write(ctx, j); err != nil {
		return err
	}

	// Best-effort cleanup through the safe Store API: a failed backout removal
	// keeps the completed journal so the next startup (or publish) retries the
	// cleanup; the publish itself has succeeded. The journal delete is only
	// attempted once the backout is gone, so a stale backout is never orphaned.
	if err := s.store.DeleteBackout(ctx, txID); err == nil {
		_ = s.store.Delete(ctx)
	}
	return nil
}

// rejectUnfinished blocks a fresh publish while a previous transaction is not
// terminal. A completed or rolled-back journal is a stale terminal record: the
// single-journal slot is released only once BOTH the old backout and the old
// journal are durably gone. If either removal fails the old journal must survive
// and the new publish is rejected — writing a fresh prepared journal over an
// uncleaned transaction would corrupt ownership between the two.
func (s *Service) rejectUnfinished(ctx context.Context) error {
	cur, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	if cur == nil {
		return nil
	}
	switch cur.Phase {
	case PhaseCompleted, PhaseRolledBack:
		if err := s.store.DeleteBackout(ctx, cur.TransactionID); err != nil {
			return newError(KindTransactionActive, "cannot start a publish while the previous transaction is not cleaned up")
		}
		if err := s.store.Delete(ctx); err != nil {
			return newError(KindTransactionActive, "cannot start a publish while the previous transaction is not cleaned up")
		}
		return nil
	default:
		return newError(KindTransactionActive, "an unfinished publish transaction exists")
	}
}

// recheckTargetHome re-canonicalizes the target home immediately before a
// mutation and verifies its fingerprint still matches the journal's recorded
// fingerprint. A mismatch (the home was swapped, moved, or became invalid since
// the transaction started) is KindTargetHomeChanged: nothing is written or
// removed and the journal/backout are left intact.
func (s *Service) recheckTargetHome(home, expectedFingerprint string) (string, error) {
	canon, err := recovery.CanonicalizeCodexHome(home)
	if err != nil {
		return "", newError(KindTargetHomeChanged, "target home changed during publish")
	}
	if recovery.HashBytes([]byte(canon)) != expectedFingerprint {
		return "", newError(KindTargetHomeChanged, "target home changed during publish")
	}
	return canon, nil
}

// advance journals the next phase, carrying over the accumulated
// publishedFiles/commitMarkerPublished state of the running transaction.
func (s *Service) advance(ctx context.Context, j *Journal, phase Phase, published []FileID, mutate func(*Journal)) error {
	j.Phase = phase
	j.PublishedFiles = published
	if mutate != nil {
		mutate(j)
	}
	j.UpdatedAt = s.stamp()
	if err := s.store.Write(ctx, j); err != nil {
		return err
	}
	return nil
}

func (s *Service) stamp() string { return s.deps.Now().UTC().Format(time.RFC3339) }

func authHash(required bool, data []byte) string {
	if !required {
		return ""
	}
	return recovery.HashBytes(data)
}

// applyAuth writes the new auth.json when auth is required, or removes a stale
// one when it is not (absence is the correct post-publish state). The stale
// removal goes through DurableRemove: delete → confirm absent → parent-dir sync
// (Unix) before the journal may advance to auth_published, because the crash
// window against a stale auth.json is judged on the removal being durable.
func (s *Service) applyAuth(ctx context.Context, canon string, in PublishInput) error {
	path := filepath.Join(canon, fileNameFor(FileAuth))
	if in.AuthRequired {
		if err := s.deps.AtomicWrite(path, in.AuthJSON); err != nil {
			return newError(KindBackoutFailed, "write auth failed")
		}
		return nil
	}
	if err := s.deps.DurableRemove(path); err != nil && !os.IsNotExist(err) {
		return newError(KindBackoutFailed, "remove stale auth failed")
	}
	return nil
}

// verifyTargetFile reports whether a target file matches its post-publish
// expectation. A missing file is expected only for an ExpectedExist=false entry.
func verifyTargetFile(home string, ef ExpectedFile) (bool, error) {
	data, err := os.ReadFile(filepath.Join(home, fileNameFor(ef.File)))
	if err != nil {
		if os.IsNotExist(err) {
			return !ef.ExpectedExist, nil
		}
		return false, newError(KindBackoutFailed, "read target file failed")
	}
	if !ef.ExpectedExist {
		return false, nil
	}
	return recovery.HashBytes(data) == ef.SHA256, nil
}
