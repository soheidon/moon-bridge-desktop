//go:build windows

package config

// Windows does not provide the Unix directory-fsync durability primitive in
// the same way. File contents are still synced before rename, and the
// exclusive hard-link publication remains atomic; skip only the directory
// Sync that returns Access is denied on Windows.
func syncParentDir(string) error {
	return nil
}
