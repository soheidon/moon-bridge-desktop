//go:build !windows

package publishrecovery

import "os"

// syncParentDir flushes the parent directory so a just-removed entry is
// crash-durable. Required after a plain os.Remove before the journal can advance
// past a deletion mutation. This mirrors codexconfig's private syncParentDir;
// that one is unexported, so the pattern is mirrored rather than reused.
func syncParentDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
