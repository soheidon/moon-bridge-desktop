package codexlauncher

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPowerShellCommandUsesEnvVarNotPath(t *testing.T) {
	cmd := PowerShellCommand()
	if !strings.Contains(cmd, "MOONBRIDGE_CODEX_EXE") {
		t.Fatalf("command must reference the env var: %q", cmd)
	}
	if strings.Contains(cmd, `\`) || strings.Contains(cmd, ":\\") {
		t.Fatalf("command must not embed a filesystem path: %q", cmd)
	}
	if !strings.Contains(cmd, "-NoExit") {
		t.Fatalf("command must keep the terminal open: %q", cmd)
	}
}

func TestResolvePowerShellPath(t *testing.T) {
	got := ResolvePowerShellPath(`C:\Windows`)
	want := filepath.Join(`C:\Windows`, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if got != want {
		t.Fatalf("ResolvePowerShellPath = %q, want %q", got, want)
	}
}

func TestIsSupportedCodexExecutable(t *testing.T) {
	cases := map[string]bool{
		".exe": true, ".cmd": true, ".bat": true,
		".EXE": true, ".CMD": true, ".BAT": true,
		".ps1": false, ".sh": false, "": false, ".txt": false, ".dll": false,
	}
	for ext, want := range cases {
		if got := IsSupportedCodexExecutable(ext); got != want {
			t.Errorf("IsSupportedCodexExecutable(%q) = %v, want %v", ext, got, want)
		}
	}
}

func TestVersionProbeCommand(t *testing.T) {
	exeName, exeArgs := VersionProbeCommand(`C:\bin\codex.exe`)
	if exeName != `C:\bin\codex.exe` || !reflect.DeepEqual(exeArgs, []string{"--version"}) {
		t.Fatalf("exe probe = %q %v", exeName, exeArgs)
	}
	for _, script := range []string{`C:\bin\codex.cmd`, `C:\bin\codex.bat`, `C:\bin\codex.CMD`} {
		name, args := VersionProbeCommand(script)
		if name != "cmd.exe" || !reflect.DeepEqual(args, []string{"/D", "/C", script, "--version"}) {
			t.Fatalf("script probe for %q = %q %v", script, name, args)
		}
	}
}
