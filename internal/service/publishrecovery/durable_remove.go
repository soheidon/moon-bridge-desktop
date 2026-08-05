package publishrecovery

import (
	"os"
	"path/filepath"
)

// durableRemove removes a file durably: delete → confirm absence → flush the
// parent directory. The order matters — the deletion must be observable as gone
// and the directory entry change must be crash-durable before the caller is
// allowed to advance the journal past the mutation (the auth.json removal crash
// window in §11). A missing file is idempotent success. The parent-directory
// flush itself is platform-specific (see syncParentDir): on Unix it is an
// os.Open + Sync of the directory handle; on Windows there is no portable
// directory-handle sync, and the documented contract in
// durable_remove_windows.go is the durability boundary.
func durableRemove(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if _, err := os.Lstat(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncParentDir(filepath.Dir(path))
}
