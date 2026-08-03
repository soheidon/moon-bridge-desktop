//go:build windows

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
	if !info.IsDir() {
		t.Fatalf("config path is not a directory: %s", path)
	}
	// Windows ACLs represent directory access; FileMode().Perm() does not
	// reliably expose the 0700 request made by os.Chmod on this platform.
	file, err := os.CreateTemp(path, ".permission-check-*")
	if err != nil {
		t.Fatalf("create permission-check file: %v", err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		t.Fatalf("close permission-check file: %v", err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatalf("remove permission-check file: %v", err)
	}
}
