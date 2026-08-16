package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/recovery"
)

// scopedGatewayIntegration wires a disposable Codex config + Recovery store into
// opts so a StartGateway call exercises the gateway-integration step (S0 -> S1)
// against a temp profile instead of the real user's config.toml / recovery
// state. It returns opts with the CodexConfig / Recovery / RecoveryHome /
// BackupDir seams filled in, and (on Windows) pins LOCALAPPDATA to the temp root
// so backup anchoring never touches the real profile.
func scopedGatewayIntegration(t *testing.T, opts AppOptions) AppOptions {
	t.Helper()
	root := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", root)
	}
	codexHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"gpt-test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backups")
	recoveryDir := filepath.Join(root, "recovery")
	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   recoveryDir,
		CodexHome:     codexHome,
		BackupDir:     backupDir,
		TrafficLogDir: filepath.Join(root, "logs", "traffic-analysis"),
		AppDataRoot:   filepath.Join(root, "appdata"),
	}, filepath.Join(recoveryDir, "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	opts.CodexConfig = codexconfig.New(codexconfig.Options{Home: codexHome, BackupDir: backupDir})
	opts.Recovery = store
	opts.RecoveryHome = codexHome
	opts.BackupDir = backupDir
	return opts
}
