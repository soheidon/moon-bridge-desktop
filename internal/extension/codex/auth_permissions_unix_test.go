//go:build !windows

package codex_test

import (
	"os"
	"testing"
)

func assertAuthJSONPermissions(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("auth.json perm = %04o, want 0600", perm)
	}
}
