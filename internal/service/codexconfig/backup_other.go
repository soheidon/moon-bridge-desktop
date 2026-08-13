//go:build !windows

package codexconfig

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// otherBackupPlatform preserves the original non-Windows CreateBackup
// behavior: MkdirAll the backup root, then create_new with 0600.
type otherBackupPlatform struct{}

type otherBackupRoot struct{ dir string }

type otherBackupFile struct {
	path string
	f    *os.File
}

func createBackupPlatform() backupPlatformOps { return otherBackupPlatform{} }

func (otherBackupPlatform) openRoot(dir string) (backupRoot, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return otherBackupRoot{dir: dir}, nil
}

func (otherBackupPlatform) verifyRoot(backupRoot) error         { return nil }
func (otherBackupPlatform) applyRootSecurity(backupRoot) error  { return nil }
func (otherBackupPlatform) verifyRootSecurity(backupRoot) error { return nil }

func (otherBackupPlatform) createFile(r backupRoot, name string) (backupFile, error) {
	root := r.(otherBackupRoot)
	path := filepath.Join(root.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errBackupExists
		}
		return nil, err
	}
	return otherBackupFile{path: path, f: f}, nil
}

func (otherBackupPlatform) verifyFile(backupFile, backupRoot, string) error { return nil }
func (otherBackupPlatform) applyFileSecurity(backupFile) error              { return nil }
func (otherBackupPlatform) verifyFileSecurity(backupFile) error             { return nil }

func (otherBackupPlatform) write(f backupFile, data []byte) error {
	b := f.(otherBackupFile)
	n, err := b.f.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (otherBackupPlatform) sync(f backupFile) error {
	b := f.(otherBackupFile)
	return b.f.Sync()
}

// deleteOnClose unlinks the artifact by name. The handle stays open until
// close, matching the original failure-cleanup behavior.
func (otherBackupPlatform) deleteOnClose(f backupFile) error {
	b := f.(otherBackupFile)
	return os.Remove(b.path)
}

func (otherBackupPlatform) retain(_ backupRoot, dir, protected string) error {
	retainConfigBackups(dir, protected)
	return nil
}

func (otherBackupPlatform) close(r backupRoot, f backupFile) error {
	if f != nil {
		b := f.(otherBackupFile)
		if b.f != nil {
			return b.f.Close()
		}
	}
	return nil
}
