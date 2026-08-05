//go:build !windows

package recovery

// isReparseAttrs reports reparse-point status. Non-Windows platforms have no
// NTFS reparse points; symlinks are rejected generically by pathWithinPhysical
// via EvalSymlinks. Return false here.
func isReparseAttrs(path string) bool {
	return false
}
