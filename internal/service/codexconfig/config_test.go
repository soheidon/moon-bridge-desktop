package codexconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestService(t *testing.T) (*Service, string, string) {
	t.Helper()
	home := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")
	svc := New(Options{Home: home, BackupDir: backupDir, Env: func(string) string { return "" }})
	return svc, home, backupDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func kindOf(t *testing.T, err error) ErrorKind {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *Error, got %v", err)
	}
	return ce.Kind
}

func TestLoadMissingConfigIsNotAnError(t *testing.T) {
	svc, home, _ := newTestService(t)
	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if snap.Exists || snap.Path != filepath.Join(home, "config.toml") {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestLoadParsesConfig(t *testing.T) {
	svc, home, _ := newTestService(t)
	writeFile(t, filepath.Join(home, "config.toml"), `model = "deepseek-v4-pro"
model_provider = "deepseek"

[model_providers.moonbridge]
base_url = "http://127.0.0.1:38440/v1"
`)
	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !snap.Exists || snap.Model != "deepseek-v4-pro" || snap.ModelProvider != "deepseek" || snap.BaseURL != "http://127.0.0.1:38440/v1" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestLoadParseFailed(t *testing.T) {
	svc, home, _ := newTestService(t)
	writeFile(t, filepath.Join(home, "config.toml"), `model = "unterminated
`)
	if _, err := svc.Load(context.Background()); kindOf(t, err) != KindParseFailed {
		t.Fatalf("expected parse_failed, got %v", err)
	}
}

func TestResolvePathUsesCodexHome(t *testing.T) {
	svc := New(Options{Env: func(key string) string {
		if key == "CODEX_HOME" {
			return `C:\codex\home`
		}
		return ""
	}})
	path, err := svc.ResolvePath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(`C:\codex\home`, "config.toml")
	if path != want {
		t.Fatalf("unexpected path %q, want %q", path, want)
	}
}

func TestResolvePathRejectsRelativeCodexHome(t *testing.T) {
	svc := New(Options{Env: func(key string) string {
		if key == "CODEX_HOME" {
			return "relative/home"
		}
		return ""
	}})
	if _, err := svc.ResolvePath(); err == nil {
		t.Fatal("expected relative CODEX_HOME to be rejected")
	}
}

func TestUpdateMissingConfigNotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Update(context.Background(), Input{Model: "b"}); kindOf(t, err) != KindNotFound {
		t.Fatalf("expected not_found, got %v", err)
	}
}

func TestUpdateValidationFailed(t *testing.T) {
	svc, home, _ := newTestService(t)
	writeFile(t, filepath.Join(home, "config.toml"), "model = \"a\"\n")
	_, err := svc.Update(context.Background(), Input{BaseURL: "not-a-url"})
	if k := kindOf(t, err); k != KindValidationFailed {
		t.Fatalf("expected validation_failed, got %v", err)
	}
	var ce *Error
	errors.As(err, &ce)
	if ce.Field == nil || *ce.Field != "baseUrl" {
		t.Fatalf("expected baseUrl field, got %v", ce.Field)
	}
	zero := int64(0)
	if _, err := svc.Update(context.Background(), Input{ModelContextWindow: &zero}); kindOf(t, err) != KindValidationFailed {
		t.Fatalf("expected validation_failed for non-positive window, got %v", err)
	}
}

func TestUpdateSetsFieldCreatesBackup(t *testing.T) {
	svc, home, backupDir := newTestService(t)
	configPath := filepath.Join(home, "config.toml")
	original := "# keep me\nmodel = \"old\"\n"
	writeFile(t, configPath, original)
	snap, err := svc.Update(context.Background(), Input{Model: "new"})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if snap.Model != "new" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	got := readFile(t, configPath)
	if got != "# keep me\nmodel = \"new\"\n" {
		t.Fatalf("config not updated:\n%s", got)
	}
	backups, err := ListBackups(backupDir)
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one backup, got %d err=%v", len(backups), err)
	}
	if b := readFile(t, backups[0].Path); b != original {
		t.Fatalf("backup content mismatch:\n%s", b)
	}
}

func TestUpdateNoOpDoesNotBackup(t *testing.T) {
	svc, home, backupDir := newTestService(t)
	configPath := filepath.Join(home, "config.toml")
	writeFile(t, configPath, "model = \"same\"\n")
	snap, err := svc.Update(context.Background(), Input{Model: "same"})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if snap.Model != "same" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	backups, _ := ListBackups(backupDir)
	if len(backups) != 0 {
		t.Fatalf("no-op update must not create a backup, got %d", len(backups))
	}
}

func TestUpdateParseFailedDoesNotWrite(t *testing.T) {
	svc, home, _ := newTestService(t)
	configPath := filepath.Join(home, "config.toml")
	broken := "model = \"unterminated\n"
	writeFile(t, configPath, broken)
	if _, err := svc.Update(context.Background(), Input{Model: "new"}); kindOf(t, err) != KindParseFailed {
		t.Fatalf("expected parse_failed, got %v", err)
	}
	if got := readFile(t, configPath); got != broken {
		t.Fatalf("broken config was modified:\n%s", got)
	}
}

func TestUpdateEditUnsupportedNoWrite(t *testing.T) {
	svc, home, _ := newTestService(t)
	configPath := filepath.Join(home, "config.toml")
	in := "model_providers = { moonbridge = { base_url = \"http://x\" } }\n"
	writeFile(t, configPath, in)
	if _, err := svc.Update(context.Background(), Input{BaseURL: "http://y"}); kindOf(t, err) != KindEditUnsupported {
		t.Fatalf("expected edit_unsupported, got %v", err)
	}
	if got := readFile(t, configPath); got != in {
		t.Fatalf("config modified on rejection:\n%s", got)
	}
}

func TestUpdateVerifyFailureRollsBack(t *testing.T) {
	svc, home, _ := newTestService(t)
	configPath := filepath.Join(home, "config.toml")
	original := "model = \"a\"\n"
	writeFile(t, configPath, original)
	prev := atomicWrite
	atomicWrite = func(path string, data []byte) error {
		// Simulate a storage bug that corrupts the edited payload on write.
		if string(data) == "model = \"b\"\n" {
			return os.WriteFile(path, []byte("model = \"BROKEN\"\n"), 0o600)
		}
		return AtomicWrite(path, data)
	}
	defer func() { atomicWrite = prev }()
	if _, err := svc.Update(context.Background(), Input{Model: "b"}); kindOf(t, err) != KindVerifyFailed {
		t.Fatalf("expected verify_failed, got %v", err)
	}
	if got := readFile(t, configPath); got != original {
		t.Fatalf("config not rolled back after verify failure:\n%s", got)
	}
}

func TestRestoreSuccessCreatesPreRestoreBackup(t *testing.T) {
	svc, home, backupDir := newTestService(t)
	configPath := filepath.Join(home, "config.toml")
	writeFile(t, configPath, "model = \"current\"\n")
	backupPath, err := CreateBackup(backupDir, []byte("model = \"old\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Advance config past the backup.
	writeFile(t, configPath, "model = \"modified\"\n")
	snap, err := svc.Restore(context.Background(), filepath.Base(backupPath))
	if err != nil {
		t.Fatalf("Restore failed: %v", err)
	}
	if !snap.Exists || snap.Model != "old" {
		t.Fatalf("unexpected snapshot after restore: %+v", snap)
	}
	if got := readFile(t, configPath); got != "model = \"old\"\n" {
		t.Fatalf("config not restored:\n%s", got)
	}
	backups, _ := ListBackups(backupDir)
	if len(backups) != 2 {
		t.Fatalf("expected a pre-restore backup to be created, got %d", len(backups))
	}
	// Pre-restore backup holds the pre-restore content.
	found := false
	for _, b := range backups {
		if b.Path != backupPath && readFile(t, b.Path) == "model = \"modified\"\n" {
			found = true
		}
	}
	if !found {
		t.Fatal("pre-restore backup does not hold the current config")
	}
}

func TestRestoreInvalidBackupRollsBack(t *testing.T) {
	svc, home, backupDir := newTestService(t)
	configPath := filepath.Join(home, "config.toml")
	original := "model = \"current\"\n"
	writeFile(t, configPath, original)
	if _, err := CreateBackup(backupDir, []byte("model = \"old\"\n")); err != nil {
		t.Fatal(err)
	}
	invalidName := backupName(backupTimes[1])
	writeBackupFile(t, backupDir, invalidName, []byte("model = \"unterminated\n"))
	snap, err := svc.Restore(context.Background(), invalidName)
	if k := kindOf(t, err); k != KindRestoreFailed {
		t.Fatalf("expected restore_failed, got %v (snap=%+v)", err, snap)
	}
	if got := readFile(t, configPath); got != original {
		t.Fatalf("config not rolled back after failed restore:\n%s", got)
	}
	// The failed restore leaves the selected + pre-restore backups in place.
	backups, _ := ListBackups(backupDir)
	if len(backups) != 3 {
		t.Fatalf("expected original + selected + pre-restore backups, got %d", len(backups))
	}
}

func TestRestoreNoBackups(t *testing.T) {
	svc, _, backupDir := newTestService(t)
	os.RemoveAll(backupDir)
	if _, err := svc.Restore(context.Background(), "20260805T103040123Z-config.toml"); kindOf(t, err) != KindNoBackups {
		t.Fatalf("expected no_backups, got %v", err)
	}
}

func TestRestoreBackupNotFound(t *testing.T) {
	svc, home, backupDir := newTestService(t)
	writeFile(t, filepath.Join(home, "config.toml"), "model = \"a\"\n")
	backupPath, err := CreateBackup(backupDir, []byte("model = \"b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	_ = backupPath
	if _, err := svc.Restore(context.Background(), "20260805T103040123Z-config.toml"); kindOf(t, err) != KindRestoreFailed {
		t.Fatalf("expected restore_failed for unknown id, got %v", err)
	}
}

func TestRestoreCreatesConfigWhenMissing(t *testing.T) {
	svc, home, backupDir := newTestService(t)
	backupPath, err := CreateBackup(backupDir, []byte("model = \"from-backup\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := svc.Restore(context.Background(), filepath.Base(backupPath))
	if err != nil {
		t.Fatalf("Restore failed: %v", err)
	}
	if !snap.Exists || snap.Model != "from-backup" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if got := readFile(t, filepath.Join(home, "config.toml")); got != "model = \"from-backup\"\n" {
		t.Fatalf("config not created from backup:\n%s", got)
	}
}

func TestListBackupsServiceEmptyWhenDirMissing(t *testing.T) {
	svc, _, _ := newTestService(t)
	backups, err := svc.ListBackups(context.Background())
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}
	if len(backups) != 0 {
		t.Fatalf("expected empty list, got %d", len(backups))
	}
}

func TestVerifyFieldsDetectsMismatch(t *testing.T) {
	if err := verifyFields([]byte("model = \"a\"\n"), []Field{{Key: "model", Value: "a"}}); err != nil {
		t.Fatalf("matching field should verify: %v", err)
	}
	if err := verifyFields([]byte("model = \"a\"\n"), []Field{{Key: "model", Value: "b"}}); err == nil {
		t.Fatal("mismatched field should fail verification")
	}
	if err := verifyFields([]byte("model = \"a\"\n"), []Field{{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://x"}}); err == nil {
		t.Fatal("missing field should fail verification")
	}
}

func TestFieldsFromInputRequiresAtLeastOneField(t *testing.T) {
	if _, err := fieldsFromInput(Input{}); kindOf(t, err) != KindValidationFailed {
		t.Fatalf("expected validation_failed, got %v", err)
	}
}
