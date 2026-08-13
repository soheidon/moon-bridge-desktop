//go:build windows

package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func newBackupTestDir(t *testing.T) string {
	t.Helper()
	base := windowsTrustedBase()
	if base == "" {
		t.Fatal("trusted backup base unavailable")
	}
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		t.Fatal("trusted backup base is not an existing directory")
	}
	attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(base))
	if err != nil || isReparsePoint(attrs) {
		t.Fatal("trusted backup base is not a normal directory")
	}
	dir, err := os.MkdirTemp(base, "moonbridge-backup-test-")
	if err != nil {
		t.Fatal("trusted backup fixture creation failed")
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rootAny, err := (windowsBackupPlatform{trustedBase: base}).openRoot(dir)
	if err != nil {
		t.Fatal("trusted backup fixture was not accepted")
	}
	root := rootAny.(*windowsBackupRoot)
	if err := (windowsBackupPlatform{trustedBase: base}).verifyRoot(root); err != nil {
		_ = windows.CloseHandle(root.handle)
		t.Fatal("trusted backup fixture failed root verification")
	}
	if err := windows.CloseHandle(root.handle); err != nil {
		t.Fatal("trusted backup fixture handle close failed")
	}
	return dir
}

func assertSafeFixtureError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected trusted-boundary rejection")
	}
	message := err.Error()
	for _, secret := range []string{"C:\\", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "\\\\", "sid", "username", "secret"} {
		if strings.Contains(strings.ToLower(message), strings.ToLower(secret)) {
			t.Fatal("trusted-boundary error leaked sensitive detail")
		}
	}
}

func TestWindowsBackupFixtureBoundaries(t *testing.T) {
	base := windowsTrustedBase()
	if base == "" {
		t.Fatal("trusted backup base unavailable")
	}
	dir := newBackupTestDir(t)
	if dir == base {
		t.Fatal("backup fixture must be below trusted base")
	}
	if rel, err := filepath.Rel(base, dir); err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatal("backup fixture is not below trusted base")
	}
	_, err := (windowsBackupPlatform{trustedBase: base}).openRoot(base)
	assertSafeFixtureError(t, err)
	outside := filepath.VolumeName(base) + string(os.PathSeparator)
	if outside == base {
		t.Fatal("outside boundary is not distinct")
	}
	if rel, err := filepath.Rel(base, outside); err != nil || rel != ".." {
		t.Fatal("outside boundary was not classified outside")
	}
	_, err = (windowsBackupPlatform{trustedBase: base}).openRoot(outside)
	assertSafeFixtureError(t, err)
}

func TestWindowsBackupFixtureCleanupIsScoped(t *testing.T) {
	base := windowsTrustedBase()
	if base == "" {
		t.Fatal("trusted backup base unavailable")
	}
	sibling, err := os.MkdirTemp(base, "moonbridge-backup-sibling-")
	if err != nil {
		t.Fatal("unrelated sibling creation failed")
	}
	siblingFile := filepath.Join(sibling, "keep.txt")
	if err := os.WriteFile(siblingFile, []byte("keep"), 0o600); err != nil {
		_ = os.RemoveAll(sibling)
		t.Fatal("unrelated sibling file creation failed")
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	var fixtureDir string
	t.Run("fixture", func(t *testing.T) {
		fixtureDir = newBackupTestDir(t)
		if _, err := os.Stat(fixtureDir); err != nil {
			t.Fatal("backup fixture was not created")
		}
	})
	if _, err := os.Stat(fixtureDir); !os.IsNotExist(err) {
		t.Fatal("backup fixture cleanup escaped its scope")
	}
	if _, err := os.Stat(siblingFile); err != nil {
		t.Fatal("unrelated sibling was removed")
	}
}
