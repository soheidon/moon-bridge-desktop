//go:build windows

package codexconfig

import "golang.org/x/sys/windows"

func replaceFile(src, dst string) error {
	srcp, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstp, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(srcp, dstp, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// syncParentDir is a no-op on Windows: there is no portable directory-handle
// sync, and MoveFileEx(MOVEFILE_WRITE_THROUGH) is the durability boundary.
func syncParentDir(string) error { return nil }
