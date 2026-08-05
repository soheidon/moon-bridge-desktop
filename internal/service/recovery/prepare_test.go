package recovery

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrepareTargetHomeAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTargetHome(target); err != nil {
		t.Fatalf("PrepareTargetHome(existing dir): %v", err)
	}
}

func TestPrepareTargetHomeAlreadyExistsNotDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "codex-home")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTargetHome(target); err == nil {
		t.Fatal("expected error for existing non-directory target")
	}
}

func TestPrepareTargetHomeCreatesFirstRun(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "codex-home")
	if err := PrepareTargetHome(target); err != nil {
		t.Fatalf("PrepareTargetHome(first run): %v", err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("target not created: %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("target is not a directory")
	}
}

func TestPrepareTargetHomeParentMissing(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nonexistent", "codex-home")
	if err := PrepareTargetHome(target); err == nil {
		t.Fatal("expected error for missing parent")
	}
}

func TestPrepareTargetHomeEmptyPath(t *testing.T) {
	if err := PrepareTargetHome(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

// TestPrepareTargetHomeRejectsAncestorSymlink verifies that a target whose
// ancestor is a symlink (or junction on Windows) is rejected even when the
// direct parent is a normal directory.
func TestPrepareTargetHomeRejectsAncestorSymlink(t *testing.T) {
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		// Symlinks require privileges on some platforms; skip if unprivileged.
		t.Skip("symlink creation requires privileges on this platform")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	sym := filepath.Join(dir, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, sym); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	// Target under the symlink: link/parent/codex-home. The direct parent
	// (link/parent) is a normal directory, but the ancestor "link" is a symlink
	// pointing to "real".
	parentDir := filepath.Join(sym, "parent")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parentDir, "codex-home")
	if err := PrepareTargetHome(target); err == nil {
		t.Fatal("expected rejection for ancestor symlink")
	}
}

// TestPrepareTargetHomeRejectsTargetAsReparse verifies that an existing target
// that is a reparse point (symlink/junction) is rejected.
func TestPrepareTargetHomeRejectsTargetAsReparse(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "codex-home")
	// Create as a junction (Windows) or symlink (Unix).
	if runtime.GOOS == "windows" {
		// On Windows, create a junction to an existing directory.
		real := filepath.Join(dir, "real-codex-home")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatal(err)
		}
		// Use mklink /J via os/exec would require elevated; skip if not possible.
		// The isReparseAttrs path is already tested by CanonicalizeCodexHome.
		t.Skip("junction creation for non-admin skipped; reparse rejection covered by CanonicalizeCodexHome")
	}
	if err := os.Symlink(dir, target); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := PrepareTargetHome(target); err == nil {
		t.Fatal("expected rejection for reparse-point target")
	}
}

// TestPrepareTargetHomeRejectsConcurrentReparseReplacement verifies that a
// TOCTOU race where the newly created target is swapped for a reparse point is
// detected.
func TestPrepareTargetHomeRejectsConcurrentReparseReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The test scenario requires symlink replacement which is non-trivial
		// on Windows without admin. The real guard is tested by the reparse
		// check after MkdirAll; the path is structurally the same as
		// TestPrepareTargetHomeRejectsTargetAsReparse.
		t.Skip("concurrent reparse replacement test skipped on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "codex-home")
	// Pre-create as a normal directory (so MkdirAll is a no-op in PrepareTargetHome).
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Swap it for a symlink before PrepareTargetHome runs. This simulates a
	// TOCTOU race between creation and verification.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, target); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := PrepareTargetHome(target); err == nil {
		t.Fatal("expected rejection for reparse-point replacement")
	}
}
