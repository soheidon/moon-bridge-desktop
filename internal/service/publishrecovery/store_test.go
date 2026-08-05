package publishrecovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/service/codexconfig"
)

func newTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := NewStore(Options{RecoveryDir: dir}, Dependencies{
		AtomicWrite: codexconfig.AtomicWrite,
		Remove:      os.Remove,
		RemoveAll:   os.RemoveAll,
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		NewID:       func() string { return testTransactionID },
		Fault:       NoopFaultInjector{},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestNewStoreRequiresAbsoluteRecoveryDir(t *testing.T) {
	if _, err := NewStore(Options{}, Dependencies{}); asErrorKind(err) != KindJournalWriteFailed {
		t.Fatalf("empty root: expected journal_write_failed, got %v", err)
	}
	if _, err := NewStore(Options{RecoveryDir: "relative"}, Dependencies{}); asErrorKind(err) != KindJournalWriteFailed {
		t.Fatalf("relative root: expected journal_write_failed, got %v", err)
	}
}

func TestStoreLoadMissingReturnsNil(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	j, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j != nil {
		t.Fatalf("expected nil journal, got %+v", j)
	}
}

func TestStoreWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	want := journalFor(PhaseCatalogPublished, nil)
	if err := s.Write(context.Background(), want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TransactionID != want.TransactionID || got.Phase != want.Phase {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, want)
	}
	if len(got.PublishedFiles) != 1 || got.PublishedFiles[0] != FileModelsCatalog {
		t.Fatalf("unexpected publishedFiles: %v", got.PublishedFiles)
	}
}

func TestStoreWriteInvalidDoesNotChangeExistingJournal(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	valid := journalFor(PhaseCatalogPublished, nil)
	if err := s.Write(context.Background(), valid); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before, err := os.ReadFile(s.JournalPath())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	bad := journalFor(PhaseCatalogPublished, nil)
	bad.SchemaVersion = 2
	if err := s.Write(context.Background(), bad); asErrorKind(err) != KindJournalWriteFailed {
		t.Fatalf("expected journal_write_failed, got %v", err)
	}
	after, err := os.ReadFile(s.JournalPath())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("invalid write changed the existing journal")
	}

	// An Update that mutates into an invalid journal must also leave the file alone.
	if err := s.Update(context.Background(), func(cur *Journal) error {
		cur.Phase = Phase("bogus")
		return nil
	}); asErrorKind(err) != KindJournalWriteFailed {
		t.Fatalf("expected journal_write_failed from Update, got %v", err)
	}
	after, err = os.ReadFile(s.JournalPath())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("invalid update changed the existing journal")
	}
}

func TestStoreLoadRejectsInvalidJournalBytes(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	if err := os.WriteFile(s.JournalPath(), []byte(`{"schemaVersion":2}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Load(context.Background()); asErrorKind(err) != KindJournalParseFailed {
		t.Fatalf("expected journal_parse_failed, got %v", err)
	}
	if err := os.WriteFile(s.JournalPath(), []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Load(context.Background()); asErrorKind(err) != KindJournalParseFailed {
		t.Fatalf("expected journal_parse_failed, got %v", err)
	}
}

func TestStoreUpdate(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	if err := s.Write(context.Background(), journalFor(PhasePrepared, nil)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Update(context.Background(), func(cur *Journal) error {
		cur.Phase = PhaseBackoutCopied
		cur.BackoutManifestSHA256 = strings.Repeat("e", 64)
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != PhaseBackoutCopied {
		t.Fatalf("expected backout_copied, got %s", got.Phase)
	}
}

func TestStoreDeleteIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	if err := s.Write(context.Background(), journalFor(PhasePrepared, nil)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(s.JournalPath()); !os.IsNotExist(err) {
		t.Fatalf("journal still exists: %v", err)
	}
	if err := s.Delete(context.Background()); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestStoreWriteRejectsNil(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	if err := s.Write(context.Background(), nil); asErrorKind(err) != KindJournalWriteFailed {
		t.Fatalf("expected journal_write_failed, got %v", err)
	}
}

func TestStoreWriteSurfacesAtomicWriteFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(Options{RecoveryDir: dir}, Dependencies{
		AtomicWrite: func(path string, data []byte) error { return os.ErrPermission },
		Remove:      os.Remove,
		RemoveAll:   os.RemoveAll,
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		NewID:       func() string { return testTransactionID },
		Fault:       NoopFaultInjector{},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Write(context.Background(), journalFor(PhasePrepared, nil)); asErrorKind(err) != KindJournalWriteFailed {
		t.Fatalf("expected journal_write_failed, got %v", err)
	}
}

func TestJournalBytesContainNoAbsolutePathOrSecret(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	if err := s.Write(context.Background(), journalFor(PhaseConfigPublished, nil)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(s.JournalPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The journal references the home by fingerprint, never by path.
	if strings.Contains(string(data), dir) {
		t.Fatalf("journal leaks the recovery root path")
	}
	// The journal can never carry a raw secret; its only content fields are
	// SHA-256 hashes and logical IDs.
	if strings.Contains(string(data), "sk-SUPER-SECRET-TOKEN") {
		t.Fatalf("journal leaks a raw secret")
	}
}

func TestStoreConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	if err := s.Write(context.Background(), journalFor(PhasePrepared, nil)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				if err := s.Update(context.Background(), func(cur *Journal) error {
					cur.UpdatedAt = "2026-08-06T00:00:02Z"
					return nil
				}); err != nil {
					t.Errorf("Update: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != PhasePrepared {
		t.Fatalf("unexpected phase %s", got.Phase)
	}
	if got.UpdatedAt != "2026-08-06T00:00:02Z" {
		t.Fatalf("unexpected updatedAt %q", got.UpdatedAt)
	}
}

// compile-time check: the journal file path is under RecoveryDir with the fixed
// name, and the transaction root is under RecoveryDir/publish-transactions.
func TestStorePaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recovery")
	s := newTestStore(t, dir)
	if want := filepath.Join(dir, "codex-home-publish-journal.json"); s.JournalPath() != want {
		t.Fatalf("JournalPath = %q, want %q", s.JournalPath(), want)
	}
	if want := filepath.Join(dir, "publish-transactions"); s.TransactionRoot() != want {
		t.Fatalf("TransactionRoot = %q, want %q", s.TransactionRoot(), want)
	}
}

func TestStoreErrorsDoNotLeakValidationDetails(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)

	// An invalid journal on disk surfaces as a boundary parse error with no
	// internal validation details.
	if err := os.WriteFile(s.JournalPath(), []byte(`{"schemaVersion":2}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var te *Error
	if _, err := s.Load(context.Background()); !errors.As(err, &te) {
		t.Fatalf("expected typed Error, got %v", err)
	}
	if te.Kind != KindJournalParseFailed {
		t.Fatalf("expected journal_parse_failed, got %s", te.Kind)
	}
	if len(te.Details) != 0 {
		t.Fatalf("load validation details leaked: %v", te.Details)
	}

	// An invalid Write surfaces as a boundary write error, again without details.
	bad := journalFor(PhasePrepared, nil)
	bad.SchemaVersion = 2
	if err := s.Write(context.Background(), bad); !errors.As(err, &te) {
		t.Fatalf("expected typed Error, got %v", err)
	}
	if te.Kind != KindJournalWriteFailed {
		t.Fatalf("expected journal_write_failed, got %s", te.Kind)
	}
	if len(te.Details) != 0 {
		t.Fatalf("write validation details leaked: %v", te.Details)
	}
}
