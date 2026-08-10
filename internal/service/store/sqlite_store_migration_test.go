package store_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"moonbridge/internal/secretstore"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/store"
)

// migrationFakeCodec encrypts with the dpapi:v1: prefix. When failPlaintext is
// set, encrypting exactly that value fails (simulating a platform/DPAPI error).
type migrationFakeCodec struct {
	supported     bool
	failPlaintext string
}

func (f migrationFakeCodec) Encrypt(plaintext string) (string, error) {
	if !f.supported {
		return "", secretstore.ErrUnsupported
	}
	if plaintext == "" {
		return "", nil
	}
	if f.failPlaintext != "" && plaintext == f.failPlaintext {
		return "", errors.New("simulated encrypt failure")
	}
	return "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (f migrationFakeCodec) Decrypt(stored string) (string, error) {
	if !f.supported {
		return "", secretstore.ErrUnsupported
	}
	if !secretstore.IsCiphertext(stored) {
		return "", errors.New("not ciphertext")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "dpapi:v1:"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (f migrationFakeCodec) Supported() bool { return f.supported }

// newSQLiteStoreForTest returns the concrete store plus the raw table access so
// tests can assert exact DB state (ciphertext written / plaintext preserved).
func newSQLiteStoreForTest(t *testing.T) (*store.SQLiteConfigStore, *testDBStore) {
	t.Helper()
	logger := testLogger(t)
	c := store.NewConfigStoreConsumer(logger)
	ts := newTestStore(t, "config_store", c.Tables())
	if err := c.BindStore(ts); err != nil {
		t.Fatalf("BindStore() error = %v", err)
	}
	s, ok := c.Store().(*store.SQLiteConfigStore)
	if !ok {
		t.Fatal("Store() is not *store.SQLiteConfigStore")
	}
	return s, ts
}

func rawProviderAPIKey(t *testing.T, ts *testDBStore, providerKey string) string {
	t.Helper()
	table, err := ts.Table("providers")
	if err != nil {
		t.Fatal(err)
	}
	var apiKey string
	if err := ts.QueryRowContext(context.Background(),
		"SELECT api_key FROM "+table+" WHERE key = ?", providerKey).Scan(&apiKey); err != nil {
		t.Fatalf("query raw api_key for %q: %v", providerKey, err)
	}
	return apiKey
}

func TestSQLiteStoreLegacyPlaintextKeyMigration(t *testing.T) {
	s, ts := newSQLiteStoreForTest(t)
	cfg := buildTestConfig() // plaintext keys; codec not yet injected
	if err := s.SeedFromConfig(cfg); err != nil {
		t.Fatalf("SeedFromConfig() error = %v", err)
	}
	// Confirm a legacy DB actually holds the plaintext.
	plain := cfg.ProviderDefs["anthropic"].APIKey
	if got := rawProviderAPIKey(t, ts, "anthropic"); got != plain {
		t.Fatalf("raw api_key before migration = %q, want %q", got, plain)
	}

	s.SetCodec(migrationFakeCodec{supported: true})
	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() error = %v", err)
	}
	key := loaded.ProviderDefs["anthropic"].APIKey
	if !secretstore.IsCiphertext(key) {
		t.Fatalf("api_key after migration = %q, want ciphertext", key)
	}
	if key == plain {
		t.Fatal("api_key unchanged after migration")
	}
	// DB row replaced with ciphertext.
	if got := rawProviderAPIKey(t, ts, "anthropic"); got != key {
		t.Fatalf("raw api_key after migration = %q, want %q", got, key)
	}
	// Round-trips back to the original plaintext.
	dec, err := (migrationFakeCodec{supported: true}).Decrypt(key)
	if err != nil {
		t.Fatalf("Decrypt(migrated) error = %v", err)
	}
	if dec != plain {
		t.Fatalf("round trip = %q, want %q", dec, plain)
	}
	if issues := s.LastMigrationIssues(); len(issues) != 0 {
		t.Fatalf("LastMigrationIssues() = %v, want empty", issues)
	}
}

func TestSQLiteStoreLegacyMigrationPreservesFailedPlaintext(t *testing.T) {
	s, ts := newSQLiteStoreForTest(t)
	cfg := buildTestConfig()
	badKey := "sk-fail-me"
	openaiDef := cfg.ProviderDefs["openai"]
	openaiDef.APIKey = badKey
	cfg.ProviderDefs["openai"] = openaiDef
	if err := s.SeedFromConfig(cfg); err != nil {
		t.Fatalf("SeedFromConfig() error = %v", err)
	}

	s.SetCodec(migrationFakeCodec{supported: true, failPlaintext: badKey})
	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() error = %v", err)
	}

	// Successful provider: ciphertext in FileConfig and DB.
	antKey := loaded.ProviderDefs["anthropic"].APIKey
	if !secretstore.IsCiphertext(antKey) {
		t.Fatalf("anthropic api_key = %q, want ciphertext", antKey)
	}
	if got := rawProviderAPIKey(t, ts, "anthropic"); got != antKey {
		t.Fatalf("anthropic raw api_key = %q, want %q", got, antKey)
	}
	// Failed provider: key left intact (graph stays valid) and DB plaintext
	// preserved (recoverable). The resolver refuses to start it with the
	// plaintext; the issue records stored/unavailable/migration_failed.
	if got := loaded.ProviderDefs["openai"].APIKey; got != "" {
		t.Fatalf("openai api_key after failed migration = %q, want empty", got)
	}
	if got := rawProviderAPIKey(t, ts, "openai"); got != badKey {
		t.Fatalf("openai raw api_key = %q, want preserved %q", got, badKey)
	}
	// Issue recorded with source=stored / unavailable / migration_failed.
	var found *provider.CredentialInfo
	for _, iss := range s.LastMigrationIssues() {
		if iss.ProviderID == "openai" {
			found = &iss
		}
	}
	if found == nil {
		t.Fatal("no migration_failed issue recorded for openai")
	}
	if found.Source != provider.SourceStored || found.State != provider.StateUnavailable || found.ErrorCode != provider.ErrCodeMigrationFailed {
		t.Fatalf("issue = %+v, want stored/unavailable/migration_failed", *found)
	}
}

func TestSQLiteStoreLegacyMigrationUnsupportedCodec(t *testing.T) {
	s, ts := newSQLiteStoreForTest(t)
	cfg := buildTestConfig()
	if err := s.SeedFromConfig(cfg); err != nil {
		t.Fatalf("SeedFromConfig() error = %v", err)
	}

	s.SetCodec(migrationFakeCodec{supported: false})
	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() error = %v", err)
	}
	// On unsupported platforms the stored key is unusable. The graph stays
	// valid (key intact); the resolver refuses it and the issue records
	// unsupported_platform so env-only use can take over.
	if got := loaded.ProviderDefs["anthropic"].APIKey; got != "" {
		t.Fatalf("anthropic api_key = %q, want empty", got)
	}
	// DB plaintext is preserved (recoverable), never overwritten.
	if got := rawProviderAPIKey(t, ts, "anthropic"); got != cfg.ProviderDefs["anthropic"].APIKey {
		t.Fatalf("anthropic raw api_key = %q, want preserved plaintext", got)
	}
	var found *provider.CredentialInfo
	for _, iss := range s.LastMigrationIssues() {
		if iss.ProviderID == "anthropic" {
			found = &iss
		}
	}
	if found == nil {
		t.Fatal("no unsupported_platform issue recorded")
	}
	if found.ErrorCode != provider.ErrCodeUnsupportedPlatform {
		t.Fatalf("issue = %+v, want unsupported_platform", *found)
	}
}

func TestSQLiteStoreWriteEncryptsPlaintextKeys(t *testing.T) {
	s, ts := newSQLiteStoreForTest(t)
	s.SetCodec(migrationFakeCodec{supported: true})
	cfg := buildTestConfig() // plaintext keys

	if _, err := s.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	raw := rawProviderAPIKey(t, ts, "anthropic")
	if !secretstore.IsCiphertext(raw) {
		t.Fatalf("raw api_key after SaveConfig = %q, want ciphertext", raw)
	}
	if strings.Contains(raw, cfg.ProviderDefs["anthropic"].APIKey) {
		t.Fatal("DB api_key leaks plaintext")
	}
	// A ciphertext-only SaveConfig is not double-encrypted.
	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() error = %v", err)
	}
	again := loaded.ProviderDefs["anthropic"].APIKey
	if _, err := s.SaveConfig(context.Background(), loaded); err != nil {
		t.Fatalf("second SaveConfig() error = %v", err)
	}
	if got := rawProviderAPIKey(t, ts, "anthropic"); got != again {
		t.Fatalf("raw api_key after ciphertext save = %q, want unchanged %q", got, again)
	}
}
