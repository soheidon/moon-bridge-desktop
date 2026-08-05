package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HashBytes returns the SHA-256 hex of data (whole-file fingerprint).
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashFile returns the SHA-256 hex of the file's contents. It distinguishes a
// missing file (os.IsNotExist) from unreadable content so callers can classify
// config_unreadable vs absent.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CanonicalizeCodexHome returns a stable canonical form of a codex home path for
// root-binding: Abs must succeed → Clean → the directory must exist →
// EvalSymlinks must succeed → a reparse root is rejected → Windows volume/case
// normalization → Clean → the result must be absolute. Any failure is an error:
// an unresolved path is never canonicalized (it would produce an unstable root
// identifier). The same root always canonicalizes to the same value, which is
// the root identifier paired with a relative configPath.
func CanonicalizeCodexHome(home string) (string, error) {
	if home == "" {
		return "", errors.New("codex home is empty")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("codex home is not absolute-resolvable: %w", err)
	}
	clean := filepath.Clean(abs)
	fi, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("codex home is not a directory: %w", err)
	}
	if !fi.IsDir() {
		return "", errors.New("codex home is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("codex home cannot be physically resolved: %w", err)
	}
	if isReparseAttrs(clean) {
		return "", errors.New("codex home reparse point is unsupported")
	}
	if runtime.GOOS == "windows" {
		resolved = strings.ToLower(resolved)
	}
	out := filepath.Clean(resolved)
	if !filepath.IsAbs(out) {
		return "", errors.New("canonical codex home is not absolute")
	}
	return out, nil
}

// CodexHomeFingerprint returns the SHA-256 hex of the canonical codex home — the
// root-binding identifier stored beside a relative configPath. A home that cannot
// be canonicalized (missing, unresolved, reparse, non-directory) is an error: the
// caller must treat the root binding as unavailable rather than fingerprint an
// unresolved path.
func CodexHomeFingerprint(home string) (string, error) {
	canon, err := CanonicalizeCodexHome(home)
	if err != nil {
		return "", err
	}
	return HashBytes([]byte(canon)), nil
}
