//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// errAmbiguousCodexAppServer is returned when more than one process satisfies
// the strict app-server match. The caller must fail closed (never hand off).
var errAmbiguousCodexAppServer = errors.New("multiple ChatGPT codex app-server processes")

// detectChatGPTCodexAppServer locates the ChatGPT Desktop app-server child
// process (codex.exe) that is currently serving a running Codex session. It is
// strictly read-only: it enumerates processes via PowerShell and returns the
// target PID, never terminating anything.
//
// The match requires all three conditions (same as Plan 11-3 S1):
//
//	Name == codex.exe
//	AND CommandLine contains "app-server"
//	AND ParentProcessId belongs to a main ChatGPT.exe (CommandLine lacks --type=)
//
// It returns (pid, true, nil) for exactly one match, (0, false, nil) for zero,
// and (0, false, err) for multiple matches or an enumeration failure. No command
// line, config body, or URL is ever logged or returned.
func detectChatGPTCodexAppServer(ctx context.Context) (pid uint32, found bool, err error) {
	const script = `$m = @(Get-CimInstance Win32_Process -Filter "Name='ChatGPT.exe'" | Where-Object { $_.CommandLine -and ($_.CommandLine -notmatch '--type=') }); $p = @($m | ForEach-Object { $_.ProcessId }); Get-CimInstance Win32_Process -Filter "Name='codex.exe'" | Where-Object { $_.CommandLine -and ($_.CommandLine -match 'app-server') -and ($p -contains $_.ParentProcessId) } | ForEach-Object { $_.ProcessId }`

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, false, errors.New("codex app-server detection failed")
	}

	fields := strings.Fields(string(out))
	switch len(fields) {
	case 0:
		return 0, false, nil
	case 1:
		parsed, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return 0, false, errors.New("codex app-server detection returned an invalid pid")
		}
		return uint32(parsed), true, nil
	default:
		return 0, false, errAmbiguousCodexAppServer
	}
}
