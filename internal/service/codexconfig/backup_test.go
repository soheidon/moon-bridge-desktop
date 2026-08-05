package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var backupTimes = []time.Time{
	time.Date(2026, 8, 5, 10, 30, 40, 123000000, time.UTC),
	time.Date(2026, 8, 5, 9, 20, 30, 456000000, time.UTC),
	time.Date(2026, 8, 5, 8, 10, 20, 789000000, time.UTC),
	time.Date(2026, 8, 4, 7, 0, 10, 111000000, time.UTC),
	time.Date(2026, 8, 4, 6, 0, 0, 222000000, time.UTC),
	time.Date(2026, 8, 4, 5, 0, 0, 333000000, time.UTC),
	time.Date(2026, 8, 4, 4, 0, 0, 444000000, time.UTC),
}

func TestBackupNameFormatAndRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 30, 40, 123000000, time.UTC)
	name := backupName(now)
	want := "20260805T103040123Z-config.toml"
	if name != want {
		t.Fatalf("unexpected backup name %q, want %q", name, want)
	}
	stem, ok := configBackupStem(name)
	if !ok {
		t.Fatalf("name not recognized: %q", name)
	}
	parsed, ok := parseBackupTimestamp(stem)
	if !ok || !parsed.Equal(now) {
		t.Fatalf("round-trip mismatch: %v vs %v (ok=%v)", parsed, now, ok)
	}
}

func TestConfigBackupStemValidation(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"20260805T103040123Z-config.toml", true},
		{"20260805T103040123Z", false},              // missing suffix
		{"20260805T10304012Z-config.toml", false},   // short stem
		{"2026080T103040123Z-config.toml", false},   // missing T
		{"20260805T10304012Z-config.toml", false},   // too short time
		{"20260805T103040123-config.toml", false},   // missing Z
		{"20260805x103040123Z-config.toml", false},  // non-digit
		{"2026-0805T103040123Z-config.toml", false}, // non-digit
		{"foo-config.toml", false},
		{"-config.toml", false},
	}
	for _, c := range cases {
		if _, ok := configBackupStem(c.name); ok != c.ok {
			t.Errorf("configBackupStem(%q) = %v, want %v", c.name, ok, c.ok)
		}
	}
}

func writeBackupFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBackupWritesContent(t *testing.T) {
	dir := t.TempDir()
	content := []byte("model = \"a\"\n")
	path, err := CreateBackup(dir, content)
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("backup not in dir: %q", path)
	}
	if _, ok := configBackupStem(filepath.Base(path)); !ok {
		t.Fatalf("backup name %q invalid", filepath.Base(path))
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(content) {
		t.Fatalf("backup content mismatch: %q err=%v", got, err)
	}
}

func TestCreateBackupCreatesAnotherNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreateBackup(dir, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateBackup(dir, []byte("b")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected two distinct backups, got %d", len(entries))
	}
}

func TestListBackupsNewestFirstSkipsForeign(t *testing.T) {
	dir := t.TempDir()
	for _, ts := range backupTimes {
		writeBackupFile(t, dir, backupName(ts), []byte("x"))
	}
	writeBackupFile(t, dir, "notes.txt", []byte("junk"))
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	backups, err := ListBackups(dir)
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}
	if len(backups) != len(backupTimes) {
		t.Fatalf("expected %d backups, got %d", len(backupTimes), len(backups))
	}
	for i := 1; i < len(backups); i++ {
		if backups[i-1].CreatedAt.Before(backups[i].CreatedAt) {
			t.Fatalf("backups not newest-first: %v then %v", backups[i-1].CreatedAt, backups[i].CreatedAt)
		}
	}
	if backups[0].ID != backupName(backupTimes[0]) {
		t.Fatalf("newest not first: %q", backups[0].ID)
	}
	if backups[0].Size <= 0 {
		t.Fatalf("size not reported: %d", backups[0].Size)
	}
}

func TestRetainConfigBackupsKeepsNewestFive(t *testing.T) {
	dir := t.TempDir()
	for _, ts := range backupTimes {
		writeBackupFile(t, dir, backupName(ts), []byte("x"))
	}
	newest := backupName(backupTimes[0])
	retainConfigBackups(dir, newest)
	remaining, err := ListBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 5 {
		t.Fatalf("expected 5 retained, got %d", len(remaining))
	}
	if remaining[0].ID != newest {
		t.Fatalf("newest backup removed: %q not first", newest)
	}
	for _, b := range remaining {
		if b.ID == backupName(backupTimes[5]) || b.ID == backupName(backupTimes[6]) {
			t.Fatalf("old backup survived retention: %q", b.ID)
		}
	}
}

func TestResolveBackupPathValid(t *testing.T) {
	dir := t.TempDir()
	id := backupName(backupTimes[0])
	got, err := ResolveBackupPath(dir, id)
	if err != nil {
		t.Fatalf("ResolveBackupPath failed: %v", err)
	}
	if got != filepath.Join(dir, id) {
		t.Fatalf("unexpected path %q", got)
	}
}

func TestResolveBackupPathRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{
		"..",
		"..\\..\\config.toml",
		"../config.toml",
		"20260805T103040123Z-config.toml/../x",
		"C:\\Windows\\system32\\config.toml",
		"sub\\20260805T103040123Z-config.toml",
		"",
		"not-a-backup.toml",
	} {
		if _, err := ResolveBackupPath(dir, id); err == nil {
			t.Errorf("expected rejection for id %q", id)
		}
	}
}

func TestListBackupsMissingDirIsEmpty(t *testing.T) {
	backups, err := ListBackups(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if len(backups) != 0 {
		t.Fatalf("expected empty list, got %d", len(backups))
	}
}

func TestAtomicWriteReplacesAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("new")); err != nil {
		t.Fatalf("AtomicWrite failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("content mismatch: %q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %q", e.Name())
		}
	}
}

func TestAtomicWriteCreatesMissingDirs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "config.toml")
	if err := AtomicWrite(path, []byte("x")); err != nil {
		t.Fatalf("AtomicWrite failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestDefaultBackupDir(t *testing.T) {
	lookup := func(key string) string {
		switch key {
		case "LOCALAPPDATA":
			return `C:\Users\test\AppData\Local`
		}
		return ""
	}
	got, err := defaultBackupDir(lookup)
	if err != nil {
		t.Fatalf("defaultBackupDir failed: %v", err)
	}
	want := filepath.Join(`C:\Users\test\AppData\Local`, "Moon Bridge", "backups", "codex-config")
	if got != want {
		t.Fatalf("unexpected backup dir %q, want %q", got, want)
	}
	if _, err := defaultBackupDir(func(string) string { return "" }); err == nil {
		t.Fatal("expected error when no base env var set")
	}
}
