package codexconfig

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	dir := newBackupTestDir(t)
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
	dir := newBackupTestDir(t)
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

func TestRetainConfigBackupsProtectsActiveReferenceOutsideLimit(t *testing.T) {
	dir := t.TempDir()
	for _, ts := range backupTimes {
		writeBackupFile(t, dir, backupName(ts), []byte("x"))
	}
	oldProtected := backupName(backupTimes[len(backupTimes)-1])
	newest := backupName(backupTimes[0])
	retainConfigBackups(dir, oldProtected, newest)
	remaining, err := ListBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 6 {
		t.Fatalf("expected five ordinary plus protected backup, got %d", len(remaining))
	}
	seen := make(map[string]bool, len(remaining))
	for _, backup := range remaining {
		seen[backup.ID] = true
	}
	if !seen[oldProtected] || !seen[newest] {
		t.Fatalf("protected backups were trimmed: %v", seen)
	}
	if seen[backupName(backupTimes[len(backupTimes)-2])] {
		t.Fatal("unprotected oldest backup was retained")
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

type backupFake struct {
	events            []string
	fail              string
	failDelete        bool
	writes            int
	closes            int
	deletes           int
	collisions        int // createFile attempts reporting errBackupExists before succeeding
	createCalls       int
	successfulCreates int
	names             []string
	writePartial      bool
}

type backupFakeRoot struct{}
type backupFakeFile struct{}

func (f *backupFake) record(event string) { f.events = append(f.events, event) }
func (f *backupFake) openRoot(string) (backupRoot, error) {
	f.record("root open")
	if f.fail == "root open" {
		return nil, errors.New("root failure")
	}
	return backupFakeRoot{}, nil
}
func (f *backupFake) verifyRoot(backupRoot) error {
	f.record("root reparse/identity verify")
	if f.fail == "root verify" {
		return errors.New("root verify failure")
	}
	return nil
}
func (f *backupFake) applyRootSecurity(backupRoot) error {
	f.record("root ACL apply")
	if f.fail == "root ACL apply" {
		return errors.New("root ACL failure")
	}
	return nil
}
func (f *backupFake) verifyRootSecurity(backupRoot) error {
	f.record("root ACL verify")
	if f.fail == "root ACL verify" {
		return errors.New("root ACL verify failure")
	}
	return nil
}
func (f *backupFake) createFile(_ backupRoot, name string) (backupFile, error) {
	f.record("file create-new")
	f.createCalls++
	f.names = append(f.names, name)
	if f.fail == "file create" {
		return nil, errors.New("file create failure")
	}
	if f.collisions > 0 {
		f.collisions--
		return nil, errBackupExists
	}
	f.successfulCreates++
	return backupFakeFile{}, nil
}
func (f *backupFake) verifyFile(backupFile, backupRoot, string) error {
	f.record("file reparse/identity verify")
	if f.fail == "file verify" {
		return errors.New("file verify failure")
	}
	return nil
}
func (f *backupFake) applyFileSecurity(backupFile) error {
	f.record("file ACL apply")
	if f.fail == "file ACL apply" {
		return errors.New("file ACL failure")
	}
	return nil
}
func (f *backupFake) verifyFileSecurity(backupFile) error {
	f.record("file ACL verify")
	if f.fail == "file ACL verify" {
		return errors.New("file ACL verify failure")
	}
	return nil
}
func (f *backupFake) write(backupFile, []byte) error {
	f.record("write")
	f.writes++
	if f.writePartial {
		return errors.New("short write")
	}
	if f.fail == "write" {
		return errors.New("write failure")
	}
	return nil
}
func (f *backupFake) sync(backupFile) error {
	f.record("sync")
	if f.fail == "sync" {
		return errors.New("sync failure")
	}
	return nil
}
func (f *backupFake) deleteOnClose(backupFile) error {
	f.record("delete")
	f.deletes++
	if f.failDelete || f.fail == "delete" {
		return errors.New("delete failure")
	}
	return nil
}
func (f *backupFake) retain(backupRoot, string, []string) error {
	f.record("retain")
	if f.fail == "retain" {
		return errors.New("retain failure")
	}
	return nil
}
func (f *backupFake) close(backupRoot, backupFile) error {
	f.record("close")
	f.closes++
	if f.fail == "close" {
		return errors.New("close failure")
	}
	return nil
}

func TestCreateBackupWithSingleCloseAndOrder(t *testing.T) {
	f := &backupFake{}
	path, err := createBackupWith(t.TempDir(), []byte("payload"), f)
	if err != nil {
		t.Fatalf("createBackupWith: %v", err)
	}
	if path == "" || f.closes != 1 {
		t.Fatalf("path=%q closes=%d", path, f.closes)
	}
	want := []string{"root open", "root reparse/identity verify", "root ACL apply", "root ACL verify", "file create-new", "file reparse/identity verify", "file ACL apply", "file ACL verify", "write", "sync", "retain", "close"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events=%v want=%v", f.events, want)
	}
}

func TestCreateBackupWithFailureNeverWritesBeforeVerification(t *testing.T) {
	preCreate := map[string]bool{
		"root open": true, "root verify": true, "root ACL apply": true,
		"root ACL verify": true, "file create": true,
	}
	for _, stage := range []string{"root open", "root verify", "root ACL apply", "root ACL verify", "file create", "file verify", "file ACL apply", "file ACL verify"} {
		t.Run(stage, func(t *testing.T) {
			f := &backupFake{fail: stage}
			path, err := createBackupWith(t.TempDir(), []byte("secret-sentinel"), f)
			if err == nil {
				t.Fatal("expected failure")
			}
			if f.writes != 0 {
				t.Fatalf("writes=%d", f.writes)
			}
			if path != "" {
				t.Fatalf("path=%q returned on failure", path)
			}
			if strings.Contains(err.Error(), "secret-sentinel") {
				t.Fatal("secret leaked")
			}
			wantCloses := 1
			if stage == "root open" { // no handle was opened, so none to close
				wantCloses = 0
			}
			if f.closes != wantCloses {
				t.Fatalf("closes=%d, want %d", f.closes, wantCloses)
			}
			if preCreate[stage] {
				if f.deletes != 0 {
					t.Fatalf("pre-create failure deleted an artifact: deletes=%d", f.deletes)
				}
			} else if f.deletes != 1 {
				t.Fatalf("post-create failure did not delete the artifact: deletes=%d", f.deletes)
			}
			// A root-stage failure (open/verify/ACL apply/ACL verify) must never
			// reach createFile, so no backup file is created (C.44/C.45).
			wantCreates := 1
			if strings.HasPrefix(stage, "root") {
				wantCreates = 0
			}
			if f.createCalls != wantCreates {
				t.Fatalf("createCalls=%d, want %d for stage %q", f.createCalls, wantCreates, stage)
			}
		})
	}
}

func TestBackupFakeSeamIsPerInstance(t *testing.T) {
	a := &backupFake{}
	b := &backupFake{}
	a.record("write")
	a.writes++
	if len(b.events) != 0 || b.writes != 0 {
		t.Fatal("failure-injection seam is shared across instances")
	}
}

func TestCreateBackupWithCollisionRetriesWithFreshNameExactlyOnce(t *testing.T) {
	f := &backupFake{collisions: 2}
	path, err := createBackupWith(t.TempDir(), []byte("payload"), f)
	if err != nil {
		t.Fatalf("createBackupWith: %v", err)
	}
	if path == "" {
		t.Fatal("empty path on success")
	}
	if f.successfulCreates != 1 {
		t.Fatalf("successful creates=%d, want exactly 1", f.successfulCreates)
	}
	if f.createCalls != 3 {
		t.Fatalf("create attempts=%d, want 3 (2 collisions + 1 success)", f.createCalls)
	}
	if len(f.names) != 3 || f.names[0] == f.names[1] || f.names[1] == f.names[2] {
		t.Fatalf("retries did not use fresh names: %v", f.names)
	}
	if f.deletes != 0 {
		t.Fatalf("collision cleanup deleted an artifact: deletes=%d", f.deletes)
	}
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
}

func TestCreateBackupWithCollisionExhaustionIsFailure(t *testing.T) {
	f := &backupFake{collisions: 3}
	path, err := createBackupWith(t.TempDir(), []byte("payload"), f)
	if err == nil {
		t.Fatal("expected failure after 3 collisions")
	}
	if path != "" {
		t.Fatalf("path=%q returned on collision exhaustion", path)
	}
	if f.createCalls != 3 {
		t.Fatalf("create attempts=%d, want 3", f.createCalls)
	}
	if f.successfulCreates != 0 {
		t.Fatalf("successful creates=%d, want 0", f.successfulCreates)
	}
	if f.deletes != 0 {
		t.Fatalf("deletes=%d", f.deletes)
	}
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
}

func TestCreateBackupWithNonCollisionCreateErrorDoesNotRetry(t *testing.T) {
	f := &backupFake{fail: "file create"}
	path, err := createBackupWith(t.TempDir(), []byte("payload"), f)
	if err == nil {
		t.Fatal("expected failure")
	}
	if path != "" {
		t.Fatalf("path=%q returned on failure", path)
	}
	if f.createCalls != 1 {
		t.Fatalf("create attempts=%d, want 1 (non-collision errors must not retry)", f.createCalls)
	}
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
}

func TestCreateBackupWithPartialWriteFailsSafely(t *testing.T) {
	f := &backupFake{writePartial: true}
	path, err := createBackupWith(t.TempDir(), []byte("secret-sentinel"), f)
	if err == nil {
		t.Fatal("expected partial write failure")
	}
	if path != "" {
		t.Fatalf("path=%q returned on partial write", path)
	}
	if f.writes != 1 {
		t.Fatalf("writes=%d, want 1 (the partial attempt)", f.writes)
	}
	if f.deletes != 1 {
		t.Fatalf("deletes=%d, want identity-safe delete of the partial artifact", f.deletes)
	}
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
	if strings.Contains(err.Error(), "secret-sentinel") {
		t.Fatal("secret leaked")
	}
}

func TestCreateBackupDoesNotOverwriteOrMutateExisting(t *testing.T) {
	dir := newBackupTestDir(t)
	existing := backupName(backupTimes[0])
	existingContent := []byte("existing-backup-content")
	writeBackupFile(t, dir, existing, existingContent)
	writeBackupFile(t, dir, "notes.txt", []byte("unrelated"))

	path, err := CreateBackup(dir, []byte("new-payload"))
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if path == "" || filepath.Base(path) == existing {
		t.Fatalf("new backup reused the existing name: %q", path)
	}
	gotExisting, err := os.ReadFile(filepath.Join(dir, existing))
	if err != nil || !reflect.DeepEqual(gotExisting, existingContent) {
		t.Fatalf("existing backup content mutated: %q err=%v", gotExisting, err)
	}
	if sha256.Sum256(gotExisting) != sha256.Sum256(existingContent) {
		t.Fatal("existing backup SHA-256 changed")
	}
	info, err := os.Stat(filepath.Join(dir, existing))
	if err != nil || info.Size() != int64(len(existingContent)) {
		t.Fatalf("existing backup size changed: size=%d err=%v", info.Size(), err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "notes.txt")); err != nil || string(got) != "unrelated" {
		t.Fatalf("unrelated file changed: %q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(entries), entries)
	}
}

type failStagePlatform struct {
	backupPlatformOps
	stage string
}

func (p *failStagePlatform) write(f backupFile, data []byte) error {
	if p.stage == "write" {
		return errors.New("injected write failure")
	}
	return p.backupPlatformOps.write(f, data)
}

func TestCreateBackupWithFailureKeepsUnrelatedBackup(t *testing.T) {
	dir := t.TempDir()
	unrelated := backupName(backupTimes[1])
	writeBackupFile(t, dir, unrelated, []byte("keep-me"))

	p := &failStagePlatform{backupPlatformOps: createBackupPlatform(), stage: "write"}
	path, err := createBackupWith(dir, []byte("secret-sentinel"), p)
	if err == nil {
		t.Fatal("expected write failure")
	}
	if path != "" {
		t.Fatalf("path=%q returned on failure", path)
	}
	if strings.Contains(err.Error(), "secret-sentinel") || strings.Contains(err.Error(), dir) {
		t.Fatalf("unsafe error: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("backup root was removed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, unrelated)); err != nil || string(got) != "keep-me" {
		t.Fatalf("unrelated backup changed: %q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("failure cleanup left extra artifacts: %v", entries)
	}
}

func TestCreateBackupWithRetentionFailureClosesOnce(t *testing.T) {
	f := &backupFake{fail: "retain"}
	if _, err := createBackupWith(t.TempDir(), []byte("payload"), f); err == nil {
		t.Fatal("expected retention failure")
	}
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
}

func TestCreateBackupWithCleanupFailureIsSafe(t *testing.T) {
	dir := t.TempDir()
	for _, stage := range []string{"delete", "close", "sync", "write"} {
		t.Run(stage, func(t *testing.T) {
			f := &backupFake{fail: stage, failDelete: stage == "delete"}
			if stage == "delete" {
				f.fail = "write"
			}
			path, err := createBackupWith(dir, []byte("secret-sentinel"), f)
			if err == nil || err.Error() == "" {
				t.Fatal("expected safe failure")
			}
			if strings.Contains(err.Error(), "secret-sentinel") || strings.Contains(err.Error(), dir) {
				t.Fatalf("unsafe error: %v", err)
			}
			if path != "" {
				t.Fatalf("path=%q returned on failure", path)
			}
			if f.closes != 1 {
				t.Fatalf("closes=%d", f.closes)
			}
			if stage == "delete" || stage == "close" {
				if !strings.Contains(err.Error(), backupCleanupFailed) {
					t.Fatalf("cleanup failure not surfaced as safe code: %v", err)
				}
			} else if f.deletes != 1 {
				t.Fatalf("deletes=%d, want identity-safe delete after %s failure", f.deletes, stage)
			}
		})
	}
}

// G.80: a deleteOnClose failure must keep the secret artifact (still carrying
// its protected DACL — proven separately by the Windows real-API tests) in
// place, return no success, and surface only the fixed backup_cleanup_failed
// code. Real-API delete failure is not safely inducible in-process (the backup
// file handle always holds DELETE access, so any coexisting handle must grant
// FILE_SHARE_DELETE — bidirectional sharing rule), so the delete-failure path
// is pinned here via the fake seam while the protected-DACL property is pinned
// by TestWindowsCreateBackupRealAPIAndDACL and the pending-delete test.
func TestCreateBackupWithDeleteFailureKeepsArtifactAndIsCleanupSafe(t *testing.T) {
	dir := t.TempDir()
	for _, stage := range []string{"write", "sync"} {
		t.Run(stage, func(t *testing.T) {
			f := &backupFake{fail: stage, failDelete: true}
			path, err := createBackupWith(dir, []byte("secret-sentinel"), f)
			if err == nil {
				t.Fatal("expected cleanup failure")
			}
			// File security verification succeeded before the failing stage; the
			// exact event sequence below fixes the ACL-verify-before-write order.
			if path != "" {
				t.Fatalf("path=%q returned on cleanup failure", path)
			}
			// deleteOnClose was called exactly once and failed; only then is the
			// handle closed, each at most once.
			if f.deletes != 1 {
				t.Fatalf("deletes=%d, want exactly 1 deleteOnClose attempt", f.deletes)
			}
			if f.closes != 1 {
				t.Fatalf("closes=%d, want exactly 1 close", f.closes)
			}
			// Fixed safe code only, never the original stage error or any secret,
			// path, filename, SID, or username material.
			if err.Error() != backupCleanupFailed {
				t.Fatalf("err=%q, want exactly %q", err, backupCleanupFailed)
			}
			for _, bad := range []string{"failure", "secret-sentinel", dir, "-config.toml", "S-1-"} {
				if strings.Contains(err.Error(), bad) {
					t.Fatalf("unsafe error leaks %q: %q", bad, err)
				}
			}
			// No retention, no ACL re-apply/relax/delete after the cleanup
			// failure: the only events after the failing stage are the single
			// delete attempt and the single close.
			want := []string{
				"root open", "root reparse/identity verify", "root ACL apply", "root ACL verify",
				"file create-new", "file reparse/identity verify", "file ACL apply", "file ACL verify",
				"write",
			}
			if stage == "sync" {
				want = append(want, "sync")
			}
			want = append(want, "delete", "close")
			if !reflect.DeepEqual(f.events, want) {
				t.Fatalf("events=%v want=%v", f.events, want)
			}
		})
	}
}
