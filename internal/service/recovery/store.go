package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"moonbridge/internal/service/codexconfig"
)

// Paths resolves the app-managed roots Recovery state fields are relative to.
// No absolute path is ever written to the journal; a root identifier plus a
// root-relative path (or logical id) is used instead. Callers construct a Paths
// value; the Store copies it so the external pointer is never held.
type Paths struct {
	// RecoveryDir is %LOCALAPPDATA%\Moon Bridge\recovery.
	RecoveryDir string
	// CodexHome is the CODEX_HOME resolved when a traffic session starts. A
	// CODEX_HOME change on the next boot is flagged config_path_invalid.
	CodexHome string
	// BackupDir is %LOCALAPPDATA%\Moon Bridge\backups\codex-config.
	BackupDir string
	// TrafficLogDir is %LOCALAPPDATA%\Moon Bridge\logs\traffic-analysis.
	TrafficLogDir string
	// AppDataRoot is %APPDATA%\Moon Bridge Desktop (v1 / migration source root).
	AppDataRoot string
}

// Resolve turns a stored relative path (new contract) into an absolute path
// under root. It rejects traversal outside root.
func Resolve(root, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty relative path")
	}
	clean := filepath.Clean(root)
	joined := filepath.Join(clean, rel)
	if !pathWithin(clean, joined) {
		return "", errors.New("path escapes root")
	}
	return joined, nil
}

// ToRelative returns the root-relative form of an absolute path if it lies
// under root; otherwise it returns an error (the path must not be stored).
func ToRelative(root, abs string) (string, error) {
	abs = stripVerbatimPrefix(abs)
	root = stripVerbatimPrefix(root)
	if !filepath.IsAbs(abs) {
		return "", errors.New("path is not absolute")
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes root")
	}
	return rel, nil
}

// DefaultDir resolves %LOCALAPPDATA%\Moon Bridge (fallback APPDATA / USERPROFILE).
func DefaultDir(env func(string) string) (string, error) {
	root := env("LOCALAPPDATA")
	if root == "" {
		root = env("APPDATA")
	}
	if root == "" {
		root = env("USERPROFILE")
	}
	if root == "" {
		return "", errors.New("LOCALAPPDATA/APPDATA/USERPROFILE is unavailable")
	}
	return filepath.Join(root, "Moon Bridge"), nil
}

// Store is a serializing flat-JSON store for Recovery State. All read-modify-
// write operations go through Update under an internal mutex, so concurrent
// writers never lose updates and never write JSON directly.
type Store struct {
	mu    sync.Mutex
	path  string
	paths Paths // value copy; external Paths pointer is never aliased
	env   func(string) string
}

// NewStore builds a Store for the given app roots. If recoveryStatePath is
// empty it is defaulted to %LOCALAPPDATA%\Moon Bridge\recovery\
// recovery-state-v2.json; a failure to resolve an absolute root is an error, so
// the Store never silently falls back to a relative path. An explicit
// recoveryStatePath must also be absolute (a relative one is rejected). The
// supplied Paths is copied by value.
func NewStore(paths *Paths, recoveryStatePath string) (*Store, error) {
	if recoveryStatePath == "" {
		base, err := DefaultDir(os.Getenv)
		if err != nil {
			return nil, &Error{Kind: KindStateNotFound, Message: "resolve recovery root failed: " + err.Error()}
		}
		if !filepath.IsAbs(base) {
			return nil, &Error{Kind: KindStateNotFound, Message: "recovery root is not absolute"}
		}
		recoveryStatePath = filepath.Join(base, "recovery", "recovery-state-v2.json")
	}
	if !filepath.IsAbs(recoveryStatePath) {
		return nil, &Error{Kind: KindStateNotFound, Message: "recovery state path is not absolute"}
	}
	if paths == nil {
		paths = &Paths{}
	}
	for _, root := range []struct {
		name, val string
	}{
		{"recovery dir", paths.RecoveryDir},
		{"codex home", paths.CodexHome},
		{"backup dir", paths.BackupDir},
		{"traffic log dir", paths.TrafficLogDir},
		{"app data root", paths.AppDataRoot},
	} {
		if root.val != "" && !filepath.IsAbs(root.val) {
			return nil, &Error{Kind: KindStateNotFound, Message: root.name + " must be absolute"}
		}
	}
	return &Store{path: recoveryStatePath, paths: *paths, env: os.Getenv}, nil
}

// Path returns the canonical v2 recovery state path.
func (s *Store) Path() string { return s.path }

// Load reads and decodes the current state. A missing file is not an error:
// it returns (nil, nil) so callers can treat "no state" distinctly. Legacy
// absolute path values are accepted for reading (validation + normalization
// happen at write time).
func (s *Store) Load(ctx context.Context) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadUnlocked(ctx)
}

func (s *Store) loadUnlocked(ctx context.Context) (*State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &Error{Kind: KindStateParseFailed, Message: "read recovery state failed"}
	}
	return s.decode(data)
}

// decode parses bytes, enforcing schemaVersion == 2 exactly (v1 is read only
// through the dedicated Migrate path, never here). Relative (new contract)
// configPath is traversal-checked; legacy absolute values are accepted for
// reading.
func (s *Store) decode(data []byte) (*State, error) {
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, &Error{Kind: KindStateParseFailed, Message: "decode recovery state failed"}
	}
	if st.SchemaVersion != SchemaVersion {
		return nil, &Error{Kind: KindStateUnsupportedVersion, Message: "unsupported recovery state schema version"}
	}
	if err := s.validatePaths(&st); err != nil {
		return nil, err
	}
	if st.PreviousOpenaiBaseURLPresent {
		if st.PreviousOpenaiBaseURL == nil || codexconfig.ValidateTrafficURL(*st.PreviousOpenaiBaseURL) != nil {
			return nil, &Error{Kind: KindRestoreFailed, Message: "recovery original route is invalid"}
		}
	}
	if st.AppliedOpenaiBaseURL != "" && codexconfig.ValidateTrafficURL(st.AppliedOpenaiBaseURL) != nil {
		return nil, &Error{Kind: KindRestoreFailed, Message: "recovery applied route is invalid"}
	}
	return &st, nil
}

// validatePaths guards the read path only: the new relative contract must not
// traverse outside its root (any `..` component is rejected with
// parse_failed), and a legacy absolute value is read as-is — the
// environment-aware physical safety check and write-time normalization happen
// in resolveConfigPath / normalizeForWrite when the path is actually used.
func (s *Store) validatePaths(st *State) error {
	if st.ConfigPath != "" {
		if !filepath.IsAbs(st.ConfigPath) && isTraversalRelative(st.ConfigPath) {
			return &Error{Kind: KindStateParseFailed, Message: "recovery config path traverses outside codex home"}
		}
		if !filepath.IsAbs(st.ConfigPath) && s.paths.CodexHome != "" {
			if _, err := Resolve(s.paths.CodexHome, st.ConfigPath); err != nil {
				return &Error{Kind: KindStateParseFailed, Message: "recovery config path escapes codex home"}
			}
		}
	}
	if st.BackupPath != nil && *st.BackupPath != "" && !filepath.IsAbs(*st.BackupPath) && isTraversalRelative(*st.BackupPath) {
		return &Error{Kind: KindStateParseFailed, Message: "recovery backup path traverses outside backup root"}
	}
	if st.AutoLog != nil && st.AutoLog.Path != "" && !filepath.IsAbs(st.AutoLog.Path) && isTraversalRelative(st.AutoLog.Path) {
		return &Error{Kind: KindStateParseFailed, Message: "recovery autoLog path traverses outside traffic log root"}
	}
	if st.Migration != nil && st.Migration.SourcePath != "" && isTraversalRelative(st.Migration.SourcePath) {
		return &Error{Kind: KindStateParseFailed, Message: "recovery migration source path traverses outside app data root"}
	}
	return nil
}

// isTraversalRelative reports whether a relative path contains a `..`
// component, which would escape its root. It is a root-independent lexical
// check covering both / and \ separators, so the same rule applies to every
// path field regardless of the platform a state file was written on.
func isTraversalRelative(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// Update serializes a read-modify-write: it Loads the current state (nil if
// absent), lets fn mutate it in place, and atomically persists via
// normalizeForWrite. fn must leave current non-nil; deletion is the explicit
// Delete method, not a nil return. fn may return errChangesSkipped to abort
// without persisting and without error.
func (s *Store) Update(ctx context.Context, fn func(current *State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.loadUnlocked(ctx)
	if err != nil {
		return err
	}
	if err := fn(cur); err != nil {
		if errors.Is(err, errChangesSkipped) {
			return nil
		}
		return err
	}
	if cur == nil {
		return &Error{Kind: KindStateParseFailed, Message: "update produced a nil recovery state"}
	}
	return s.writeLocked(ctx, cur)
}

// UpdateOrCreate is the atomic counterpart for callers that may be writing
// the first Recovery record. Unlike Update, the callback is given a fresh
// schema-v2 state when the file does not exist. The load, callback, and write
// remain under one Store lock so two first checkpoints cannot overwrite one
// another or observe a partially initialized record.
func (s *Store) UpdateOrCreate(ctx context.Context, fn func(current *State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.loadUnlocked(ctx)
	if err != nil {
		return err
	}
	if cur == nil {
		cur = New()
	}
	if err := fn(cur); err != nil {
		if errors.Is(err, errChangesSkipped) {
			return nil
		}
		return err
	}
	return s.writeLocked(ctx, cur)
}

// Write persists a full State atomically, running normalizeForWrite first.
func (s *Store) Write(ctx context.Context, st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(ctx, st)
}

// Delete removes the recovery state. It is idempotent: a missing file is
// success. Runs under the Store mutex like all other writes.
func (s *Store) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return &Error{Kind: KindStateParseFailed, Message: "remove recovery state failed"}
	}
	return nil
}

// DeleteIf atomically validates the current state and deletes it while the
// Store mutex is held. It is used by discard transactions so a stale read
// cannot delete a newer Recovery record. A false predicate is a safe no-op.
func (s *Store) DeleteIf(ctx context.Context, predicate func(*State) (bool, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.loadUnlocked(ctx)
	if err != nil {
		return false, err
	}
	if cur == nil {
		return false, nil
	}
	ok, err := predicate(cur)
	if err != nil || !ok {
		return false, err
	}
	if err := os.Remove(s.path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, &Error{Kind: KindStateParseFailed, Message: "remove recovery state failed"}
	}
	return true, nil
}

// ClearCleanupPending atomically clears a matching cleanup record. If no
// regular recovery evidence remains, it removes the state file as part of the
// same store-serialized operation.
func (s *Store) ClearCleanupPending(ctx context.Context, transactionID, backupID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.loadUnlocked(ctx)
	if err != nil || current == nil || current.CleanupPending == nil {
		return false, err
	}
	if current.CleanupPending.TransactionID != transactionID || current.CleanupPending.BackupID != backupID {
		return false, nil
	}
	current.CleanupPending = nil
	if !hasRegularRecovery(current) {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return false, &Error{Kind: KindStateParseFailed, Message: "remove recovery state failed"}
		}
		return true, nil
	}
	return true, s.writeLocked(ctx, current)
}

func hasRegularRecovery(st *State) bool {
	if st == nil || st.CleanupPending != nil {
		return false
	}
	return st.IntegrationActive || st.OperationID != "" || st.ConfigPath != "" ||
		st.TransitionID != "" || st.RoutePhase != "" || st.DesiredRoute != "" || st.RouteEvidence != "" ||
		st.ConfigHashBeforeApply != "" || st.ConfigHashAfterApply != "" ||
		st.UnsavedObservationsMayRemain || st.Phase != PhaseInactive && st.Phase != PhaseReconciledRestored
}

// errChangesSkipped is returned by Update callbacks that decide nothing should
// be persisted (e.g. a reconciliation that found no work). Update treats it as
// a successful no-op, not an error.
var errChangesSkipped = errors.New("recovery changes skipped")

func (s *Store) writeLocked(ctx context.Context, st *State) error {
	if st == nil {
		return &Error{Kind: KindStateParseFailed, Message: "cannot write a nil recovery state"}
	}
	// Validate + normalize (path roots, schemaVersion, phase, UpdatedAt) before
	// persisting. On failure the existing file is left untouched.
	norm, err := s.normalizeForWrite(st)
	if err != nil {
		return err
	}
	if err := s.validateNormalizedState(norm); err != nil {
		return err
	}
	return s.writeLockedNorm(ctx, norm)
}

// writeLockedMigrated persists a state produced by v1→v2 migration. Migrate has
// already sanitized every path (relativized/diagnosed/dropped) before calling, so
// this only validates schema/phase, runs the shared pre-marshal validator, stamps
// UpdatedAt, and writes — it does not re-run normalizeForWrite. Running
// validateNormalizedState here means a future sanitize gap can never leak an
// absolute path or traversal into v2.
func (s *Store) writeLockedMigrated(ctx context.Context, st *State) error {
	if st == nil {
		return &Error{Kind: KindStateParseFailed, Message: "cannot write a nil recovery state"}
	}
	if st.SchemaVersion != SchemaVersion {
		return &Error{Kind: KindStateUnsupportedVersion, Message: "unsupported recovery state schema version"}
	}
	if st.Phase == "" {
		return &Error{Kind: KindStateParseFailed, Message: "recovery phase is required before write"}
	}
	norm := *st
	now := nowString()
	norm.UpdatedAt = &now
	if err := s.validateNormalizedState(&norm); err != nil {
		return err
	}
	return s.writeLockedNorm(ctx, &norm)
}

func (s *Store) writeLockedNorm(ctx context.Context, norm *State) error {
	data, err := json.MarshalIndent(norm, "", "  ")
	if err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "encode recovery state failed"}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "create recovery dir failed"}
	}
	if err := codexconfig.AtomicWrite(s.path, data); err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "atomic write recovery state failed"}
	}
	return nil
}

// normalizeForWrite returns a deep copy of st that is safe to persist:
// schemaVersion must be exactly 2 (never force-written), a phase is required,
// UpdatedAt is stamped, and every path field is normalized to a root-relative
// (or logical) form, rejecting any that escapes its allowed root (physically,
// symlinks/junctions included). It never mutates st — pointer fields are copied
// so a failed normalization leaves st byte-for-byte unchanged. On any failure the
// caller must not persist.
func (s *Store) normalizeForWrite(st *State) (*State, error) {
	if st.SchemaVersion != SchemaVersion {
		return nil, &Error{Kind: KindStateUnsupportedVersion, Message: "unsupported recovery state schema version"}
	}
	if st.Phase == "" {
		return nil, &Error{Kind: KindStateParseFailed, Message: "recovery phase is required before write"}
	}
	out := cloneState(st)
	now := nowString()
	out.UpdatedAt = &now

	var err error
	if out.PreviousOpenaiBaseURLPresent {
		if out.PreviousOpenaiBaseURL == nil {
			return nil, &Error{Kind: KindRestoreFailed, Message: "recovery original route is invalid"}
		}
		if err := codexconfig.ValidateTrafficURL(*out.PreviousOpenaiBaseURL); err != nil {
			return nil, &Error{Kind: KindRestoreFailed, Message: "recovery original route is invalid"}
		}
	}
	if out.AppliedOpenaiBaseURL != "" {
		if err := codexconfig.ValidateTrafficURL(out.AppliedOpenaiBaseURL); err != nil {
			return nil, &Error{Kind: KindRestoreFailed, Message: "recovery applied route is invalid"}
		}
	}
	if out.ConfigPath != "" {
		out.ConfigPath, err = s.normalizePath(out.ConfigPath, s.paths.CodexHome)
		if err != nil {
			return nil, &Error{Kind: KindConfigPathInvalid, Message: "invalid recovery configPath"}
		}
	}
	if out.BackupPath != nil && *out.BackupPath != "" {
		var rel string
		rel, err = s.normalizePath(*out.BackupPath, s.paths.BackupDir)
		if err != nil {
			return nil, &Error{Kind: KindRestoreFailed, Message: "invalid recovery backupPath"}
		}
		out.BackupPath = &rel
	}
	if out.AutoLog != nil && out.AutoLog.Path != "" {
		var rel string
		rel, err = s.normalizePath(out.AutoLog.Path, s.paths.TrafficLogDir)
		if err != nil {
			return nil, &Error{Kind: KindStateParseFailed, Message: "invalid recovery autoLog.path"}
		}
		out.AutoLog.Path = rel
	}
	if out.Migration != nil && out.Migration.SourcePath != "" {
		out.Migration.SourcePath = s.normalizeMigrationSource(out.Migration.SourcePath)
	}
	return out, nil
}

// normalizePath turns a path into a root-relative one, validating that the
// physical location stays inside root. A relative (new-contract) input must
// resolve inside root without traversal before being stored; an absolute input
// must physically resolve inside root (symlinks/junctions/reparse cannot escape).
// root=="" rejects any non-empty path.
func (s *Store) normalizePath(p, root string) (string, error) {
	if p == "" {
		return "", nil
	}
	if root == "" {
		return "", errors.New("no root configured for path")
	}
	if filepath.IsAbs(p) {
		if !pathWithinPhysical(root, p) {
			return "", errors.New("path resolves outside its allowed root")
		}
		rel, err := ToRelative(root, p)
		if err != nil {
			return "", err
		}
		return rel, nil
	}
	resolved, err := Resolve(root, p)
	if err != nil {
		return "", err
	}
	if !pathWithinPhysical(root, resolved) {
		return "", errors.New("path resolves outside its allowed root")
	}
	rel, err := ToRelative(root, resolved)
	if err != nil {
		return "", err
	}
	return rel, nil
}

// normalizeMigrationSource stores migration.sourcePath as a logical identifier
// or an AppDataRoot-relative path, never an arbitrary absolute path. The old
// APPDATA root is informational (never opened), so lexical containment is
// sufficient here.
func (s *Store) normalizeMigrationSource(abs string) string {
	if !filepath.IsAbs(abs) {
		return abs
	}
	if s.paths.AppDataRoot != "" {
		if pathWithin(filepath.Clean(s.paths.AppDataRoot), filepath.Clean(abs)) {
			if rel, err := ToRelative(s.paths.AppDataRoot, abs); err == nil {
				return rel
			}
		}
	}
	return filepath.Base(abs) // logical identifier
}

// cloneState returns a deep copy of st: every pointer field is copied so the
// clone never aliases the original. Used by normalizeForWrite (a failed
// normalization must leave the caller's State untouched) and by the reconcile
// field-diff guard (the before snapshot must not be mutated through the after
// state's pointers).
func cloneState(st *State) *State {
	if st == nil {
		return nil
	}
	out := *st
	copyStr := func(p *string) *string {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	out.PreviousOpenaiBaseURL = copyStr(st.PreviousOpenaiBaseURL)
	out.BackupPath = copyStr(st.BackupPath)
	out.UpdatedAt = copyStr(st.UpdatedAt)
	out.AutoLogStatus = copyStr(st.AutoLogStatus)
	out.ReconciliationStatus = copyStr(st.ReconciliationStatus)
	out.ReconciledAt = copyStr(st.ReconciledAt)
	out.ReconciliationDetail = copyStr(st.ReconciliationDetail)
	if st.AutoLog != nil {
		a := *st.AutoLog
		a.LastCheckpointAt = copyStr(st.AutoLog.LastCheckpointAt)
		out.AutoLog = &a
	}
	if st.Migration != nil {
		m := *st.Migration
		out.Migration = &m
	}
	if st.CleanupPending != nil {
		c := *st.CleanupPending
		out.CleanupPending = &c
	}
	return &out
}

// validateNormalizedState is the shared pre-marshal validator for the normal
// write paths (writeLocked and writeLockedMigrated). It enforces the v2 write
// contract that normalizeForWrite / migration sanitization are supposed to
// guarantee, independent of either caller: schemaVersion==2, a non-empty phase,
// no absolute path in any path field, no traversal, and — for a non-empty
// relative configPath — a codexHomeFingerprint that matches the current codex
// home. writeClassificationLocked is the documented exception and never runs
// here (it patches raw JSON without re-encoding the whole State).
func (s *Store) validateNormalizedState(st *State) error {
	if st.SchemaVersion != SchemaVersion {
		return &Error{Kind: KindStateUnsupportedVersion, Message: "unsupported recovery state schema version"}
	}
	if st.Phase == "" {
		return &Error{Kind: KindStateParseFailed, Message: "recovery phase is required before write"}
	}
	if filepath.IsAbs(st.ConfigPath) {
		return &Error{Kind: KindConfigPathInvalid, Message: "recovery configPath must be relative"}
	}
	if isTraversalRelative(st.ConfigPath) {
		return &Error{Kind: KindConfigPathInvalid, Message: "recovery configPath escapes codex home"}
	}
	if st.ConfigPath != "" {
		fp, err := s.fingerprintCodexHome()
		if err != nil {
			return &Error{Kind: KindConfigPathInvalid, Message: "cannot fingerprint the current codex home"}
		}
		if st.CodexHomeFingerprint == "" {
			return &Error{Kind: KindConfigPathInvalid, Message: "relative configPath requires a codex home fingerprint"}
		}
		if st.CodexHomeFingerprint != fp {
			return &Error{Kind: KindConfigPathInvalid, Message: "codex home changed since the traffic session"}
		}
	}
	if st.BackupPath != nil && *st.BackupPath != "" {
		if filepath.IsAbs(*st.BackupPath) {
			return &Error{Kind: KindRestoreFailed, Message: "recovery backupPath must be relative"}
		}
		if isTraversalRelative(*st.BackupPath) {
			return &Error{Kind: KindRestoreFailed, Message: "recovery backupPath escapes backup root"}
		}
	}
	if st.AutoLog != nil && st.AutoLog.Path != "" {
		if filepath.IsAbs(st.AutoLog.Path) {
			return &Error{Kind: KindStateParseFailed, Message: "recovery autoLog.path must be relative"}
		}
		if isTraversalRelative(st.AutoLog.Path) {
			return &Error{Kind: KindStateParseFailed, Message: "recovery autoLog.path escapes traffic log root"}
		}
	}
	if st.Migration != nil && st.Migration.SourcePath != "" {
		if !validMigrationSource(st.Migration.SourcePath) {
			return &Error{Kind: KindStateParseFailed, Message: "recovery migration.sourcePath is not a valid logical id"}
		}
	}
	if st.CleanupPending != nil {
		if st.CleanupPending.TransactionID == "" || st.CleanupPending.BackupID == "" {
			return &Error{Kind: KindRestoreFailed, Message: "invalid cleanup pending record"}
		}
		if filepath.Base(st.CleanupPending.BackupID) != st.CleanupPending.BackupID || strings.ContainsAny(st.CleanupPending.BackupID, `/\\`) {
			return &Error{Kind: KindRestoreFailed, Message: "invalid cleanup pending record"}
		}
		if st.CleanupPending.RouteMutationResult != "applied" && st.CleanupPending.RouteMutationResult != "restored" && st.CleanupPending.RouteMutationResult != "unchanged" {
			return &Error{Kind: KindRestoreFailed, Message: "invalid cleanup pending record"}
		}
		if st.CleanupPending.Status != "pending" && st.CleanupPending.Status != "persistence_failed" && st.CleanupPending.Status != "delete_failed" && st.CleanupPending.Status != "clear_failed" {
			return &Error{Kind: KindRestoreFailed, Message: "invalid cleanup pending record"}
		}
	}
	return nil
}

// fingerprintCodexHome returns the fingerprint of the current codex home root.
// An unset or non-canonicalizable home is an error: a relative configPath can
// only be root-bound to a home that exists and resolves.
func (s *Store) fingerprintCodexHome() (string, error) {
	if s.paths.CodexHome == "" {
		return "", errors.New("codex home is unset")
	}
	return CodexHomeFingerprint(s.paths.CodexHome)
}

// updateReconciled is the classification-only transaction backing
// ReconcileStartup. Unlike Update it never runs normalizeForWrite: it reads the
// existing raw JSON bytes once, decodes a State for the callback, then patches
// the SAME bytes. Every non-classification field (evidence, unknown/future
// fields, null-vs-missing distinctions) keeps its JSON value: scalar/string
// values and null survive the re-marshal unchanged, and unknown/nested objects
// are preserved semantically (they are never decoded through a Go struct and
// re-encoded). Whole-object byte-identical preservation is NOT guaranteed —
// json.MarshalIndent normalizes whitespace, key order, and nested formatting.
// It is private and only ever called by ReconcileStartup; it never creates a
// new state (a missing raw file means no work, reported as inactive).
func (s *Store) updateReconciled(ctx context.Context, fn func(cur *State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			if ferr := fn(nil); ferr != nil {
				if errors.Is(ferr, errChangesSkipped) {
					return nil
				}
				return ferr
			}
			// A callback that returns nil for a nil state would create a state —
			// reconcile is classification-only and must never do that.
			return newError(KindReconcileFailed, "reconcile must not create a recovery state")
		}
		return &Error{Kind: KindStateParseFailed, Message: "read recovery state failed"}
	}
	// Reject a duplicate top-level key BEFORE decoding to a State. encoding/json
	// silently keeps the LAST value of a duplicate key, so decoding first would
	// let the callback (and its external readConfig) run against a JSON object
	// whose content is not the author's bytes. The classification writer re-checks
	// at patch time as defense in depth, but the callback must never observe an
	// ambiguous document.
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "recovery state has duplicate json keys"}
	}
	cur, err := s.decode(raw)
	if err != nil {
		return err
	}
	before := cloneState(cur)
	if err := fn(cur); err != nil {
		if errors.Is(err, errChangesSkipped) {
			return nil
		}
		return err
	}
	// reconciledAt and updatedAt share one timestamp: updatedAt is stamped from
	// the reconciledAt the callback generated (single generation).
	if cur.ReconciledAt != nil {
		cur.UpdatedAt = cur.ReconciledAt
	}
	if err := assertClassificationOnly(before, cur); err != nil {
		return err
	}
	return s.writeClassificationLocked(ctx, raw, cur)
}

// classificationKeys are the only fields Reconcile may write. Every other JSON
// key (configPath / codexHomeFingerprint / backupPath / autoLog / migration /
// evidence / unknown) is preserved as raw bytes.
var classificationKeys = map[string]bool{
	"phase":                true,
	"reconciliationStatus": true,
	"reconciledAt":         true,
	"reconciliationDetail": true,
	"updatedAt":            true,
	"integrationActive":    true,
}

func isClassificationKey(k string) bool { return classificationKeys[k] }

// writeClassificationLocked patches a raw recovery JSON with only the permitted
// classification fields, keeping the JSON VALUE of every other field: scalar/
// string values and null stay the same (they are carried as json.RawMessage
// through a map and never re-decoded through a struct), and unknown/nested
// objects are preserved semantically. It is the documented exception to
// normalizeForWrite/validateNormalizedState: Reconcile must never decode→
// re-marshal the whole State through a struct (that would lose unknown fields,
// null-vs-missing distinctions, and future/legacy JSON details). Whole-object
// byte-identical preservation is NOT part of the contract — json.MarshalIndent
// normalizes whitespace, key order, and nested formatting.
//
// A duplicate top-level key is rejected before decoding to a map
// (map[string]json.RawMessage cannot represent duplicate keys; silently
// collapsing them would corrupt the original JSON). schemaVersion must be
// exactly 2. Before AtomicWrite the patched output is re-checked on BOTH sides:
// no non-classification key was added, removed, or had its JSON value changed.
func (s *Store) writeClassificationLocked(ctx context.Context, raw []byte, st *State) error {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "recovery state has duplicate json keys"}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "decode recovery state failed"}
	}
	sv, ok := m["schemaVersion"]
	if !ok {
		return &Error{Kind: KindStateParseFailed, Message: "recovery state missing schemaVersion"}
	}
	var svn int
	if err := json.Unmarshal(sv, &svn); err != nil || svn != SchemaVersion {
		return &Error{Kind: KindStateUnsupportedVersion, Message: "unsupported recovery state schema version"}
	}
	original := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		original[k] = v
	}
	if err := setRawString(m, "phase", string(st.Phase)); err != nil {
		return err
	}
	if err := setRawPtr(m, "reconciliationStatus", st.ReconciliationStatus); err != nil {
		return err
	}
	if err := setRawPtr(m, "reconciledAt", st.ReconciledAt); err != nil {
		return err
	}
	if err := setRawPtr(m, "reconciliationDetail", st.ReconciliationDetail); err != nil {
		return err
	}
	if err := setRawPtr(m, "updatedAt", st.UpdatedAt); err != nil {
		return err
	}
	if isAlreadyRestored(st) {
		if err := setRawBool(m, "integrationActive", st.IntegrationActive); err != nil {
			return err
		}
	}
	// Re-check before writing, on BOTH sides: the patched map must not add,
	// remove, or change the JSON value of any non-classification field. Nothing
	// but the classification fields may have changed. (json.RawMessage values are
	// compared byte-for-byte here — the classification patch sets them verbatim
	// from the input, so any difference means the field was genuinely touched.)
	for k, v := range m {
		if isClassificationKey(k) {
			continue
		}
		ov, ok := original[k]
		if !ok {
			return newError(KindReconcileFailed, "classification write added a non-classification field")
		}
		if !bytes.Equal(ov, v) {
			return newError(KindReconcileFailed, "classification write modified a non-classification field")
		}
	}
	for k := range original {
		if isClassificationKey(k) {
			continue
		}
		if _, ok := m[k]; !ok {
			return newError(KindReconcileFailed, "classification write removed a non-classification field")
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "encode recovery state failed"}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "create recovery dir failed"}
	}
	if err := codexconfig.AtomicWrite(s.path, out); err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "atomic write recovery state failed"}
	}
	return nil
}

func setRawString(m map[string]json.RawMessage, key, val string) error {
	b, err := json.Marshal(val)
	if err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "encode recovery classification failed"}
	}
	m[key] = b
	return nil
}

func setRawBool(m map[string]json.RawMessage, key string, val bool) error {
	b, err := json.Marshal(val)
	if err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "encode recovery classification failed"}
	}
	m[key] = b
	return nil
}

func setRawPtr(m map[string]json.RawMessage, key string, val *string) error {
	b, err := json.Marshal(val)
	if err != nil {
		return &Error{Kind: KindStateParseFailed, Message: "encode recovery classification failed"}
	}
	m[key] = b
	return nil
}

// isAlreadyRestored reports whether a reconcile classification resolved the
// integration (status already_restored or phase reconciled_restored), the only
// case where the classification may flip integrationActive.
func isAlreadyRestored(st *State) bool {
	return (st.ReconciliationStatus != nil && *st.ReconciliationStatus == string(StatusAlreadyRestored)) ||
		st.Phase == PhaseReconciledRestored
}

// assertClassificationOnly verifies that a reconcile callback changed only the
// permitted classification fields (phase / reconciliationStatus / reconciledAt /
// reconciliationDetail / updatedAt, plus integrationActive for an
// already_restored classification). Any other change — most importantly the path
// fields and every evidence field — is a KindReconcileFailed error: reconcile
// must never rewrite recovery evidence.
func assertClassificationOnly(before, after *State) error {
	if !isAlreadyRestored(after) && before.IntegrationActive != after.IntegrationActive {
		return newError(KindReconcileFailed, "reconcile must not change integrationActive except on already_restored")
	}
	if before.OperationID != after.OperationID ||
		before.TransitionID != after.TransitionID ||
		before.RoutePhase != after.RoutePhase ||
		before.DesiredRoute != after.DesiredRoute ||
		before.RouteEvidence != after.RouteEvidence ||
		before.ConfigPath != after.ConfigPath ||
		before.CodexHomeFingerprint != after.CodexHomeFingerprint ||
		before.PreviousOpenaiBaseURLPresent != after.PreviousOpenaiBaseURLPresent ||
		!strPtrEq(before.PreviousOpenaiBaseURL, after.PreviousOpenaiBaseURL) ||
		before.AppliedOpenaiBaseURL != after.AppliedOpenaiBaseURL ||
		before.ConfigHashBeforeApply != after.ConfigHashBeforeApply ||
		before.ConfigHashAfterApply != after.ConfigHashAfterApply ||
		!strPtrEq(before.BackupPath, after.BackupPath) ||
		before.StartedAt != after.StartedAt ||
		!autoLogEq(before.AutoLog, after.AutoLog) ||
		!strPtrEq(before.AutoLogStatus, after.AutoLogStatus) ||
		before.UnsavedObservationsMayRemain != after.UnsavedObservationsMayRemain ||
		before.UnsavedDiscardConfirmed != after.UnsavedDiscardConfirmed ||
		!migrationEq(before.Migration, after.Migration) ||
		!cleanupPendingEq(before.CleanupPending, after.CleanupPending) ||
		before.CaptureStateLastKnown != after.CaptureStateLastKnown ||
		before.RelayActiveLastKnown != after.RelayActiveLastKnown ||
		before.RestartAttempted != after.RestartAttempted {
		return newError(KindReconcileFailed, "reconcile must not modify recovery evidence fields")
	}
	return nil
}

func cleanupPendingEq(a, b *CleanupPending) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func autoLogEq(a, b *AutoLogRecoveryState) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SessionID == b.SessionID && a.Path == b.Path &&
		a.LastCheckpointSequence == b.LastCheckpointSequence && a.Finalized == b.Finalized &&
		strPtrEq(a.LastCheckpointAt, b.LastCheckpointAt)
}

func migrationEq(a, b *RecoveryMigrationState) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SourcePath == b.SourcePath && a.SourceSchemaVersion == b.SourceSchemaVersion && a.MigratedAt == b.MigratedAt
}

// rejectDuplicateJSONKeys reports an error when the JSON object at the top level
// contains a duplicate key. map[string]json.RawMessage cannot represent duplicate
// keys (the later value silently wins), so a raw file with duplicates is rejected
// before any patch — the original file is never modified.
func rejectDuplicateJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("recovery state is not a json object")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := ktok.(string)
		if !ok {
			return errors.New("invalid json key")
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("duplicate json key %q", key)
		}
		seen[key] = struct{}{}
		if err := skipJSONValue(dec); err != nil {
			return err
		}
	}
	return nil
}

// skipJSONValue consumes the JSON value at the decoder's current position
// (following an object key), descending into nested objects/arrays so the token
// stream stays aligned.
func skipJSONValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			if depth == 0 {
				return nil
			}
		default:
			if depth == 0 {
				return nil
			}
		}
	}
}

// CodexHome returns the resolved codex home root for relative configPath.
func (s *Store) CodexHome() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paths.CodexHome
}

// SetCodexHome updates the codex home root used to resolve configPath (used
// when a session starts / on reconciliation). It upholds the same root contract
// as NewStore and the strict root-binding that codexHomeFingerprint relies on:
// an empty value clears the root, but a non-empty home must be ABSOLUTE and must
// canonicalize (CanonicalizeCodexHome: existing directory, physically resolvable,
// not a reparse root). The canonical absolute form is stored — a relative input
// is rejected outright rather than resolved against the process working directory,
// so the root binding never depends on cwd. A home that cannot be canonicalized
// is an error and the previous root is kept.
func (s *Store) SetCodexHome(home string) error {
	if home == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.paths.CodexHome = ""
		return nil
	}
	if !filepath.IsAbs(home) {
		return &Error{Kind: KindConfigPathInvalid, Message: "codex home must be absolute"}
	}
	canonical, err := CanonicalizeCodexHome(home)
	if err != nil {
		return &Error{Kind: KindConfigPathInvalid, Message: "codex home cannot be canonicalized"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths.CodexHome = canonical
	return nil
}
