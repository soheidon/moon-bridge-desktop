//go:build windows

package recovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrepareTargetHomeAcceptsShortPathAncestor(t *testing.T) {
	longParent := filepath.Join(t.TempDir(), "long parent directory for short path")
	if err := os.MkdirAll(longParent, 0o755); err != nil {
		t.Fatal(err)
	}

	ptr, err := windows.UTF16PtrFromString(longParent)
	if err != nil {
		t.Fatal(err)
	}
	n, err := windows.GetShortPathName(ptr, nil, 0)
	if err != nil || n == 0 {
		t.Skipf("8.3 short paths are unavailable: %v", err)
	}
	buf := make([]uint16, n)
	n, err = windows.GetShortPathName(ptr, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Skipf("8.3 short paths are unavailable: %v", err)
	}
	shortParent := filepath.Clean(windows.UTF16ToString(buf[:n]))
	if strings.EqualFold(shortParent, longParent) {
		t.Skip("volume did not produce a distinct 8.3 path")
	}

	target := filepath.Join(shortParent, "codex-home")
	if err := PrepareTargetHome(target); err != nil {
		t.Fatalf("PrepareTargetHome(short-path ancestor): %v", err)
	}
	if fi, err := os.Stat(filepath.Join(longParent, "codex-home")); err != nil {
		t.Fatalf("long-path target was not created: %v", err)
	} else if !fi.IsDir() {
		t.Fatal("long-path target is not a directory")
	}
}
