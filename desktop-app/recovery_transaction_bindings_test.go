package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/recovery"
)

func recoveryStringPtr(value string) *string { return &value }

func TestRestoreRecoveryRestoresManagedRootWithoutReplacingTheFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	backup := filepath.Join(root, "backups")
	recoveryDir := filepath.Join(root, "recovery")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	original := "# keep\nmodel = \"gpt-test\"\nopenai_base_url = \"https://api.openai.com/v1\"\n[extra]\nmarker = \"keep-me\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	editor := codexconfig.New(codexconfig.Options{Home: home, BackupDir: backup})
	before, err := editor.ReadRootURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := editor.PrepareRootURLChange(ctx, recoveryStringPtr("http://127.0.0.1:38441"), before.ConfigHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := editor.CommitPreparedRootURLChange(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	after, err := editor.ReadRootURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, err := recovery.NewStore(&recovery.Paths{RecoveryDir: recoveryDir, CodexHome: home, BackupDir: backup}, filepath.Join(recoveryDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := recovery.CodexHomeFingerprint(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, &recovery.State{
		SchemaVersion: recovery.SchemaVersion, IntegrationActive: true,
		Phase: recovery.PhaseIntegrationApplied, OperationID: "op-test",
		ConfigPath: "config.toml", CodexHomeFingerprint: fingerprint,
		PreviousOpenaiBaseURLPresent: true, PreviousOpenaiBaseURL: recovery.StringPtr(before.Value),
		AppliedOpenaiBaseURL: "http://127.0.0.1:38441", ConfigHashBeforeApply: before.ConfigHash,
		ConfigHashAfterApply: after.ConfigHash, StartedAt: "2026-08-07T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	app := NewApp(AppOptions{CodexConfig: editor, Recovery: store, RecoveryHome: home, BackupDir: backup, EmitEvents: noopEmit})
	result := app.RestoreRecovery(RestoreRecoveryInput{})
	if !result.OK || result.Value == nil {
		t.Fatalf("RestoreRecovery() = %#v, want success", result)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != original {
		t.Fatalf("restored config changed unrelated content:\n%s", content)
	}
	state, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.IntegrationActive || state.Phase != recovery.PhaseReconciledRestored {
		t.Fatalf("recovery state after restore = %#v, want reconciled_restored/inactive", state)
	}
}

func TestRestoreRecoveryConflictRequiresConfirmationAndLocalConfirmationPreservesExternalChange(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	backup := filepath.Join(root, "backups")
	recoveryDir := filepath.Join(root, "recovery")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	original := "model = \"gpt-test\"\nopenai_base_url = \"https://api.openai.com/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	editor := codexconfig.New(codexconfig.Options{Home: home, BackupDir: backup})
	before, _ := editor.ReadRootURL(ctx)
	prepared, err := editor.PrepareRootURLChange(ctx, recoveryStringPtr("http://127.0.0.1:38441"), before.ConfigHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := editor.CommitPreparedRootURLChange(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("model = \"changed-by-user\"\nopenai_base_url = \"http://external.invalid\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, _ := editor.ReadRootURL(ctx)
	fingerprint, _ := recovery.CodexHomeFingerprint(home)
	store, err := recovery.NewStore(&recovery.Paths{RecoveryDir: recoveryDir, CodexHome: home, BackupDir: backup}, filepath.Join(recoveryDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, &recovery.State{
		SchemaVersion: recovery.SchemaVersion, IntegrationActive: true, Phase: recovery.PhaseIntegrationApplied,
		OperationID: "op-conflict", ConfigPath: "config.toml", CodexHomeFingerprint: fingerprint,
		PreviousOpenaiBaseURLPresent: true, PreviousOpenaiBaseURL: recovery.StringPtr(before.Value),
		AppliedOpenaiBaseURL: "http://127.0.0.1:38441", ConfigHashBeforeApply: before.ConfigHash,
		ConfigHashAfterApply: prepared.AfterHash, StartedAt: "2026-08-07T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	app := NewApp(AppOptions{CodexConfig: editor, Recovery: store, RecoveryHome: home, BackupDir: backup, EmitEvents: noopEmit})
	blocked := app.RestoreRecovery(RestoreRecoveryInput{})
	if blocked.OK || blocked.Error == nil || blocked.Error.Code != "recovery_config_conflict" || !blocked.Error.ConfirmationRequired {
		t.Fatalf("unconfirmed restore = %#v, want confirmation required", blocked)
	}
	confirmed := app.RestoreRecovery(RestoreRecoveryInput{ConfirmConflict: true})
	if !confirmed.OK {
		t.Fatalf("confirmed restore = %#v, want success", confirmed)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) == original || current.Value == "" {
		t.Fatalf("expected local managed-key restore while preserving external content: %s", content)
	}
}

func TestCleanupPendingDiscardDeletesOwnedBackupAndClearsPending(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	recoveryDir := filepath.Join(root, "recovery")
	backup := filepath.Join(root, "backups")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"gpt-test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := recovery.NewStore(&recovery.Paths{RecoveryDir: recoveryDir, CodexHome: home, BackupDir: backup}, filepath.Join(recoveryDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, &recovery.State{SchemaVersion: recovery.SchemaVersion, Phase: recovery.PhaseReconciledRestored, StartedAt: "2026-08-07T00:00:00Z", CleanupPending: &recovery.CleanupPending{TransactionID: "tx", BackupID: "20260805T103040123Z-config.toml", RouteMutationResult: "applied", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	app := NewApp(AppOptions{CodexConfig: codexconfig.New(codexconfig.Options{Home: home}), Recovery: store, RecoveryHome: home, BackupDir: backup, EmitEvents: noopEmit})
	result := app.DiscardRecovery(DiscardRecoveryInput{Confirm: true})
	if !result.OK {
		t.Fatalf("discard = %#v", result)
	}
	state, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.CleanupPending != nil {
		t.Fatalf("pending not cleared: %#v", state)
	}
}

func TestDiscardRecoveryRequiresConfirmationAndDeletesOnlyMatchingResolvedState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	recoveryDir := filepath.Join(root, "recovery")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"gpt-test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := recovery.NewStore(&recovery.Paths{RecoveryDir: recoveryDir, CodexHome: home}, filepath.Join(recoveryDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, &recovery.State{SchemaVersion: recovery.SchemaVersion, Phase: recovery.PhaseReconciledRestored, StartedAt: "2026-08-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	app := NewApp(AppOptions{CodexConfig: codexconfig.New(codexconfig.Options{Home: home}), Recovery: store, RecoveryHome: home, EmitEvents: noopEmit})
	blocked := app.DiscardRecovery(DiscardRecoveryInput{})
	if blocked.OK || blocked.Error == nil || !blocked.Error.ConfirmationRequired {
		t.Fatalf("unconfirmed discard = %#v, want confirmation required", blocked)
	}
	deleted := app.DiscardRecovery(DiscardRecoveryInput{Confirm: true})
	if !deleted.OK {
		t.Fatalf("confirmed discard = %#v, want success", deleted)
	}
	state, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("state after discard = %#v, want nil", state)
	}
}
