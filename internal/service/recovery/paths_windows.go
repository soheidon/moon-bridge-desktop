//go:build windows

package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
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

// normalizePathForRedirectComparison expands Windows 8.3 components without
// following symlinks or junctions. The leaf may not exist yet, so its existing
// parent is expanded and the original leaf name is reattached. This keeps a
// real reparse redirect visible to checkAncestorPath while treating short and
// long spellings of the same ordinary parent as equivalent.
func normalizePathForRedirectComparison(path string) (string, error) {
	clean := filepath.Clean(stripVerbatimPrefix(path))
	if _, err := os.Lstat(clean); err == nil {
		return windowsLongPath(clean)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	parent := filepath.Dir(clean)
	if parent == clean {
		return "", errors.New("path has no existing parent")
	}
	longParent, err := windowsLongPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(longParent, filepath.Base(clean)), nil
}

func windowsLongPath(path string) (string, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	n, err := windows.GetLongPathName(ptr, nil, 0)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", errors.New("long path is empty")
	}
	buf := make([]uint16, n)
	n, err = windows.GetLongPathName(ptr, &buf[0], uint32(len(buf)))
	if err != nil {
		return "", err
	}
	return filepath.Clean(windows.UTF16ToString(buf[:n])), nil
}
