package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// v1SourcePath returns %APPDATA%\Moon Bridge Desktop\traffic-analysis
// \integration-state.json. The envFallback mirrors codexconfig.defaultBackupDir.
func v1SourcePath(env func(string) string) string {
	root := env("APPDATA")
	if root == "" {
		root = env("USERPROFILE")
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, "Moon Bridge Desktop", "traffic-analysis", "integration-state.json")
}

// legacyV1State is the on-disk shape of a legacy v1 file. The old Rust
// RecoveryState used the SAME struct for v1 and v2, distinguished only by
// schemaVersion, with the integration flag named `integrationActive` (see
// desktop/src-tauri/src/traffic_analysis.rs RecoveryState). Every field is read
// here so a v1 record migrates without losing data; a v1's own `migration`
// sub-object is superseded by the fresh v1→v2 migration metadata (a v1 file can
// never itself be a migration product, since migrations write v2).
type legacyV1State struct {
	SchemaVersion                int                     `json:"schemaVersion"`
	IntegrationActive            bool                    `json:"integrationActive"`
	Phase                        string                  `json:"phase"`
	OperationID                  string                  `json:"operationId"`
	ConfigPath                   string                  `json:"configPath"`
	PreviousOpenaiBaseURLPresent bool                    `json:"previousOpenaiBaseUrlPresent"`
	PreviousOpenaiBaseURL        *string                 `json:"previousOpenaiBaseUrl,omitempty"`
	AppliedOpenaiBaseURL         string                  `json:"appliedOpenaiBaseUrl"`
	ConfigHashBeforeApply        string                  `json:"configHashBeforeApply"`
	ConfigHashAfterApply         string                  `json:"configHashAfterApply"`
	BackupPath                   *string                 `json:"backupPath,omitempty"`
	StartedAt                    string                  `json:"startedAt"`
	UpdatedAt                    *string                 `json:"updatedAt,omitempty"`
	AutoLog                      *AutoLogRecoveryState   `json:"autoLog,omitempty"`
	AutoLogStatus                *string                 `json:"autoLogStatus,omitempty"`
	Migration                    *RecoveryMigrationState `json:"migration,omitempty"`
	UnsavedObservationsMayRemain bool                    `json:"unsavedObservationsMayRemain"`
	UnsavedDiscardConfirmed      bool                    `json:"unsavedDiscardConfirmed"`
	CaptureStateLastKnown        string                  `json:"captureStateLastKnown,omitempty"`
	RelayActiveLastKnown         bool                    `json:"relayActiveLastKnown"`
	ReconciliationStatus         *string                 `json:"reconciliationStatus,omitempty"`
	ReconciledAt                 *string                 `json:"reconciledAt,omitempty"`
	ReconciliationDetail         *string                 `json:"reconciliationDetail,omitempty"`
	RestartAttempted             bool                    `json:"restartAttempted"`
}

// Migrate migrates a legacy v1 integration-state (if present and no v2 exists)
// into the canonical v2 path, following the Rust migrate_recovery_state order:
// write v2 first, then archive the original v1 bytes. The v1 source is NEVER
// deleted, and re-running after a successful migration is a no-op because the
// v2 path exists (no archive proliferation).
//
// Contracts: only schemaVersion < 2 is migrated (>= 2 is rejected, never
// written); appliedOpenaiBaseUrl is never fabricated (empty stays empty); every
// path is sanitized before write so no absolute path reaches v2; unparseable v1
// is archived once (content-derived name) and still returns migration_failed.
// The post-migration archive of the v1 bytes is best-effort: after the v2 write
// is durable, an archive failure is a sanitized log warning and Migrate returns
// nil (the v1 source is never deleted, so nothing is lost; no retry — v2 exists).
// The whole migration runs under the Store mutex so concurrent Migrate calls
// never double-write or double-archive.
func (s *Store) Migrate(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path); err == nil {
		return nil // v2 already exists; never re-migrate
	} else if !os.IsNotExist(err) {
		return &Error{Kind: KindMigrationFailed, Message: "stat recovery state failed"}
	}
	v1 := v1SourcePath(s.env)
	if v1 == "" {
		return nil
	}
	orig, err := os.ReadFile(v1)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no v1, nothing to migrate
		}
		return &Error{Kind: KindMigrationFailed, Message: "read v1 recovery state failed"}
	}

	var v1State legacyV1State
	if err := json.Unmarshal(orig, &v1State); err != nil {
		// Unparseable v1: archive once under a content-derived name so the same
		// bytes are never double-archived on subsequent boots, then report
		// migration_failed. Archiving is not success; no v2 is written.
		if aerr := s.archiveV1Deterministic(ctx, orig); aerr != nil {
			return aerr
		}
		return &Error{Kind: KindMigrationFailed, Message: "unparseable legacy v1 recovery state"}
	}
	// schemaVersion absent (0) is treated as v1 (Rust read_recovery_state's
	// `schema_version < 2` check); 2+ belongs to the v2 contract, not migrated.
	if v1State.SchemaVersion >= 2 {
		return &Error{Kind: KindMigrationFailed, Message: "legacy v1 recovery state has unsupported schema version"}
	}

	phase := PhaseRecovered
	if v1State.IntegrationActive {
		phase = PhaseIntegrationApplied
	}
	now := time.Now().UTC().Format(time.RFC3339)

	migrated := State{
		SchemaVersion:                SchemaVersion,
		IntegrationActive:            v1State.IntegrationActive,
		Phase:                        phase,
		OperationID:                  v1State.OperationID,
		PreviousOpenaiBaseURLPresent: v1State.PreviousOpenaiBaseURLPresent,
		PreviousOpenaiBaseURL:        v1State.PreviousOpenaiBaseURL,
		AppliedOpenaiBaseURL:         v1State.AppliedOpenaiBaseURL, // never fabricated
		ConfigHashBeforeApply:        v1State.ConfigHashBeforeApply,
		ConfigHashAfterApply:         v1State.ConfigHashAfterApply,
		StartedAt:                    v1State.StartedAt,
		UpdatedAt:                    &now,
		AutoLog:                      v1State.AutoLog,
		AutoLogStatus:                v1State.AutoLogStatus,
		UnsavedObservationsMayRemain: v1State.UnsavedObservationsMayRemain,
		UnsavedDiscardConfirmed:      v1State.UnsavedDiscardConfirmed,
		CaptureStateLastKnown:        v1State.CaptureStateLastKnown,
		RelayActiveLastKnown:         v1State.RelayActiveLastKnown,
		ReconciliationStatus:         v1State.ReconciliationStatus,
		ReconciledAt:                 v1State.ReconciledAt,
		ReconciliationDetail:         v1State.ReconciliationDetail,
		RestartAttempted:             v1State.RestartAttempted,
		Migration: &RecoveryMigrationState{
			SourcePath:          relativeOrLogical(filepath.Join(filepath.Dir(v1), filepath.Base(v1)), s.paths.AppDataRoot),
			SourceSchemaVersion: v1State.SchemaVersion, // actual read value, not forced to 1
			MigratedAt:          now,
		},
	}

	// Path safety: never carry an absolute path into v2.
	rel, fp := s.migrationConfigPath(v1State.ConfigPath)
	migrated.ConfigPath = rel
	migrated.CodexHomeFingerprint = fp
	if v1State.ConfigPath != "" && rel == "" {
		// An absolute legacy configPath that cannot be bound to the current codex
		// home is cleared and diagnosed config_path_invalid. No absolute path is
		// stored; the evidence/recovery hint is the logical reconciliationDetail.
		migrated.ReconciliationStatus = strPtr(string(StatusConfigPathInvalid))
		migrated.ReconciledAt = &now
		migrated.ReconciliationDetail = strPtr("legacy config path could not be bound to the current codex home")
	}
	if v1State.BackupPath != nil {
		if r, ok := s.relativizeWithin(*v1State.BackupPath, s.paths.BackupDir); ok {
			migrated.BackupPath = &r
		}
		// else: an outside/unresolvable backupPath is dropped (nil), never stored.
	}
	if v1State.AutoLog != nil {
		al := *v1State.AutoLog
		if r, ok := s.relativizeWithin(al.Path, s.paths.TrafficLogDir); ok {
			al.Path = r
		} else {
			al.Path = "" // outside/unresolvable autoLog path is dropped
		}
		migrated.AutoLog = &al
	}

	if err := s.writeLockedMigrated(ctx, &migrated); err != nil {
		return &Error{Kind: KindMigrationFailed, Message: "write migrated recovery state failed"}
	}
	stamp := time.Now().UTC().Format("20060102T150405000Z")
	if err := s.archiveV1Named(ctx, orig, stamp); err != nil {
		// Best-effort archive (Rust-compatible contract): the v2 write is already
		// durable and the v1 source is never deleted, so an archive failure must not
		// surface as migration_failed — that would mislead the caller into treating
		// a completed migration as failed. Log a sanitized warning and do not retry:
		// the v2 path exists, so the next boot never re-migrates. No state/config
		// details (paths) are logged.
		log.Printf("recovery: archive of migrated v1 failed (best-effort, v2 is durable); skipping archive")
	}
	return nil
}

// migrationConfigPath converts a legacy v1 configPath for storage in v2. An
// absolute path physically inside the current CODEX_HOME is relativized and
// root-bound with codexHomeFingerprint; an already-relative value is kept and
// root-bound the same way (the new write contract requires a non-empty relative
// configPath to carry a matching fingerprint). Any value that cannot be
// root-bound — empty home, traversal, absolute outside the current home (lexical
// OR via a symlink/junction/reparse escape), or a home that cannot be
// canonicalized — yields ("","") so the caller diagnoses config_path_invalid.
// No absolute path is ever stored in v2.
func (s *Store) migrationConfigPath(abs string) (rel, fingerprint string) {
	if abs == "" {
		return "", ""
	}
	var relPath string
	if !filepath.IsAbs(abs) {
		relPath = filepath.Clean(abs)
		if isTraversalRelative(relPath) {
			return "", ""
		}
	} else {
		if s.paths.CodexHome == "" {
			return "", ""
		}
		// Lexical containment alone is not enough: a legacy absolute configPath
		// may sit under a directory that is a symlink/junction/reparse pointing
		// outside home. The physical location must stay inside home.
		if !pathWithinPhysical(s.paths.CodexHome, abs) {
			return "", ""
		}
		var err error
		relPath, err = ToRelative(s.paths.CodexHome, abs)
		if err != nil {
			return "", ""
		}
	}
	fp, err := s.fingerprintCodexHome()
	if err != nil {
		return "", ""
	}
	return relPath, fp
}

// relativizeWithin converts an absolute path to a root-relative one when it lies
// inside root (lexical). An already-relative value is cleaned and kept. Empty or
// outside-root values yield ok=false so callers drop the path rather than
// persisting an absolute form.
func (s *Store) relativizeWithin(abs, root string) (string, bool) {
	if abs == "" {
		return "", false
	}
	if !filepath.IsAbs(abs) {
		return filepath.Clean(abs), true
	}
	if root == "" {
		return "", false
	}
	if !pathWithin(root, abs) {
		return "", false
	}
	rel, err := ToRelative(root, abs)
	if err != nil {
		return "", false
	}
	return rel, true
}

// archiveV1Named stores the original v1 bytes (not the migrated v2) into
// recovery\migrated-v1 with the Rust-compatible name:
//   - integration-state-<stamp>.json
//   - integration-state-<stamp>-<suffix:03>.json on collision
//   - integration-state-<stamp>-overflow.json past suffix 999
func (s *Store) archiveV1Named(ctx context.Context, orig []byte, stamp string) error {
	dir := filepath.Join(filepath.Dir(s.path), "migrated-v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &Error{Kind: KindMigrationFailed, Message: "create migrated-v1 dir failed"}
	}
	base := filepath.Join(dir, "integration-state-"+stamp)
	path, err := uniqueMigrationArchivePath(base)
	if err != nil {
		return &Error{Kind: KindMigrationFailed, Message: "allocate migration archive path failed"}
	}
	return writeNewFileSyncDurable(path, dir, orig)
}

// archiveV1Deterministic archives an unparseable v1 under a content-derived name
// (migrated-v1/unparseable-<sha256[:16]>.json) so re-boots with the same bytes
// never create a second archive. No v2 is written for unparseable v1. An EEXIST
// from the Stat → O_EXCL window is idempotent success (same bytes already
// archived concurrently).
func (s *Store) archiveV1Deterministic(ctx context.Context, orig []byte) error {
	dir := filepath.Join(filepath.Dir(s.path), "migrated-v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &Error{Kind: KindMigrationFailed, Message: "create migrated-v1 dir failed"}
	}
	path := filepath.Join(dir, "unparseable-"+HashBytes(orig)[:16]+".json")
	if _, err := os.Stat(path); err == nil {
		return nil // same bytes already archived; do not double-archive
	} else if !os.IsNotExist(err) {
		return &Error{Kind: KindMigrationFailed, Message: "stat migration archive failed"}
	}
	if err := writeNewFileSyncDurable(path, dir, orig); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil // concurrently archived; idempotent success
		}
		return &Error{Kind: KindMigrationFailed, Message: "write migration archive failed"}
	}
	return nil
}

// uniqueMigrationArchivePath finds an unused archive path, trying <base>.json,
// <base>-<suffix>.json (001..999), then <base>-overflow.json, matching the Rust
// unique_migration_archive_path.
func uniqueMigrationArchivePath(baseNoExt string) (string, error) {
	if _, err := os.Stat(baseNoExt + ".json"); os.IsNotExist(err) {
		return baseNoExt + ".json", nil
	}
	for i := 1; i <= 999; i++ {
		cand := fmt.Sprintf("%s-%03d.json", baseNoExt, i)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand, nil
		}
	}
	return baseNoExt + "-overflow.json", nil
}

// writeNewFileSyncDurable writes bytes with O_CREATE|O_EXCL (never overwrites),
// fsyncs the file, and best-effort fsyncs the parent directory. The file fsync is
// the primary durability guarantee; directory fsync is best-effort only (Windows
// does not support directory fsync, so failures are ignored and do not block the
// archive write).
func writeNewFileSyncDurable(path, dir string, bytes []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(bytes); werr != nil {
		f.Close()
		return werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// relativeOrLogical stores migration.sourcePath as a logical id or APPDATA-
// relative path, never an arbitrary absolute path.
func relativeOrLogical(abs, appDataRoot string) string {
	if appDataRoot != "" {
		if rel, err := ToRelative(appDataRoot, abs); err == nil {
			return rel
		}
	}
	return filepath.Base(abs) // logical identifier
}

// migrationSourceLogicalID matches a safe logical identifier for
// migration.sourcePath: a single token with no path separators, no volume, and
// no leading dot. The values actually written by relativeOrLogical
// (integration-state.json / unparseable-v1 / legacy-integration-state or an
// AppDataRoot-relative path) all satisfy the allowlist or this grammar.
var migrationSourceLogicalID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validMigrationSource reports whether a stored migration.sourcePath is a safe
// logical identifier or an AppDataRoot-relative path. A logical identifier is a
// single token matching the safe grammar; a relative path is allowed separators
// but must not traverse (no `..`), must not be absolute, and must not carry a
// volume name. Arbitrary strings and absolute paths are rejected.
func validMigrationSource(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) {
		return false
	}
	if isTraversalRelative(p) {
		return false
	}
	if strings.ContainsAny(p, `\/`) {
		// AppDataRoot-relative form: separators are allowed, volumes are not.
		return !strings.Contains(p, ":")
	}
	if strings.Contains(p, ":") {
		return false
	}
	return migrationSourceLogicalID.MatchString(p)
}
