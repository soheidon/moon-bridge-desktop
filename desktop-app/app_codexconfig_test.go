package main

import (
	"context"
	"testing"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/gateway"
)

// scriptedCodexConfig is a codexConfigController fake. The zero value returns
// empty results and nil errors; tests override the per-method functions.
type scriptedCodexConfig struct {
	loadFn    func(context.Context) (codexconfig.Snapshot, error)
	updateFn  func(context.Context, codexconfig.Input) (codexconfig.Snapshot, error)
	restoreFn func(context.Context, string) (codexconfig.Snapshot, error)
	listFn    func(context.Context) ([]codexconfig.BackupInfo, error)
}

func (c *scriptedCodexConfig) Load(ctx context.Context) (codexconfig.Snapshot, error) {
	if c.loadFn != nil {
		return c.loadFn(ctx)
	}
	return codexconfig.Snapshot{}, nil
}

func (c *scriptedCodexConfig) Update(ctx context.Context, in codexconfig.Input) (codexconfig.Snapshot, error) {
	if c.updateFn != nil {
		return c.updateFn(ctx, in)
	}
	return codexconfig.Snapshot{}, nil
}

func (c *scriptedCodexConfig) Restore(ctx context.Context, id string) (codexconfig.Snapshot, error) {
	if c.restoreFn != nil {
		return c.restoreFn(ctx, id)
	}
	return codexconfig.Snapshot{}, nil
}

func (c *scriptedCodexConfig) ListBackups(ctx context.Context) ([]codexconfig.BackupInfo, error) {
	if c.listFn != nil {
		return c.listFn(ctx)
	}
	return []codexconfig.BackupInfo{}, nil
}

func newCodexConfigApp(t *testing.T, cc codexConfigController) *App {
	t.Helper()
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	return NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
		CodexConfig: cc,
	})
}

func snapshotExists() codexconfig.Snapshot {
	return codexconfig.Snapshot{
		Path:          `C:\Users\test\.codex\config.toml`,
		Exists:        true,
		Model:         "deepseek-v4-pro",
		ModelProvider: "moonbridge",
		BaseURL:       "https://api.deepseek.com/anthropic",
	}
}

func sampleBackups(n int) []codexconfig.BackupInfo {
	out := make([]codexconfig.BackupInfo, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, codexconfig.BackupInfo{
			ID:        string(rune('a' + i)),
			Name:      "20260101T000000000Z-config.toml",
			Path:      `C:\backups\codex-config\20260101T000000000Z-config.toml`,
			CreatedAt: time.Unix(int64(1700000000+i), 0),
			Size:      128,
		})
	}
	return out
}

func TestLoadCodexConfigSuccess(t *testing.T) {
	cc := &scriptedCodexConfig{
		loadFn: func(context.Context) (codexconfig.Snapshot, error) { return snapshotExists(), nil },
		listFn: func(context.Context) ([]codexconfig.BackupInfo, error) { return sampleBackups(2), nil },
	}
	app := newCodexConfigApp(t, cc)
	defer app.shutdown(context.Background())

	res := app.LoadCodexConfig()
	if !res.OK || res.Value == nil {
		t.Fatalf("LoadCodexConfig() = %#v, want ok", res)
	}
	if res.Error != nil {
		t.Fatalf("LoadCodexConfig() error = %#v, want nil", res.Error)
	}
	if res.Value.Config == nil || !res.Value.Config.Exists {
		t.Fatalf("config = %#v, want exists snapshot", res.Value.Config)
	}
	if res.Value.Config.Model != "deepseek-v4-pro" || res.Value.Config.ModelProvider != "moonbridge" {
		t.Fatalf("config = %#v, want model/provider fields", res.Value.Config)
	}
	if len(res.Value.Backups) != 2 {
		t.Fatalf("backups = %d, want 2", len(res.Value.Backups))
	}
}

func TestLoadCodexConfigMissingIsNotError(t *testing.T) {
	cc := &scriptedCodexConfig{
		loadFn: func(context.Context) (codexconfig.Snapshot, error) {
			return codexconfig.Snapshot{Path: `C:\Users\test\.codex\config.toml`, Exists: false}, nil
		},
	}
	app := newCodexConfigApp(t, cc)
	defer app.shutdown(context.Background())

	res := app.LoadCodexConfig()
	if !res.OK {
		t.Fatalf("LoadCodexConfig() = %#v, want ok for a missing config", res)
	}
	if res.Value == nil || res.Value.Config == nil || res.Value.Config.Exists {
		t.Fatalf("config = %#v, want Exists=false and not an error", res.Value)
	}
}

func TestUpdateCodexConfigSuccess(t *testing.T) {
	cc := &scriptedCodexConfig{
		updateFn: func(_ context.Context, in codexconfig.Input) (codexconfig.Snapshot, error) {
			return snapshotExists(), nil
		},
	}
	app := newCodexConfigApp(t, cc)
	defer app.shutdown(context.Background())

	res := app.UpdateCodexConfig(codexconfig.Input{Model: "deepseek-v4-flash"})
	if !res.OK || res.Value == nil {
		t.Fatalf("UpdateCodexConfig() = %#v, want ok", res)
	}
	if res.Value.Config == nil || !res.Value.Config.Exists {
		t.Fatalf("config = %#v, want updated snapshot", res.Value.Config)
	}
}

func TestUpdateCodexConfigValidationField(t *testing.T) {
	field := "baseUrl"
	cc := &scriptedCodexConfig{
		updateFn: func(context.Context, codexconfig.Input) (codexconfig.Snapshot, error) {
			return codexconfig.Snapshot{}, &codexconfig.Error{Kind: codexconfig.KindValidationFailed, Field: &field, Message: "base url must be an http(s) URL"}
		},
	}
	app := newCodexConfigApp(t, cc)
	defer app.shutdown(context.Background())

	res := app.UpdateCodexConfig(codexconfig.Input{BaseURL: "ftp://nope"})
	if res.OK {
		t.Fatal("UpdateCodexConfig() ok = true, want false")
	}
	if res.Value != nil {
		t.Fatalf("UpdateCodexConfig() value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "codex_config_validation_failed" {
		t.Fatalf("UpdateCodexConfig() error = %#v, want codex_config_validation_failed", res.Error)
	}
	if res.Error.Field == nil || *res.Error.Field != "baseUrl" {
		t.Fatalf("UpdateCodexConfig() field = %v, want baseUrl", res.Error.Field)
	}
}

func TestRestoreCodexConfigBackupSuccess(t *testing.T) {
	cc := &scriptedCodexConfig{
		restoreFn: func(_ context.Context, id string) (codexconfig.Snapshot, error) {
			return snapshotExists(), nil
		},
	}
	app := newCodexConfigApp(t, cc)
	defer app.shutdown(context.Background())

	res := app.RestoreCodexConfigBackup(RestoreCodexConfigBackupRequest{ID: "a"})
	if !res.OK || res.Value == nil {
		t.Fatalf("RestoreCodexConfigBackup() = %#v, want ok", res)
	}
	if res.Value.Config == nil || !res.Value.Config.Exists {
		t.Fatalf("config = %#v, want restored snapshot", res.Value.Config)
	}
}

func TestRestoreCodexConfigBackupTraversalRejected(t *testing.T) {
	cc := &scriptedCodexConfig{
		restoreFn: func(context.Context, string) (codexconfig.Snapshot, error) {
			return codexconfig.Snapshot{}, &codexconfig.Error{Kind: codexconfig.KindRestoreFailed, Message: "invalid backup id"}
		},
	}
	app := newCodexConfigApp(t, cc)
	defer app.shutdown(context.Background())

	res := app.RestoreCodexConfigBackup(RestoreCodexConfigBackupRequest{ID: "..\\..\\config.toml"})
	if res.OK {
		t.Fatal("RestoreCodexConfigBackup() ok = true, want false")
	}
	if res.Value != nil {
		t.Fatalf("RestoreCodexConfigBackup() value = %#v, want nil", res.Value)
	}
	if res.Error == nil || res.Error.Code != "codex_config_restore_failed" {
		t.Fatalf("RestoreCodexConfigBackup() error = %#v, want codex_config_restore_failed", res.Error)
	}
}

func TestCodexConfigBackupsSuccess(t *testing.T) {
	cc := &scriptedCodexConfig{
		listFn: func(context.Context) ([]codexconfig.BackupInfo, error) { return sampleBackups(3), nil },
	}
	app := newCodexConfigApp(t, cc)
	defer app.shutdown(context.Background())

	res := app.CodexConfigBackups()
	if !res.OK || res.Value == nil {
		t.Fatalf("CodexConfigBackups() = %#v, want ok", res)
	}
	if len(res.Value.Backups) != 3 {
		t.Fatalf("backups = %d, want 3", len(res.Value.Backups))
	}
	if res.Value.Backups[0].ID != "a" || res.Value.Backups[0].Size != 128 {
		t.Fatalf("backups[0] = %#v, want mapped ID/size", res.Value.Backups[0])
	}
}

func TestCodexConfigBindingsClosed(t *testing.T) {
	app := newCodexConfigApp(t, &scriptedCodexConfig{})
	app.shutdown(context.Background())

	bindings := []func() DesktopCommandResult{
		func() DesktopCommandResult { return app.LoadCodexConfig() },
		func() DesktopCommandResult { return app.UpdateCodexConfig(codexconfig.Input{Model: "m"}) },
		func() DesktopCommandResult { return app.RestoreCodexConfigBackup(RestoreCodexConfigBackupRequest{ID: "a"}) },
		func() DesktopCommandResult { return app.CodexConfigBackups() },
	}
	for i, call := range bindings {
		res := call()
		if res.OK {
			t.Fatalf("binding %d ok = true after shutdown, want false", i)
		}
		if res.Value != nil {
			t.Fatalf("binding %d value = %#v, want nil", i, res.Value)
		}
		if res.Error == nil || res.Error.Code != "codex_host_closed" {
			t.Fatalf("binding %d error = %#v, want codex_host_closed", i, res.Error)
		}
		if res.Error.Stage != "host" {
			t.Fatalf("binding %d stage = %q, want host", i, res.Error.Stage)
		}
	}
}

func TestCodexConfigErrorMapping(t *testing.T) {
	tests := []struct {
		kind codexconfig.ErrorKind
		code string
	}{
		{codexconfig.KindNotFound, "codex_config_not_found"},
		{codexconfig.KindParseFailed, "codex_config_parse_failed"},
		{codexconfig.KindValidationFailed, "codex_config_validation_failed"},
		{codexconfig.KindEditUnsupported, "codex_config_edit_unsupported"},
		{codexconfig.KindUpdateFailed, "codex_config_update_failed"},
		{codexconfig.KindVerifyFailed, "codex_config_verify_failed"},
		{codexconfig.KindBackupFailed, "codex_config_backup_failed"},
		{codexconfig.KindRestoreFailed, "codex_config_restore_failed"},
		{codexconfig.KindNoBackups, "codex_config_no_backups"},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			res := codexConfigError("Op", "update", &codexconfig.Error{Kind: tt.kind, Message: "x"})
			if res.OK {
				t.Fatal("ok = true, want false")
			}
			if res.Value != nil {
				t.Fatalf("value = %#v, want nil", res.Value)
			}
			if res.Error == nil || res.Error.Code != tt.code {
				t.Fatalf("error = %#v, want code %q", res.Error, tt.code)
			}
		})
	}
}
