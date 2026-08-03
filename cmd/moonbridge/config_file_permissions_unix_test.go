//go:build !windows

package main

import (
	"os"
	"testing"
)

func assertConfigFilePermissions(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat created config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("created config mode = %v, want 0600", got)
	}
}
