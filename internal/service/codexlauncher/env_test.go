package codexlauncher

import (
	"reflect"
	"testing"
)

func TestMergeEnvReplacesCaseInsensitiveWithoutDuplicating(t *testing.T) {
	base := []string{"Path=C:\\orig", "CODEX_HOME=C:\\old-home", "MOONBRIDGE_CODEX_EXE=C:\\old.exe"}
	merged := MergeEnv(base, map[string]string{
		"codex_home":          `C:\new-home`,
		"MOONBRIDGE_CODEX_EXE": `C:\new.exe`,
	})
	if len(merged) != 3 {
		t.Fatalf("merged env must not grow: %v", merged)
	}
	if merged[0] != "Path=C:\\orig" {
		t.Fatalf("unrelated entry reordered: %v", merged)
	}
	if merged[1] != "codex_home=C:\\new-home" {
		t.Fatalf("case-insensitive replace failed: %v", merged)
	}
	if merged[2] != "MOONBRIDGE_CODEX_EXE=C:\\new.exe" {
		t.Fatalf("case-insensitive replace failed: %v", merged)
	}
}

func TestMergeEnvAppends(t *testing.T) {
	base := []string{"A=1"}
	merged := MergeEnv(base, map[string]string{"B": "2", "C": "3"})
	want := []string{"A=1", "B=2", "C=3"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("MergeEnv = %v, want %v", merged, want)
	}
}

func TestMergeEnvEmptyOverridesReturnsBase(t *testing.T) {
	base := []string{"A=1"}
	if got := MergeEnv(base, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("nil overrides must return base: %v", got)
	}
}

func TestCutEnv(t *testing.T) {
	key, value, ok := cutEnv("CODEX_HOME=C:\\home")
	if !ok || key != "CODEX_HOME" || value != "C:\\home" {
		t.Fatalf("cutEnv = %q %q %v", key, value, ok)
	}
	if _, _, ok := cutEnv("NO_SEPARATOR"); ok {
		t.Fatal("entry without = must report false")
	}
	if key, value, ok := cutEnv("=value"); !ok || key != "" || value != "value" {
		t.Fatalf("empty key not handled: %q %q %v", key, value, ok)
	}
}
