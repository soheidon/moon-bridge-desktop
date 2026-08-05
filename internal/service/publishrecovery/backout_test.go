package publishrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/recovery"
)

// faultFunc adapts a plain function to the FaultInjector interface.
type faultFunc func(FaultPoint) error

func (f faultFunc) Hit(p FaultPoint) error { return f(p) }

// faultNth returns an injector that errors on the nth hit of point only.
func faultNth(point FaultPoint, nth int) FaultInjector {
	var mu sync.Mutex
	counts := make(map[FaultPoint]int)
	return faultFunc(func(p FaultPoint) error {
		if p != point {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		counts[p]++
		if counts[p] == nth {
			return errors.New("fault injected")
		}
		return nil
	})
}

func storeWithFault(t *testing.T, dir string, fault FaultInjector) *Store {
	t.Helper()
	s, err := NewStore(Options{RecoveryDir: dir}, Dependencies{
		AtomicWrite: codexconfig.AtomicWrite,
		Remove:      os.Remove,
		RemoveAll:   os.RemoveAll,
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		NewID:       func() string { return testTransactionID },
		Fault:       fault,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// canonicalHome creates base/name as a real directory and returns its canonical
// form, matching the CreateBackout contract that TargetHome must already be
// canonical (absolute, existing, physically resolved).
func canonicalHome(t *testing.T, base, name string) string {
	t.Helper()
	home := filepath.Join(base, name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	canon, err := recovery.CanonicalizeCodexHome(home)
	if err != nil {
		t.Fatalf("canonicalize home: %v", err)
	}
	return canon
}

// backoutFixture builds a Store plus a target home and writes the three target
// files, returning a map of the original bytes per FileID. When a name is mapped
// to nil the file is intentionally left absent.
func backoutFixture(t *testing.T, present map[FileID][]byte) (*Store, string) {
	t.Helper()
	s := newTestStore(t, t.TempDir())
	home := canonicalHome(t, t.TempDir(), "codex-home")
	for id, data := range present {
		if data == nil {
			continue
		}
		path := filepath.Join(home, fileNameFor(id))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}
	return s, home
}

func txDirOf(s *Store) string { return filepath.Join(s.TransactionRoot(), testTransactionID) }

func TestCreateBackoutAllExist(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		FileAuth:          []byte(`{"tokens":["sk-secret"]}`),
		FileConfig:        []byte("openai_base_url=\"old\"\n"),
	}
	s, home := backoutFixture(t, orig)
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	if !hex64RE.MatchString(hash) {
		t.Fatalf("returned hash %q is not 64-hex", hash)
	}
	for _, id := range backoutOrder {
		backup, err := os.ReadFile(filepath.Join(txDirOf(s), backupFileNameFor(id)))
		if err != nil {
			t.Fatalf("backup %s: %v", id, err)
		}
		if string(backup) != string(orig[id]) {
			t.Fatalf("backup %s bytes differ from original", id)
		}
	}
	m, err := s.ReadBackout(context.Background(), testTransactionID, hash)
	if err != nil {
		t.Fatalf("ReadBackout: %v", err)
	}
	for i, id := range backoutOrder {
		if m.Entries[i].File != id || !m.Entries[i].PreviousExists {
			t.Fatalf("entry %d: expected %s existing, got %+v", i, id, m.Entries[i])
		}
		if m.Entries[i].SHA256 != sha256Hex(orig[id]) {
			t.Fatalf("entry %d hash mismatch", i)
		}
	}
	// Original targets are untouched.
	for id, data := range orig {
		cur, err := os.ReadFile(filepath.Join(home, fileNameFor(id)))
		if err != nil {
			t.Fatalf("re-read %s: %v", id, err)
		}
		if string(cur) != string(data) {
			t.Fatalf("target %s was modified", id)
		}
	}
}

func TestCreateBackoutMissingTargetRecordsAbsence(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		// auth.json intentionally absent (e.g. ServerToken empty).
		FileConfig: []byte("openai_base_url=\"old\"\n"),
	}
	s, home := backoutFixture(t, orig)
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(txDirOf(s), backupFileNameFor(FileAuth))); !os.IsNotExist(err) {
		t.Fatalf("an absent target must not produce a backup file")
	}
	m, err := s.ReadBackout(context.Background(), testTransactionID, hash)
	if err != nil {
		t.Fatalf("ReadBackout: %v", err)
	}
	for i, id := range backoutOrder {
		wantExist := id != FileAuth
		if m.Entries[i].PreviousExists != wantExist {
			t.Fatalf("entry %s previousExists=%v, want %v", id, m.Entries[i].PreviousExists, wantExist)
		}
		if wantExist && m.Entries[i].SHA256 == "" {
			t.Fatalf("entry %s is missing its hash", id)
		}
		if !wantExist && m.Entries[i].SHA256 != "" {
			t.Fatalf("entry %s must not carry a hash", id)
		}
	}
}

func TestCreateBackoutManifestBytesHashMatchesReturn(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		FileAuth:          []byte(`{"tokens":["sk-secret"]}`),
		FileConfig:        []byte("openai_base_url=\"old\"\n"),
	}
	s, home := backoutFixture(t, orig)
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(txDirOf(s), backoutManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if sha256Hex(data) != hash {
		t.Fatalf("on-disk manifest bytes hash to %s, returned %s", sha256Hex(data), hash)
	}
	// The exact bytes were parsed and validated.
	var m BackoutManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("on-disk manifest invalid: %v", err)
	}
	// No absolute path or raw secret in the serialized manifest.
	for _, needle := range []string{home, "sk-secret"} {
		if strings.Contains(string(data), needle) {
			t.Fatalf("manifest leaks %q", needle)
		}
	}
}

func TestCreateBackoutRejectsInvalidTransactionID(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	for _, id := range []string{"", "../escape", "C:\\publish", "a/b"} {
		if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
			TransactionID: id,
			TargetHome:    home,
		}); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("transactionID %q: expected transaction_invalid, got %v", id, err)
		}
	}
}

func TestCreateBackoutRejectsInvalidTargetHome(t *testing.T) {
	s, _ := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	for _, home := range []string{"", "relative/home", "."} {
		if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
			TransactionID: testTransactionID,
			TargetHome:    home,
		}); asErrorKind(err) != KindConfigPathInvalid {
			t.Fatalf("target home %q: expected config_path_invalid, got %v", home, err)
		}
	}
}

func TestCreateBackoutExistingTxDirRejectedAndUntouched(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		FileAuth:          []byte(`{"tokens":["sk-secret"]}`),
		FileConfig:        []byte("openai_base_url=\"old\"\n"),
	}
	s, home := backoutFixture(t, orig)
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); err != nil {
		t.Fatalf("first CreateBackout: %v", err)
	}
	// Plant evidence inside the existing transaction directory.
	sentinel := filepath.Join(txDirOf(s), "user-note.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); asErrorKind(err) != KindTransactionActive {
		t.Fatalf("expected transaction_active, got %v", err)
	}
	// The pre-existing directory and its contents are untouched.
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("existing transaction content was removed: %v", err)
	}
	backup, err := os.ReadFile(filepath.Join(txDirOf(s), backupFileNameFor(FileConfig)))
	if err != nil {
		t.Fatalf("existing backup was removed: %v", err)
	}
	if string(backup) != string(orig[FileConfig]) {
		t.Fatalf("existing backup was overwritten")
	}
}

func TestCreateBackoutFaultOnFirstBackupCleansUp(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		FileAuth:          []byte(`{"tokens":["sk-secret"]}`),
		FileConfig:        []byte("openai_base_url=\"old\"\n"),
	}
	dir := t.TempDir()
	home := canonicalHome(t, t.TempDir(), "codex-home")
	for id, data := range orig {
		if err := os.WriteFile(filepath.Join(home, fileNameFor(id)), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}
	s := storeWithFault(t, dir, faultNth(FaultBackoutWrite, 1))
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	if _, err := os.Stat(txDirOf(s)); !os.IsNotExist(err) {
		t.Fatalf("partial transaction directory left on disk: %v", err)
	}
	// The originals are never modified by a failed backout.
	for id, data := range orig {
		cur, err := os.ReadFile(filepath.Join(home, fileNameFor(id)))
		if err != nil {
			t.Fatalf("re-read %s: %v", id, err)
		}
		if string(cur) != string(data) {
			t.Fatalf("target %s was modified by failed backout", id)
		}
	}
}

func TestCreateBackoutFaultOnManifestWriteCleansUp(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		FileAuth:          []byte(`{"tokens":["sk-secret"]}`),
		FileConfig:        []byte("openai_base_url=\"old\"\n"),
	}
	dir := t.TempDir()
	home := canonicalHome(t, t.TempDir(), "codex-home")
	for id, data := range orig {
		if err := os.WriteFile(filepath.Join(home, fileNameFor(id)), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}
	// 3 existing targets fault the backups at hits 1-3; hit 4 is the manifest.
	s := storeWithFault(t, dir, faultNth(FaultBackoutWrite, 4))
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	if _, err := os.Stat(txDirOf(s)); !os.IsNotExist(err) {
		t.Fatalf("partial transaction directory left on disk: %v", err)
	}
}

func TestCreateBackoutRejectsSymlinkEscape(t *testing.T) {
	home := canonicalHome(t, t.TempDir(), "codex-home")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "auth.json"), []byte(`{"tokens":["sk-secret"]}`), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// The target file inside home is a symlink that resolves outside home. If the
	// environment forbids symlinks, the escape cannot be arranged; skip rather
	// than fail.
	if err := os.Symlink(filepath.Join(outside, "auth.json"), filepath.Join(home, "auth.json")); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "models_catalog.json"), []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("openai_base_url=\"old\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	s := newTestStore(t, t.TempDir())
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); asErrorKind(err) != KindConfigPathInvalid {
		t.Fatalf("expected config_path_invalid, got %v", err)
	}
	if _, err := os.Stat(txDirOf(s)); !os.IsNotExist(err) {
		t.Fatalf("transaction directory left on disk after escape rejection: %v", err)
	}
	// The symlink target outside home is never read or copied.
	if _, err := os.Stat(filepath.Join(outside, "auth.json")); err != nil {
		t.Fatalf("outside file was disturbed: %v", err)
	}
}

func TestReadBackoutRoundTrip(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		FileAuth:          []byte(`{"tokens":["sk-secret"]}`),
		FileConfig:        []byte("openai_base_url=\"old\"\n"),
	}
	s, home := backoutFixture(t, orig)
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	m1, err := s.ReadBackout(context.Background(), testTransactionID, hash)
	if err != nil {
		t.Fatalf("ReadBackout: %v", err)
	}
	// Idempotent: a second read returns the same result and mutates nothing.
	m2, err := s.ReadBackout(context.Background(), testTransactionID, hash)
	if err != nil {
		t.Fatalf("second ReadBackout: %v", err)
	}
	if m1.TransactionID != m2.TransactionID || len(m1.Entries) != len(m2.Entries) {
		t.Fatalf("idempotency violation: %+v vs %+v", m1, m2)
	}
}

func TestReadBackoutRejectsMalformedExpectedHash(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	// The expected manifest hash must match the ^[0-9a-f]{64}$ grammar: empty,
	// non-hex, wrong length, and uppercase are all rejected before any disk access.
	for _, h := range []string{
		"",
		"xyz",
		strings.Repeat("g", 64),
		strings.Repeat("a", 63),
		strings.ToUpper(strings.Repeat("a", 64)),
	} {
		if _, err := s.ReadBackout(context.Background(), testTransactionID, h); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("expected hash %q: expected transaction_invalid, got %v", h, err)
		}
	}
}

func TestReadBackoutRejectsInvalidTransactionID(t *testing.T) {
	s, _ := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	for _, id := range []string{"", "../escape", "C:\\publish"} {
		if _, err := s.ReadBackout(context.Background(), id, strings.Repeat("a", 64)); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("transactionID %q: expected transaction_invalid, got %v", id, err)
		}
	}
}

func TestReadBackoutMissingManifest(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	if err := os.Remove(filepath.Join(txDirOf(s), backoutManifestFileName)); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
}

func TestReadBackoutManifestTamperDetected(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	manifestPath := filepath.Join(txDirOf(s), backoutManifestFileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// Flip a byte that changes the on-disk hash.
	data[0] ^= 0x01
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatalf("tamper manifest: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); asErrorKind(err) != KindExternalModification {
		t.Fatalf("expected external_modification, got %v", err)
	}
}

func TestReadBackoutBackupTamperDetected(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	backupPath := filepath.Join(txDirOf(s), backupFileNameFor(FileConfig))
	if err := os.WriteFile(backupPath, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper backup: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); asErrorKind(err) != KindExternalModification {
		t.Fatalf("expected external_modification, got %v", err)
	}
}

func TestReadBackoutDeletedBackupDetected(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	if err := os.Remove(filepath.Join(txDirOf(s), backupFileNameFor(FileConfig))); err != nil {
		t.Fatalf("remove backup: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); asErrorKind(err) != KindExternalModification {
		t.Fatalf("expected external_modification, got %v", err)
	}
}

func TestReadBackoutTransactionMismatch(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	// A valid manifest for a different transaction replaces the real one; its
	// own bytes hash consistently, so only the ID mismatch can reject it.
	other := "22222222-2222-4222-8222-222222222222"
	m := testManifest([3]bool{false, false, true})
	m.TransactionID = other
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(txDirOf(s), backoutManifestFileName), data, 0o600); err != nil {
		t.Fatalf("write foreign manifest: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, sha256Hex(data)); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestReadBackoutMalformedManifestJSON(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	// Garbage that hashes to the passed expectation: the hash check passes, then
	// decode must reject it.
	garbage := []byte("{ not valid json ")
	if err := os.WriteFile(filepath.Join(txDirOf(s), backoutManifestFileName), garbage, 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, sha256Hex(garbage)); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
}

func TestBackoutHashFitsJournalContract(t *testing.T) {
	// The hash CreateBackout returns is exactly what the journal's
	// BackoutManifestSHA256 field accepts (64-hex), so backout_copied can be
	// recorded with it without further transformation.
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	j := journalFor(PhaseBackoutCopied, nil)
	j.BackoutManifestSHA256 = hash
	if err := j.Validate(); err != nil {
		t.Fatalf("returned hash does not satisfy the journal contract: %v", err)
	}
}

func TestCreateBackoutHonorsFaultOnlyForBackoutWrites(t *testing.T) {
	// A fault point other than FaultBackoutWrite must never fire during backout.
	s := storeWithFault(t, t.TempDir(), faultNth(FaultPoint("unrelated"), 1))
	home := canonicalHome(t, t.TempDir(), "codex-home")
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), []byte("x"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); err != nil {
		t.Fatalf("unrelated fault fired: %v", err)
	}
}

func TestReadBackoutInvalidManifestSchemaMapsToBackoutFailed(t *testing.T) {
	// A schema violation on disk (an empty entries array) is backout corruption,
	// not a caller error: it must map to backout_failed at the ReadBackout
	// boundary and must not leak the validation detail.
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	data := []byte(fmt.Sprintf(`{"schemaVersion":1,"transactionId":%q,"entries":[]}`, testTransactionID))
	if err := os.WriteFile(filepath.Join(txDirOf(s), backoutManifestFileName), data, 0o600); err != nil {
		t.Fatalf("write invalid manifest: %v", err)
	}
	_, err := s.ReadBackout(context.Background(), testTransactionID, sha256Hex(data))
	if asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	// The conversion must not leak the validation detail (here: the empty entries
	// array) into the error.
	if strings.Contains(err.Error(), "entries") {
		t.Fatalf("validation detail leaked into the error: %v", err)
	}
}

func TestReadBackoutRejectsUnexpectedBackupForAbsentEntry(t *testing.T) {
	// auth.json was absent when the backout was captured, so its backup must not
	// exist. A planted auth.json.backup is external modification, even though the
	// manifest says nothing about auth.
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(txDirOf(s), backupFileNameFor(FileAuth)), []byte("planted"), 0o600); err != nil {
		t.Fatalf("plant backup for absent target: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); asErrorKind(err) != KindExternalModification {
		t.Fatalf("expected external_modification, got %v", err)
	}
}

func TestReadBackoutRejectsStrayFile(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(txDirOf(s), "user-note.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("plant stray file: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); asErrorKind(err) != KindExternalModification {
		t.Fatalf("expected external_modification, got %v", err)
	}
}
