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

// stripVerbatimPrefix converts the Windows extended-length (verbatim) forms that
// denote an ordinary file — `\\?\C:\...` and `\\?\UNC\server\share\...` — back to
// their canonical DOS/UNC form. Both spellings reference the same physical
// location, so stripping never weakens a containment check. Device and
// volume-GUID namespaces (`\\?\Volume{...}`, `\\?\pipe\...`, `\\.\...`) are left
// unchanged: they are not ordinary file paths and must keep failing the legacy
// absolute-path checks.
func stripVerbatimPrefix(path string) string {
	const uncPrefix = `\\?\UNC\`
	const drivePrefix = `\\?\`

	if strings.HasPrefix(strings.ToUpper(path), strings.ToUpper(uncPrefix)) {
		return `\\` + path[len(uncPrefix):]
	}

	if len(path) >= len(drivePrefix)+2 &&
		strings.HasPrefix(path, drivePrefix) &&
		isDriveLetter(path[len(drivePrefix)]) &&
		path[len(drivePrefix)+1] == ':' {
		return path[len(drivePrefix):]
	}

	return path
}

func isDriveLetter(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// pathWithinPhysical reports whether a path resolves to a location inside root
// after following symlinks/junctions. It is the authoritative guard against an
// absolute (legacy) value that redirects outside an allowed root. When the target
// itself exists it is fully resolved first, so a leaf symlink/junction pointing
// outside root is rejected even though its parent is inside root; when it does
// not exist yet, only the existing ancestor chain is resolved and the remainder
// is re-joined lexically.
func pathWithinPhysical(root, target string) bool {
	root = stripVerbatimPrefix(root)
	target = stripVerbatimPrefix(target)
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
		return pathWithin(stripVerbatimPrefix(rootPhys), stripVerbatimPrefix(targetPhys))
	}
	targetPhys, err := evalPhysicalDir(filepath.Dir(target))
	if err != nil {
		return false
	}
	return pathWithin(stripVerbatimPrefix(rootPhys), stripVerbatimPrefix(targetPhys))
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
