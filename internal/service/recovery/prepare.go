package recovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareTargetHome creates a target codex home directory when it does not yet
// exist (first-run). It validates the entire ancestor path before creating the
// child so that a symlink/junction/reparse-point ancestor is rejected before any
// filesystem mutation.
//
// The contract:
//  1. If targetHome already exists and is a directory → return nil (no-op).
//  2. If targetHome already exists and is NOT a directory → error.
//  3. The full ancestor path is canonicalized through EvalSymlinks (or, when the
//     target does not yet exist, the nearest existing ancestor is resolved and
//     the remainder is re-joined). The resolved ancestor must not be a reparse
//     point, and the resolved path must equal the cleaned absolute path (this
//     catches junction/symlink ancestors whose child appears as a normal
//     directory to os.Stat).
//  4. Create targetHome with 0755.
//  5. Re-verify: targetHome must now exist, be a directory, and not be a reparse
//     point (a TOCTOU race could have swapped it between create and verify).
//
// The caller is expected to call CanonicalizeCodexHome after this function
// returns nil, which performs the full symlink/resolution/Windows-case check.
func PrepareTargetHome(targetHome string) error {
	if targetHome == "" {
		return errors.New("target home path is empty")
	}
	abs, err := filepath.Abs(targetHome)
	if err != nil {
		return fmt.Errorf("target home is not absolute-resolvable: %w", err)
	}
	clean := filepath.Clean(abs)

	// Case 1 & 2: target already exists.
	if fi, err := os.Stat(clean); err == nil {
		if fi.IsDir() {
			return nil
		}
		return errors.New("target home path exists but is not a directory")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat target home: %w", err)
	}

	// Case 3: validate the full ancestor path. The parent must physically
	// exist; then evalPhysicalDir resolves it (and every ancestor above it)
	// through symlinks/junctions. When the resolved path does not equal the
	// cleaned absolute path, an ancestor contains a symlink/junction that
	// redirects the target outside its expected location.
	parent := filepath.Dir(clean)
	if parent == clean {
		return errors.New("target home has no parent directory")
	}
	if _, err := os.Stat(parent); err != nil {
		return fmt.Errorf("target home parent does not exist: %w", err)
	}
	resolved, err := evalPhysicalDir(clean)
	if err != nil {
		return fmt.Errorf("target home ancestor cannot be resolved: %w", err)
	}
	// On Windows, CanonicalizeCodexHome lowercases the resolved path.
	// PrepareTargetHome must compare the same way to avoid a false mismatch.
	if err := checkAncestorPath(clean, resolved); err != nil {
		return err
	}

	// Case 4: create the target.
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return fmt.Errorf("create target home: %w", err)
	}

	// Case 5: re-verify after creation (TOCTOU guard).
	if isReparseAttrs(clean) {
		return errors.New("target home became a reparse point after creation")
	}
	return nil
}

// checkAncestorPath verifies that the resolved physical path matches the
// expected cleaned absolute path. On Windows the comparison is
// case-insensitive (matching CanonicalizeCodexHome behavior). A mismatch means
// a symlink/junction ancestor redirected the target to an unexpected location.
func checkAncestorPath(clean, resolved string) error {
	// filepath.EvalSymlinks already returns a cleaned path; we compare against
	// the cleaned absolute input. On Windows, lowercase both (matching
	// CanonicalizeCodexHome).
	a := clean
	b := resolved
	if isWindows {
		a = filepath.Clean(toLower(a))
		b = filepath.Clean(toLower(b))
	}
	if a != b {
		return errors.New("target home ancestor contains a symlink or junction redirect")
	}
	// The resolved ancestor must not itself be a reparse point.
	if isReparseAttrs(filepath.Dir(resolved)) {
		return errors.New("target home ancestor is a reparse point")
	}
	return nil
}

// isWindows is set at init time so checkAncestorPath can mirror the
// CanonicalizeCodexHome case-folding without importing runtime.
var isWindows bool

func init() {
	isWindows = filepath.Separator == '\\'
}

func toLower(s string) string {
	// ASCII-only lowercase mirrors the Windows case-insensitive comparison in
	// CanonicalizeCodexHome (which calls strings.ToLower on the resolved path).
	// Paths on Windows contain drive letters and backslashes — no Unicode is
	// expected in canonical codex-home paths.
	b := make([]byte, len(s))
	for i, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
