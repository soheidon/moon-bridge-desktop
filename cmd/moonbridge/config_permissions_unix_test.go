//go:build !windows

package main

import (
	"os"
	"testing"
)

func assertConfigDirPermissions(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("created config dir mode = %v, want 0700", got)
	}
}
