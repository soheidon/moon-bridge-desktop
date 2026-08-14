//go:build !windows

package recovery

import "path/filepath"

// isReparseAttrs reports reparse-point status. Non-Windows platforms have no
// NTFS reparse points; symlinks are rejected generically by pathWithinPhysical
// via EvalSymlinks. Return false here.
func isReparseAttrs(path string) bool {
	return false
}

func normalizePathForRedirectComparison(path string) (string, error) {
	return filepath.Clean(path), nil
}
