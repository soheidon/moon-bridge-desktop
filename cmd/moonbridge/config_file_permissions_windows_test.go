//go:build windows

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
	if !info.Mode().IsRegular() {
		t.Fatalf("created config is not a regular file: %v", info.Mode())
	}
	// Windows ACLs represent file access; FileMode().Perm() cannot reliably
	// expose the 0600 request made by os.OpenFile on this platform.
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open created config: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close created config: %v", err)
	}
}
