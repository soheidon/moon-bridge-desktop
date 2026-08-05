package recovery

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCanonicalizeCodexHomeRequiresExistingDirectory(t *testing.T) {
	if _, err := CanonicalizeCodexHome(""); err == nil {
		t.Fatal("empty home must error")
	}
	if _, err := CodexHomeFingerprint(""); err == nil {
		t.Fatal("empty home fingerprint must error")
	}
	if _, err := CanonicalizeCodexHome(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a non-existent home must error (never fingerprint an unresolved path)")
	}
	if _, err := CodexHomeFingerprint(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("fingerprint of a non-existent home must error")
	}
	// A file, not a directory, must be rejected too.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalizeCodexHome(f); err == nil {
		t.Fatal("a non-directory home must error")
	}
}

func TestCanonicalizeCodexHomeStableAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	c1, err := CanonicalizeCodexHome(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if !filepath.IsAbs(c1) {
		t.Fatalf("canonical form not absolute: %q", c1)
	}
	// Trailing separator / redundant separators must not change the result.
	c2, err := CanonicalizeCodexHome(filepath.Clean(dir + string(filepath.Separator)))
	if err != nil {
		t.Fatalf("canonicalize trailing sep: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("canonical form unstable across trailing separator: %q vs %q", c1, c2)
	}
	fp, err := CodexHomeFingerprint(dir)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if len(fp) != 64 {
		t.Fatalf("fingerprint = %q, want 64-hex", fp)
	}
	if fp2, _ := CodexHomeFingerprint(dir); fp2 != fp {
		t.Fatalf("fingerprint not stable: %q vs %q", fp, fp2)
	}
}

func TestCanonicalizeCodexHomeRejectsReparseRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("reparse roots are a Windows concept")
	}
	root := t.TempDir()
	real := t.TempDir()
	link := filepath.Join(root, "home")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink/junction creation unsupported: %v", err)
	}
	if _, err := CanonicalizeCodexHome(link); err == nil {
		t.Fatal("a reparse-point codex home must be rejected")
	}
	if _, err := CodexHomeFingerprint(link); err == nil {
		t.Fatal("fingerprint of a reparse-point home must error")
	}
}
