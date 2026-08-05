package codexlauncher

import (
	"path/filepath"
	"strings"
)

// PowerShellCommand returns the argument list for launching the codex CLI in an
// interactive PowerShell terminal. The codex path travels via the
// MOONBRIDGE_CODEX_EXE environment variable, so it never appears on the command
// line (which keeps quoting rules trivial and the path out of process listings).
func PowerShellCommand() string {
	return `-NoLogo -NoProfile -NoExit -Command "& $env:MOONBRIDGE_CODEX_EXE"`
}

// ResolvePowerShellPath joins the well-known Windows PowerShell location under
// systemRoot. The caller verifies the file exists and is a regular file before
// passing it to CreateProcess as lpApplicationName.
func ResolvePowerShellPath(systemRoot string) string {
	return filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

// IsSupportedCodexExecutable reports whether ext is one of the executable forms
// the launcher accepts for the codex binary.
func IsSupportedCodexExecutable(ext string) bool {
	switch strings.ToLower(ext) {
	case ".exe", ".cmd", ".bat":
		return true
	}
	return false
}

// VersionProbeCommand returns the (executable, args) used to probe a codex
// candidate's version: native exe are run directly, cmd/bat scripts go through
// cmd.exe /D /C so the script's --version flag reaches codex.
func VersionProbeCommand(candidate string) (name string, args []string) {
	if strings.EqualFold(filepath.Ext(candidate), ".exe") {
		return candidate, []string{"--version"}
	}
	return "cmd.exe", []string{"/D", "/C", candidate, "--version"}
}
