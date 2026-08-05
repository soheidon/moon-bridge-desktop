package publishrecovery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// linkDir creates a directory link at linkPath that redirects to target. On
// Windows a junction is preferred (no elevation required), otherwise a directory
// symlink is used. Tests skip when the environment forbids directory links.
func linkDir(linkPath, target string) error {
	if runtime.GOOS == "windows" {
		if err := exec.Command("cmd.exe", "/C", "mklink", "/J", linkPath, target).Run(); err == nil {
			return nil
		}
	}
	return os.Symlink(target, linkPath)
}

func TestValidateManagedDirectory(t *testing.T) {
	base := t.TempDir()

	// A normal directory passes.
	if err := validateManagedDirectory(base); err != nil {
		t.Fatalf("real directory rejected: %v", err)
	}
	// A missing path fails.
	if err := validateManagedDirectory(filepath.Join(base, "missing")); err == nil {
		t.Fatalf("missing directory accepted")
	}
	// A regular file fails.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateManagedDirectory(file); err == nil {
		t.Fatalf("regular file accepted as a directory")
	}
	// A link to a directory fails (junction on Windows, symlink elsewhere).
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := linkDir(filepath.Join(base, "link"), target); err != nil {
		t.Skipf("directory link not permitted: %v", err)
	}
	if err := validateManagedDirectory(filepath.Join(base, "link")); err == nil {
		t.Fatalf("directory link accepted")
	}
}

func TestCreateBackoutRejectsRecoveryDirLink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "recovery")
	if err := linkDir(link, real); err != nil {
		t.Skipf("directory link not permitted: %v", err)
	}
	home := canonicalHome(t, base, "codex-home")
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, link)
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	// Nothing was written through the link into real.
	if entries, err := os.ReadDir(real); err != nil {
		t.Fatalf("read link target: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("recovery dir link target was written through: %v", entries)
	}
}

func TestCreateBackoutRejectsTxRootLink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "publish-transactions-real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-place a link where the transaction root is expected.
	if err := linkDir(filepath.Join(base, "publish-transactions"), real); err != nil {
		t.Skipf("directory link not permitted: %v", err)
	}
	home := canonicalHome(t, base, "codex-home")
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, base)
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	if entries, err := os.ReadDir(real); err != nil {
		t.Fatalf("read link target: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("tx root link target was written through: %v", entries)
	}
}

func TestCreateBackoutRejectsTxDirLink(t *testing.T) {
	base := t.TempDir()
	txRoot := filepath.Join(base, "publish-transactions")
	if err := os.MkdirAll(txRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "tx-real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-place a link at the transaction directory path.
	if err := linkDir(filepath.Join(txRoot, testTransactionID), real); err != nil {
		t.Skipf("directory link not permitted: %v", err)
	}
	home := canonicalHome(t, base, "codex-home")
	if err := os.WriteFile(filepath.Join(home, fileNameFor(FileConfig)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, base)
	// A link at the transaction path is not an active transaction: it is
	// rejected, never read, followed, or deleted.
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	}); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed, got %v", err)
	}
	if entries, err := os.ReadDir(real); err != nil {
		t.Fatalf("read link target: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("tx dir link target was touched: %v", entries)
	}
}

func TestCreateBackoutRejectsTargetHomeLink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "codex-home-real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, fileNameFor(FileConfig)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "codex-home")
	if err := linkDir(link, real); err != nil {
		t.Skipf("directory link not permitted: %v", err)
	}
	s := newTestStore(t, t.TempDir())
	// A link at the target home itself is rejected before any file is read, even
	// though the linked directory is a real, existing home.
	if _, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    link,
	}); asErrorKind(err) != KindConfigPathInvalid {
		t.Fatalf("expected config_path_invalid, got %v", err)
	}
	// Nothing was created or read through the link.
	if _, err := os.Stat(txDirOf(s)); !os.IsNotExist(err) {
		t.Fatalf("transaction directory created for a link target home: %v", err)
	}
}

func TestReadBackoutRejectsTxDirLink(t *testing.T) {
	s, home := backoutFixture(t, map[FileID][]byte{FileConfig: []byte("x")})
	hash, err := s.CreateBackout(context.Background(), CreateBackoutOptions{
		TransactionID: testTransactionID,
		TargetHome:    home,
	})
	if err != nil {
		t.Fatalf("CreateBackout: %v", err)
	}
	// The backout reads normally before the swap.
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); err != nil {
		t.Fatalf("ReadBackout before swap: %v", err)
	}
	// Swap the transaction directory for a link to its own content: even though
	// the content is reachable through the link, the read must refuse to follow
	// it.
	txDir := txDirOf(s)
	moved := txDir + "-moved"
	if err := os.Rename(txDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := linkDir(txDir, moved); err != nil {
		t.Skipf("directory link not permitted: %v", err)
	}
	if _, err := s.ReadBackout(context.Background(), testTransactionID, hash); asErrorKind(err) != KindBackoutFailed {
		t.Fatalf("expected backout_failed for swapped tx dir, got %v", err)
	}
}

func TestCreateBackoutConcurrent(t *testing.T) {
	orig := map[FileID][]byte{
		FileModelsCatalog: []byte(`{"models":[]}`),
		FileAuth:          []byte(`{"tokens":["sk-secret"]}`),
		FileConfig:        []byte("openai_base_url=\"old\"\n"),
	}
	s, home := backoutFixture(t, orig)
	const n = 8
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.CreateBackout(context.Background(), CreateBackoutOptions{
				TransactionID: testTransactionID,
				TargetHome:    home,
			})
		}(i)
	}
	wg.Wait()

	var ok, active int
	for i := 0; i < n; i++ {
		switch asErrorKind(errs[i]) {
		case "":
			ok++
		case KindTransactionActive:
			active++
		default:
			t.Fatalf("goroutine %d: unexpected error %v", i, errs[i])
		}
	}
	if ok != 1 {
		t.Fatalf("expected exactly one success, got %d", ok)
	}
	if active != n-1 {
		t.Fatalf("expected %d transaction_active, got %d", n-1, active)
	}
	// The winner's backout is complete and readable.
	var winnerHash string
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			winnerHash = results[i]
		}
	}
	m, err := s.ReadBackout(context.Background(), testTransactionID, winnerHash)
	if err != nil {
		t.Fatalf("ReadBackout of concurrent winner: %v", err)
	}
	if len(m.Entries) != len(backoutOrder) {
		t.Fatalf("unexpected entry count %d", len(m.Entries))
	}
	for i, id := range backoutOrder {
		if m.Entries[i].File != id || !m.Entries[i].PreviousExists {
			t.Fatalf("entry %d: expected %s existing, got %+v", i, id, m.Entries[i])
		}
	}
}
