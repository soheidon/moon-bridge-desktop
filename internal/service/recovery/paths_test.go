package recovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestPathWithinPhysicalRejectsSymlinkEscape verifies a symlinked target that
// points outside root is rejected. Symlink creation requires privileges on some
// systems; when it fails the test is skipped rather than failed.
func TestPathWithinPhysicalRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}
	// Target physically resolves to `outside`, which is not under root.
	if pathWithinPhysical(root, filepath.Join(link, "config.toml")) {
		t.Fatal("symlink escape must be rejected")
	}
	// A plain path inside root is accepted.
	if !pathWithinPhysical(root, filepath.Join(root, "config.toml")) {
		t.Fatal("inside-root path must be accepted")
	}
}

// TestPathWithinPhysicalRejectsTargetFileSymlinkEscape covers the leaf-file case:
// a target whose final component is a symlink/junction to a FILE outside root is
// rejected because the target itself is EvalSymlinks'd when it exists (not just
// its parent directory).
func TestPathWithinPhysicalRejectsTargetFileSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "real.toml")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked.toml")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}
	if pathWithinPhysical(root, link) {
		t.Fatal("leaf symlink escape must be rejected")
	}
	// A plain (non-existent) path inside root is accepted.
	if !pathWithinPhysical(root, filepath.Join(root, "plain.toml")) {
		t.Fatal("plain inside-root file must be accepted")
	}
}

func TestHashFileDistinguishesMissingFromUnreadable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.toml")
	if _, err := HashFile(missing); !os.IsNotExist(err) {
		t.Fatalf("HashFile(missing) must report os.IsNotExist, got %v", err)
	}
	p := filepath.Join(t.TempDir(), "present.toml")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := HashFile(p)
	if err != nil {
		t.Fatalf("HashFile(present): %v", err)
	}
	if len(sum) != 64 {
		t.Fatalf("HashFile returned %d-char hex, want 64", len(sum))
	}
	// Deterministic: same content -> same hash.
	sum2, _ := HashFile(p)
	if sum != sum2 {
		t.Fatal("HashFile not deterministic")
	}
}

// TestStoreWriteNormalizationFailureKeepsOldState verifies that when a write is
// rejected (outside-root path), the previously persisted state is untouched.
func TestStoreWriteNormalizationFailureKeepsOldState(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	good := stateWithPhase(t, s, PhaseIntegrationApplied)
	if err := s.Write(ctx, good); err != nil {
		t.Fatalf("seed good write: %v", err)
	}
	before, _ := os.ReadFile(s.Path())
	bad := stateWithPhase(t, s, PhaseIntegrationApplied)
	bad.ConfigPath = filepath.Join(filepath.Dir(filepath.Dir(t.TempDir())), "config.toml") // outside home
	if err := s.Write(ctx, bad); err == nil {
		t.Fatal("write with outside-root configPath must be rejected")
	}
	after, _ := os.ReadFile(s.Path())
	if string(before) != string(after) {
		t.Fatal("rejected write must not alter the persisted state")
	}
}

// stripVerbatimPrefix converts the two Windows extended-length forms that denote
// an ordinary file — `\\?\C:\...` and `\\?\UNC\server\share\...` — back to their
// canonical DOS/UNC spelling. These are pure string checks valid on every OS.
func TestStripVerbatimPrefixDrive(t *testing.T) {
	got := stripVerbatimPrefix(`\\?\C:\Users\sohei\.codex\config.toml`)
	want := `C:\Users\sohei\.codex\config.toml`
	if got != want {
		t.Fatalf("strip = %q, want %q", got, want)
	}
}

func TestStripVerbatimPrefixUNCPreservesShare(t *testing.T) {
	got := stripVerbatimPrefix(`\\?\UNC\server\share\config.toml`)
	want := `\\server\share\config.toml`
	if got != want {
		t.Fatalf("strip = %q, want %q", got, want)
	}
}

func TestStripVerbatimPrefixUNCMatchesCaseInsensitively(t *testing.T) {
	got := stripVerbatimPrefix(`\\?\unc\server\share\config.toml`)
	want := `\\server\share\config.toml`
	if got != want {
		t.Fatalf("strip = %q, want %q", got, want)
	}
}

func TestStripVerbatimPrefixLeavesOrdinaryPaths(t *testing.T) {
	for _, p := range []string{
		`C:\Users\sohei\.codex\config.toml`,
		`config.toml`,
		`./config.toml`,
		`/home/sohei/.codex/config.toml`,
		``,
	} {
		if got := stripVerbatimPrefix(p); got != p {
			t.Fatalf("strip(%q) = %q, want unchanged", p, got)
		}
	}
}

func TestStripVerbatimPrefixLeavesUnsupportedNamespaces(t *testing.T) {
	for _, p := range []string{
		`\\?\Volume{01234567-89ab-cdef-0123-456789abcdef}\config.toml`,
		`\\?\pipe\moonbridge`,
		`\\.\pipe\moonbridge`,
	} {
		if got := stripVerbatimPrefix(p); got != p {
			t.Fatalf("strip(%q) = %q, want unchanged", p, got)
		}
	}
}
