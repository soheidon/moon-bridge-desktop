//go:build windows

package codexlauncher

import (
	"testing"

	"golang.org/x/sys/windows"
)

// decodeEnvBlock splits a double-null-terminated UTF-16 env block on entry
// boundaries. windows.UTF16ToString cannot be used on the whole block because it
// stops at the first embedded NUL (the first entry separator).
func decodeEnvBlock(block []uint16) []string {
	var entries []string
	start := 0
	for i := 0; i < len(block); i++ {
		if block[i] == 0 {
			entries = append(entries, windows.UTF16ToString(block[start:i]))
			start = i + 1
		}
	}
	return entries
}

func TestBuildEnvBlockUTF16DoubleNullTerminated(t *testing.T) {
	entries := []string{"A=1", "CODEX_HOME=C:\\home"}
	block, err := buildEnvBlock(entries)
	if err != nil {
		t.Fatalf("buildEnvBlock failed: %v", err)
	}
	// The block must end with a double null (a trailing empty entry).
	decoded := decodeEnvBlock(block)
	if len(decoded) != len(entries)+1 {
		t.Fatalf("expected %d entries + terminator, got %v", len(entries), decoded)
	}
	for i, want := range entries {
		if decoded[i] != want {
			t.Fatalf("entry %d = %q, want %q", i, decoded[i], want)
		}
	}
	if decoded[len(decoded)-1] != "" {
		t.Fatalf("block must end with the double-null terminator, got %v", decoded)
	}
}

func TestBuildEnvBlockHandlesJapaneseAndSpaces(t *testing.T) {
	entries := []string{"MOONBRIDGE_CODEX_EXE=C:\\codex\\bin です\\codex.exe", "D=A B C"}
	block, err := buildEnvBlock(entries)
	if err != nil {
		t.Fatalf("buildEnvBlock failed: %v", err)
	}
	decoded := decodeEnvBlock(block)
	if decoded[0] != entries[0] {
		t.Fatalf("unicode/space entry mangled: %q", decoded[0])
	}
	if decoded[1] != "D=A B C" {
		t.Fatalf("space entry mangled: %q", decoded[1])
	}
}

func TestBuildEnvBlockEmpty(t *testing.T) {
	block, err := buildEnvBlock(nil)
	if err != nil {
		t.Fatalf("buildEnvBlock failed: %v", err)
	}
	if len(block) != 1 || block[0] != 0 {
		t.Fatalf("empty block must be a single null, got %v", block)
	}
}
