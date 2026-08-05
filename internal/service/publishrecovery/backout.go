package publishrecovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"moonbridge/internal/service/recovery"
)

// FaultBackoutWrite is a fault-injection seam hit before each durable backout
// write (each backup file and the manifest), so tests can abort mid-write and
// verify the partial transaction directory is cleaned up.
const FaultBackoutWrite = FaultPoint("publish_backout_write")

// CreateBackoutOptions identifies a transaction and the target home whose
// pre-publish bytes are captured.
type CreateBackoutOptions struct {
	TransactionID string
	TargetHome    string
}

// CreateBackout captures the pre-publish state of the three publish targets from
// TargetHome into a fresh transaction directory and durably writes the backout
// manifest. It returns the SHA-256 of the manifest bytes that were written, for
// the journal's BackoutManifestSHA256. It never overwrites an existing
// transaction directory; on any failure the partially created directory is
// removed so a retry can start clean. Backout bytes are captured as content
// hashes in the manifest only — no path, raw bytes, or secret is returned or
// logged.
//
// TargetHome must already be a canonical codex home path (as produced by
// recovery.CanonicalizeCodexHome): an absolute, existing, physically resolved
// directory with no symlink or junction at or above it. A non-canonical or
// reparse target home is rejected with KindConfigPathInvalid before any file is
// read, so pathWithinPhysical is never fooled into judging an external directory
// as inside the home.
//
// Ordering/durability contract: each existing target is copied into a
// <name>.backup file that is created with O_EXCL and fsynced before the next
// target is read, and only then is the manifest atomically written.
// codexconfig.AtomicWrite syncs the parent directory on Unix (rename + dir
// fsync; MoveFileExW with MOVEFILE_WRITE_THROUGH on Windows), so a successful
// manifest write makes the transaction directory entry durable and the
// previously created and fsynced backup entries are rediscoverable after a
// crash. The returned hash is only meaningful once AtomicWrite has returned nil.
func (s *Store) CreateBackout(ctx context.Context, opts CreateBackoutOptions) (string, error) {
	if err := ValidateTransactionID(opts.TransactionID); err != nil {
		return "", err
	}
	if opts.TargetHome == "" || !filepath.IsAbs(opts.TargetHome) {
		return "", newError(KindConfigPathInvalid, "target home must be an absolute path")
	}
	if err := validateTargetHome(opts.TargetHome); err != nil {
		return "", err
	}
	// The recovery root and the transaction root are verified level by level
	// before anything is written (real directory, not symlink/junction). The
	// transaction root is created with a single-level os.Mkdir, never MkdirAll,
	// so a junction placed anywhere in the parent chain fails the create instead
	// of being followed.
	if err := validateManagedDirectory(s.recoveryDir); err != nil {
		return "", newError(KindBackoutFailed, "recovery directory is not a safe directory")
	}
	if err := s.ensureTxRoot(); err != nil {
		return "", err
	}
	txDir, err := transactionRoot(s.txRoot, opts.TransactionID)
	if err != nil {
		return "", err
	}
	// Create the leaf with os.Mkdir (not MkdirAll) so a pre-existing directory
	// is detected atomically and never touched.
	if err := os.Mkdir(txDir, 0o700); err != nil {
		if os.IsExist(err) {
			// A real, pre-existing directory is an active transaction and is left
			// untouched. A symlink/junction/reparse at the path is not a
			// transaction: reject it without reading, following, or deleting it.
			if verr := validateManagedDirectory(txDir); verr != nil {
				return "", newError(KindBackoutFailed, "transaction directory is not a safe directory")
			}
			return "", newError(KindTransactionActive, "a transaction already exists")
		}
		return "", newError(KindBackoutFailed, "create transaction directory failed")
	}
	// Re-validate the directory we just created and the physical containment of
	// every level before writing into it, so a link or junction placed at the
	// path is never written through. This is a best-effort re-check, not a
	// handle-based guarantee: a concurrent same-user swap between this check and
	// the writes is outside Step 3B's guarantee. On failure the path is
	// deliberately not removed: RemoveAll would follow the link.
	if err := validateManagedDirectory(txDir); err != nil {
		return "", newError(KindBackoutFailed, "transaction directory is not a safe directory")
	}
	if !pathWithinPhysical(s.recoveryDir, s.txRoot) {
		return "", newError(KindBackoutFailed, "transactions root escapes the recovery directory")
	}
	if !pathWithinPhysical(s.txRoot, txDir) {
		return "", newError(KindBackoutFailed, "transaction directory escapes the transactions root")
	}
	cleanup := func() { s.deps.RemoveAll(txDir) }

	entries := make([]BackoutEntry, 0, len(backoutOrder))
	for _, id := range backoutOrder {
		target := filepath.Join(opts.TargetHome, fileNameFor(id))
		if !pathWithinPhysical(opts.TargetHome, target) {
			cleanup()
			return "", newError(KindConfigPathInvalid, "target file escapes the target home")
		}
		data, err := os.ReadFile(target)
		if err != nil {
			if os.IsNotExist(err) {
				entries = append(entries, BackoutEntry{File: id, PreviousExists: false})
				continue
			}
			cleanup()
			return "", newError(KindBackoutFailed, "read target file failed")
		}
		if err := s.deps.Fault.Hit(FaultBackoutWrite); err != nil {
			cleanup()
			return "", newError(KindBackoutFailed, "backout write fault")
		}
		if err := writeBackupExclusive(filepath.Join(txDir, backupFileNameFor(id)), data); err != nil {
			cleanup()
			return "", newError(KindBackoutFailed, "write backup file failed")
		}
		entries = append(entries, BackoutEntry{File: id, PreviousExists: true, SHA256: sha256Hex(data)})
	}

	m := &BackoutManifest{
		SchemaVersion: BackoutSchemaVersion,
		TransactionID: opts.TransactionID,
		Entries:       entries,
	}
	if err := m.Validate(); err != nil {
		cleanup()
		return "", err
	}
	manifestData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		cleanup()
		return "", newError(KindBackoutFailed, "encode backout manifest failed")
	}
	if err := s.deps.Fault.Hit(FaultBackoutWrite); err != nil {
		cleanup()
		return "", newError(KindBackoutFailed, "backout write fault")
	}
	if err := s.deps.AtomicWrite(filepath.Join(txDir, backoutManifestFileName), manifestData); err != nil {
		cleanup()
		return "", newError(KindBackoutFailed, "write backout manifest failed")
	}
	return sha256Hex(manifestData), nil
}

// writeBackupExclusive writes data to path with O_CREATE|O_EXCL so an existing
// file is never clobbered, then syncs it and closes. On any failure the partial
// file is removed.
func writeBackupExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// ensureTxRoot validates that the transaction root is a real directory, creating
// it with a single-level os.Mkdir when absent. It never uses os.MkdirAll, so a
// junction placed anywhere in the parent chain fails the create instead of being
// followed; an existing symlink/junction/non-directory is rejected untouched.
func (s *Store) ensureTxRoot() error {
	_, err := os.Lstat(s.txRoot)
	if err != nil && !os.IsNotExist(err) {
		return newError(KindBackoutFailed, "inspect transactions root failed")
	}
	if err == nil {
		// Existing path: must already be a real, safe directory.
		if verr := validateManagedDirectory(s.txRoot); verr != nil {
			return newError(KindBackoutFailed, "transactions root is not a safe directory")
		}
		return nil
	}
	// Absent: create it as a single level under the already-validated recovery
	// directory. EEXIST is tolerated for concurrent creation; the result is
	// re-validated so a link or junction placed at the root path is caught.
	if err := os.Mkdir(s.txRoot, 0o700); err != nil && !os.IsExist(err) {
		return newError(KindBackoutFailed, "create transactions root failed")
	}
	if err := validateManagedDirectory(s.txRoot); err != nil {
		return newError(KindBackoutFailed, "transactions root is not a safe directory")
	}
	return nil
}

// ReadBackout loads and verifies a transaction's backout data. expectedSHA256 is
// the journal's BackoutManifestSHA256, a 64-hex sha256: the on-disk manifest
// bytes must hash to it (manifest tamper detection), and each existing backup
// file must hash to its manifest entry (backup tamper detection). The recovery
// root, transaction root, and transaction directory are verified as real
// directories and their physical containment confirmed right before the manifest
// is read. The transaction directory must contain exactly the manifest and the
// backups referenced by existing entries: strict mode rejects a backup present
// for an absent target, or any other stray file, as external modification.
// Reading is idempotent and never modifies the transaction directory.
func (s *Store) ReadBackout(ctx context.Context, transactionID string, expectedSHA256 string) (*BackoutManifest, error) {
	if err := ValidateTransactionID(transactionID); err != nil {
		return nil, err
	}
	if !hex64RE.MatchString(expectedSHA256) {
		return nil, newError(KindTransactionInvalid, "expected manifest hash must be a sha256 hex")
	}
	// The recovery root, transaction root, and transaction directory are each
	// verified to be real directories before any read, and their physical
	// containment re-verified right before the manifest is read: a transaction
	// directory swapped for a junction since creation must never be read through.
	if err := validateManagedDirectory(s.recoveryDir); err != nil {
		return nil, newError(KindBackoutFailed, "recovery directory is not a safe directory")
	}
	if err := validateManagedDirectory(s.txRoot); err != nil {
		return nil, newError(KindBackoutFailed, "transactions root is not a safe directory")
	}
	txDir, err := transactionRoot(s.txRoot, transactionID)
	if err != nil {
		return nil, err
	}
	if err := validateManagedDirectory(txDir); err != nil {
		return nil, newError(KindBackoutFailed, "transaction directory is not a safe directory")
	}
	if !pathWithinPhysical(s.recoveryDir, s.txRoot) {
		return nil, newError(KindBackoutFailed, "transactions root escapes the recovery directory")
	}
	if !pathWithinPhysical(s.txRoot, txDir) {
		return nil, newError(KindBackoutFailed, "transaction directory escapes the transactions root")
	}
	data, err := os.ReadFile(filepath.Join(txDir, backoutManifestFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, newError(KindBackoutFailed, "backout manifest not found")
		}
		return nil, newError(KindBackoutFailed, "read backout manifest failed")
	}
	if sha256Hex(data) != expectedSHA256 {
		return nil, newError(KindExternalModification, "backout manifest hash mismatch")
	}
	var m BackoutManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, newError(KindBackoutFailed, "decode backout manifest failed")
	}
	if err := m.Validate(); err != nil {
		// A schema violation on disk is backout corruption, not a caller error:
		// convert it at the boundary without leaking the validation detail.
		return nil, newError(KindBackoutFailed, "backout manifest is invalid")
	}
	if m.TransactionID != transactionID {
		return nil, newError(KindTransactionInvalid, "backout manifest transaction mismatch")
	}
	for _, e := range m.Entries {
		if !e.PreviousExists {
			continue
		}
		backupData, err := os.ReadFile(filepath.Join(txDir, backupFileNameFor(e.File)))
		if err != nil {
			return nil, newError(KindExternalModification, "backup file missing or unreadable")
		}
		if sha256Hex(backupData) != e.SHA256 {
			return nil, newError(KindExternalModification, "backup file hash mismatch")
		}
	}
	if err := verifyNoSurplusFiles(txDir, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// DeleteBackout removes a transaction's backout directory. The recovery root,
// transaction root, and the leaf derived from the validated transactionID are
// each verified to be real directories (not symlinks/junctions), their physical
// containment is re-confirmed, and the leaf path is confirmed to be derived from
// the transactionID — so a directory swapped for a junction since creation is
// never removed through, and no stray path is ever passed to RemoveAll. An
// absent transaction root or directory is idempotent success. Callers (notably
// transaction.go) must go through this API instead of calling RemoveAll with a
// hand-built path, so cleanup stays behind the same reparse safety boundary as
// CreateBackout and ReadBackout.
func (s *Store) DeleteBackout(ctx context.Context, transactionID string) error {
	if err := ValidateTransactionID(transactionID); err != nil {
		return err
	}
	if err := validateManagedDirectory(s.recoveryDir); err != nil {
		return newError(KindBackoutFailed, "recovery directory is not a safe directory")
	}
	if _, err := os.Lstat(s.txRoot); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return newError(KindBackoutFailed, "inspect transactions root failed")
	}
	if err := validateManagedDirectory(s.txRoot); err != nil {
		return newError(KindBackoutFailed, "transactions root is not a safe directory")
	}
	if !pathWithinPhysical(s.recoveryDir, s.txRoot) {
		return newError(KindBackoutFailed, "transactions root escapes the recovery directory")
	}
	txDir, err := transactionRoot(s.txRoot, transactionID)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(txDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return newError(KindBackoutFailed, "inspect transaction directory failed")
	}
	if err := validateManagedDirectory(txDir); err != nil {
		return newError(KindBackoutFailed, "transaction directory is not a safe directory")
	}
	if !pathWithinPhysical(s.txRoot, txDir) {
		return newError(KindBackoutFailed, "transaction directory escapes the transactions root")
	}
	if err := s.deps.RemoveAll(txDir); err != nil {
		return newError(KindBackoutFailed, "remove transaction directory failed")
	}
	return nil
}

// verifyNoSurplusFiles enforces strict transaction-directory contents: the only
// files allowed are the manifest and the backups referenced by a PreviousExists
// entry. A backup present for an absent target, or any other stray file, means
// the transaction directory was modified externally and is rejected.
func verifyNoSurplusFiles(txDir string, m *BackoutManifest) error {
	entries, err := os.ReadDir(txDir)
	if err != nil {
		return newError(KindBackoutFailed, "inspect transaction directory failed")
	}
	expected := map[string]bool{backoutManifestFileName: true}
	for _, e := range m.Entries {
		if e.PreviousExists {
			expected[backupFileNameFor(e.File)] = true
		}
	}
	for _, de := range entries {
		if !expected[de.Name()] {
			return newError(KindExternalModification, "unexpected file in transaction directory")
		}
	}
	return nil
}

// validateTargetHome enforces CreateBackout's TargetHome contract: the input
// must already be a canonical codex home path — absolute, existing directory,
// physically resolved, with no symlink or junction at the root. A junction or
// symlink target home is rejected before any file is read, so pathWithinPhysical
// is never fooled into judging an external directory as inside the home.
func validateTargetHome(home string) error {
	canonical, err := recovery.CanonicalizeCodexHome(home)
	if err != nil {
		return newError(KindConfigPathInvalid, "target home is not a valid codex home")
	}
	clean := filepath.Clean(home)
	if runtime.GOOS == "windows" {
		clean = strings.ToLower(clean)
	}
	if clean != canonical {
		return newError(KindConfigPathInvalid, "target home must be a canonical codex home path")
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
