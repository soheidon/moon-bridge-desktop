//go:build windows

package publishrecovery

import (
	"errors"
	"os"
	"syscall"
)

// windowsReparseAttr is FILE_ATTRIBUTE_REPARSE_POINT (0x400). It marks symlinks
// and junctions alike, and os.Lstat does not reliably report it for directory
// junctions, so it is checked explicitly: following a pre-placed junction would
// redirect reads/writes outside the recovery root.
const windowsReparseAttr = 0x400

// validateManagedDirectory verifies path is an existing, real directory and
// neither a symlink nor a reparse point (junctions included).
func validateManagedDirectory(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return errors.New("path is not a real directory")
	}
	// os.FileInfo on Windows wraps syscall.Win32FileAttributeData; read the
	// FILE_ATTRIBUTE_REPARSE_POINT bit when available, else ModeSymlink (already
	// checked above) covers symlinks.
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok && d.FileAttributes&windowsReparseAttr != 0 {
		return errors.New("path is a reparse point")
	}
	return nil
}
