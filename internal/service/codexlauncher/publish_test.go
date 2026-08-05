package codexlauncher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"moonbridge/internal/service/codexconfig"
)

const testAuthToken = "sk-test-publish-token-12345678"

func writeStagedHome(t *testing.T, staging string) {
	t.Helper()
	files := map[string]string{
		"config.toml":        "model = \"deepseek-v4-pro\"\nmodel_provider = \"deepseek\"\n",
		"models_catalog.json": `{"models":[{"id":"deepseek-v4-pro"}]}`,
		"auth.json":          fmt.Sprintf(`{"openai_api_key":%q}`, testAuthToken),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func newStagingFor(t *testing.T, targetHome string) string {
	t.Helper()
	staging, err := CreateStagingHome(targetHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(staging) })
	return staging
}

func readTargetFile(t *testing.T, home, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// assertNoBackoutDirs checks that PublishStaged removed its backout directory on
// every path. Staging directories are owned by the launcher (deferred removal),
// not by PublishStaged, so they are not asserted here.
func assertNoBackoutDirs(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".codex-home-backout") {
			t.Fatalf("backout dir left behind: %q", e.Name())
		}
	}
}

func TestPublishFirstRunCreatesThreeFiles(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)

	if err := PublishStaged(staging, targetHome, true); err != nil {
		t.Fatalf("PublishStaged failed: %v", err)
	}
	for _, name := range codexHomeFiles {
		if _, err := os.Stat(filepath.Join(targetHome, name)); err != nil {
			t.Fatalf("published file missing: %s", name)
		}
	}
	if got := readTargetFile(t, targetHome, "auth.json"); !strings.Contains(got, testAuthToken) {
		t.Fatal("auth.json did not carry the token")
	}
	assertNoBackoutDirs(t, parent)
}

func TestPublishReplacesExistingFiles(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range codexHomeFiles {
		if err := os.WriteFile(filepath.Join(targetHome, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)

	if err := PublishStaged(staging, targetHome, true); err != nil {
		t.Fatalf("PublishStaged failed: %v", err)
	}
	if got := readTargetFile(t, targetHome, "config.toml"); !strings.Contains(got, "deepseek-v4-pro") {
		t.Fatalf("config.toml not replaced: %q", got)
	}
	assertNoBackoutDirs(t, parent)
}

func TestPublishRollsBackOnMidTransactionFailure(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	old := map[string]string{
		"config.toml":         "model = \"old\"\n",
		"models_catalog.json": `{"old":true}`,
		"auth.json":           `{"openai_api_key":"sk-old-token-1234567890"}`,
	}
	for name, content := range old {
		if err := os.WriteFile(filepath.Join(targetHome, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)

	prev := atomicWriteFile
	atomicWriteFile = func(path string, data []byte) error {
		// Fail only the replace of the staged auth.json, not the restore of the
		// old auth.json content from the backout.
		if strings.Contains(path, "auth.json") && strings.Contains(string(data), testAuthToken) {
			return errors.New("simulated write failure")
		}
		return codexconfig.AtomicWrite(path, data)
	}
	defer func() { atomicWriteFile = prev }()

	err := PublishStaged(staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	if le.Details["rolledBack"] != true {
		t.Fatalf("expected rolledBack=true in Details: %v", le.Details)
	}
	if strings.Contains(le.Error(), testAuthToken) || strings.Contains(fmt.Sprint(le.Details), testAuthToken) {
		t.Fatal("publish error leaked the token")
	}
	for name, content := range old {
		if got := readTargetFile(t, targetHome, name); got != content {
			t.Fatalf("%s not restored: %q", name, got)
		}
	}
	assertNoBackoutDirs(t, parent)
}

func TestPublishRollsBackRemovesFilesWhenTargetWasAbsent(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)

	prev := atomicWriteFile
	atomicWriteFile = func(path string, data []byte) error {
		// Fail on the last replace: config.toml is the commit marker, so
		// models_catalog.json and auth.json have already been created.
		if strings.Contains(path, "config.toml") {
			return errors.New("simulated write failure")
		}
		return codexconfig.AtomicWrite(path, data)
	}
	defer func() { atomicWriteFile = prev }()

	err := PublishStaged(staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	entries, rerr := os.ReadDir(targetHome)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("rollback to absent must leave no files, got %v", names)
	}
	assertNoBackoutDirs(t, parent)
}

func TestPublishFailsWhenStagedMissingFile(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	// No files staged at all.
	err := PublishStaged(staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) || le.Kind != KindConfigPublishFailed {
		t.Fatalf("expected KindConfigPublishFailed, got %v", err)
	}
	if strings.Contains(le.Error(), testAuthToken) {
		t.Fatal("publish error leaked the token")
	}
	assertNoBackoutDirs(t, parent)
}

func TestGenerateAndVerifyRejectsInvalidConfig(t *testing.T) {
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "config.toml"), []byte("model = \"unterminated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "models_catalog.json"), []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "auth.json"), []byte(`{"openai_api_key":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAndVerify(staging, func(string) error { return nil }, true); err == nil {
		t.Fatal("expected invalid config to be rejected")
	}
}

func TestGenerateAndVerifyRejectsMissingAuthKey(t *testing.T) {
	staging := t.TempDir()
	files := map[string]string{
		"config.toml":         "model = \"a\"\n",
		"models_catalog.json": `{"models":[]}`,
		"auth.json":           `{"foo":"bar"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := GenerateAndVerify(staging, func(string) error { return nil }, true); err == nil {
		t.Fatal("expected missing openai_api_key to be rejected")
	}
}

func TestVerifyHomeRejectsPermissiveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX permission bits")
	}
	staging := t.TempDir()
	writeStagedHome(t, staging)
	// Loosen config.toml so the mode check has something to reject.
	if err := os.Chmod(filepath.Join(staging, "config.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyHome(staging, true); err == nil {
		t.Fatal("expected permissive mode to be rejected")
	}
	if err := os.Chmod(filepath.Join(staging, "config.toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyHome(staging, true); err != nil {
		t.Fatalf("0600 mode should verify: %v", err)
	}
}

// writeStagedHomeNoAuth writes a token-less staging set (two files: no auth.json).
func writeStagedHomeNoAuth(t *testing.T, staging string) {
	t.Helper()
	files := map[string]string{
		"config.toml":         "model = \"deepseek-v4-pro\"\nmodel_provider = \"deepseek\"\n",
		"models_catalog.json": `{"models":[{"id":"deepseek-v4-pro"}]}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// An empty token is normal: the codex-home is a two-file set and auth.json must
// not be required.
func TestPublishTokenlessUsesTwoFileSet(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging)

	if err := PublishStaged(staging, targetHome, false); err != nil {
		t.Fatalf("PublishStaged(tokenless) failed: %v", err)
	}
	for _, name := range []string{"config.toml", "models_catalog.json"} {
		if _, err := os.Stat(filepath.Join(targetHome, name)); err != nil {
			t.Fatalf("published file missing: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(targetHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth.json must be absent on a token-less publish, got err=%v", err)
	}
	assertNoBackoutDirs(t, parent)
}

// A stale auth.json from a prior token-bearing publish must be removed within the
// token-less transaction so Codex does not keep using an old token.
func TestPublishTokenlessRemovesStaleAuthJSON(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	oldAuth := `{"openai_api_key":"sk-stale-token-1234567890"}`
	if err := os.WriteFile(filepath.Join(targetHome, "auth.json"), []byte(oldAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging)

	if err := PublishStaged(staging, targetHome, false); err != nil {
		t.Fatalf("PublishStaged(tokenless, stale auth) failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("stale auth.json must be removed on a token-less publish, err=%v", err)
	}
	if got := readTargetFile(t, targetHome, "config.toml"); !strings.Contains(got, "deepseek-v4-pro") {
		t.Fatalf("config.toml not published: %q", got)
	}
	assertNoBackoutDirs(t, parent)
}

// If a token-less publish fails after removing the stale auth.json, the rollback
// must restore the original auth.json from the backout.
func TestPublishTokenlessRollbackRestoresRemovedAuthJSON(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	oldAuth := `{"openai_api_key":"sk-stale-token-1234567890"}`
	if err := os.WriteFile(filepath.Join(targetHome, "auth.json"), []byte(oldAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging)

	prev := atomicWriteFile
	atomicWriteFile = func(path string, data []byte) error {
		// Fail writing the commit marker (config.toml) after auth.json removal.
		if strings.Contains(path, "config.toml") {
			return errors.New("simulated write failure")
		}
		return codexconfig.AtomicWrite(path, data)
	}
	defer func() { atomicWriteFile = prev }()

	err := PublishStaged(staging, targetHome, false)
	if err == nil {
		t.Fatal("expected a token-less publish failure")
	}
	if got := readTargetFile(t, targetHome, "auth.json"); got != oldAuth {
		t.Fatalf("removed auth.json not restored on rollback: %q", got)
	}
	assertNoBackoutDirs(t, parent)
}

// A token bearer publish without an auth.json in staging is rejected: requireAuth
// still demands the openai_api_key.
func TestPublishTokenBearRequiresAuthJSON(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	staging := newStagingFor(t, targetHome)
	writeStagedHomeNoAuth(t, staging) // no auth.json in staging

	err := PublishStaged(staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a token-bearer publish without auth.json to fail")
	}
	var le *Error
	if !errors.As(err, &le) {
		t.Fatalf("expected *Error, got %v", err)
	}
	assertNoBackoutDirs(t, parent)
}

func TestPublishRollbackFailureReportedInDetails(t *testing.T) {
	parent := t.TempDir()
	targetHome := filepath.Join(parent, "codex-home")
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing files force the restore path through the atomicWriteFile seam.
	for _, name := range codexHomeFiles {
		if err := os.WriteFile(filepath.Join(targetHome, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staging := newStagingFor(t, targetHome)
	writeStagedHome(t, staging)

	prev := atomicWriteFile
	atomicWriteFile = func(path string, data []byte) error {
		// Fail the commit AND make the rollback restore fail too.
		return errors.New("simulated write failure")
	}
	defer func() { atomicWriteFile = prev }()

	err := PublishStaged(staging, targetHome, true)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	var le *Error
	if !errors.As(err, &le) {
		t.Fatalf("expected *Error, got %v", err)
	}
	if le.Details["rolledBack"] != false {
		t.Fatalf("expected rolledBack=false when restore fails: %v", le.Details)
	}
	if _, ok := le.Details["rollbackError"]; !ok {
		t.Fatalf("expected rollbackError detail: %v", le.Details)
	}
}
