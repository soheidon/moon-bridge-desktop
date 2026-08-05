package codexconfig

import (
	"os"
	"path/filepath"
)

// AtomicWrite replaces path with data: it writes a temporary sibling file,
// syncs it, then atomically replaces path. On any failure the original file is
// left untouched.
func AtomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config.toml.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return syncParentDir(dir)
}
