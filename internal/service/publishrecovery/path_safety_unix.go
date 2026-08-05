//go:build !windows

package publishrecovery

import (
	"errors"
	"os"
)

// validateManagedDirectory verifies path is an existing, real directory and not a
// symlink. On Unix a symlink is the only indirection that could redirect a write
// or read outside the recovery root, and os.Lstat reports it directly, so this
// single check is sufficient.
func validateManagedDirectory(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return errors.New("path is not a real directory")
	}
	return nil
}
