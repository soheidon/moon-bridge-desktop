//go:build windows

package recovery

import (
	"os"
	"syscall"
)

// windowsReparseAttr is FILE_ATTRIBUTE_REPARSE_POINT (0x400).
const windowsReparseAttr = 0x400

// isReparseAttrs reports whether path is an NTFS reparse point (symlink,
// junction, mount point), mirroring Rust's `file_attributes() & 0x400`.
func isReparseAttrs(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	// os.FileInfo on Windows wraps syscall.Win32FileAttributeData; read the
	// FILE_ATTRIBUTE_REPARSE_POINT bit when available, else fall back to
	// ModeSymlink so symlinks are still flagged.
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&windowsReparseAttr != 0
	}
	return fi.Mode()&os.ModeSymlink != 0
}
