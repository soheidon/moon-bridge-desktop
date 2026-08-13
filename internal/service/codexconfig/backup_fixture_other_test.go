//go:build !windows

package codexconfig

import "testing"

func newBackupTestDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
