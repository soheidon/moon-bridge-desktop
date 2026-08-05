//go:build windows

package codexlauncher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	discoverCodex = discoverCodexWindows
}

// discoverCodexWindows finds the codex CLI on PATH, keeping the first candidate
// that is an executable file (exe/cmd/bat) and answers a version probe within
// timeout. The probe runs the exe directly and cmd/bat via cmd.exe /D /C.
func discoverCodexWindows(ctx context.Context, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := exec.CommandContext(probeCtx, "where.exe", "codex").Output()
	if err != nil {
		return "", &Error{Kind: KindNotFound, Message: "codex CLI not found on PATH"}
	}
	lastErr := error(&Error{Kind: KindNotFound, Message: "codex CLI not found on PATH"})
	for _, raw := range strings.Split(string(out), "\n") {
		candidate := strings.TrimSpace(raw)
		if candidate == "" {
			continue
		}
		if !isSupportedCodexFile(candidate) {
			lastErr = &Error{Kind: KindInvalidExecutable, Message: "codex candidate is not an executable file"}
			continue
		}
		name, args := VersionProbeCommand(candidate)
		if err := exec.CommandContext(probeCtx, name, args...).Run(); err != nil {
			lastErr = &Error{Kind: KindVersionProbeFailed, Message: "codex version probe failed"}
			continue
		}
		return candidate, nil
	}
	return "", lastErr
}

func isSupportedCodexFile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return IsSupportedCodexExecutable(filepath.Ext(p))
}
