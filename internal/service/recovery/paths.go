package recovery

import (
	"os"
	"path/filepath"
	"strings"
)

// pathWithin reports whether p is lexically inside root (or equals it). It is a
// string check only and does NOT protect against symlink/junction/reparse
// escapes; use pathWithinPhysical when the target may redirect outside root.
func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// pathWithinPhysical reports whether a path resolves to a location inside root
// after following symlinks/junctions. It is the authoritative guard against an
// absolute (legacy) value that redirects outside an allowed root. When the target
// itself exists it is fully resolved first, so a leaf symlink/junction pointing
// outside root is rejected even though its parent is inside root; when it does
// not exist yet, only the existing ancestor chain is resolved and the remainder
// is re-joined lexically.
func pathWithinPhysical(root, target string) bool {
	rootPhys, err := evalPhysicalDir(root)
	if err != nil {
		return false
	}
	if isReparseAttrs(root) {
		// A reparse-point root is itself treated as unsupported (mirrors Rust:
		// "Codex home reparse point is unsupported").
		return false
	}
	if targetPhys, err := filepath.EvalSymlinks(target); err == nil {
		return pathWithin(rootPhys, targetPhys)
	}
	targetPhys, err := evalPhysicalDir(filepath.Dir(target))
	if err != nil {
		return false
	}
	return pathWithin(rootPhys, targetPhys)
}

// evalPhysicalDir resolves a directory path through symlinks/junctions to its
// physical location. If root does not yet exist, evalPhysicalDir walks up to the
// nearest existing ancestor and joins the remainder, so create-then-validate
// flows still get a stable physical anchor.
func evalPhysicalDir(dir string) (string, error) {
	abs := filepath.Clean(dir)
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if abs == cur {
				return resolved, nil
			}
			// abs == cur + suffix; re-join the not-yet-existing suffix.
			suffix, _ := filepath.Rel(cur, abs)
			return filepath.Join(resolved, suffix), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		cur = parent
	}
}
