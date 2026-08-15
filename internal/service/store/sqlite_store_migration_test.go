package store_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"moonbridge/internal/config"
	routingprofiles "moonbridge/internal/extension/routingprofiles"
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

// newSQLiteStoreWithRoutingProfiles returns a concrete store whose consumer has
// the routing_profiles extension spec registered, so LoadAll accepts a persisted
// routing_profiles extension instead of rejecting it as unregistered.
func newSQLiteStoreWithRoutingProfiles(t *testing.T) (*store.SQLiteConfigStore, *testDBStore) {
	t.Helper()
	logger := testLogger(t)
	c := store.NewConfigStoreConsumer(logger)
	c.SetExtensionSpecs(routingprofiles.ConfigSpecs())
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

// legacyRoutingProfileConfig builds a test config whose routing_profiles
// extension RawConfig is the given map, reproducing the legacy "table" shape
// written by the old stopped-state save path.
func legacyRoutingProfileConfig(raw map[string]any) *config.Config {
	cfg := buildTestConfig()
	enabled := true
	cfg.Extensions = map[string]config.ExtensionSettings{
		"routing_profiles": {Enabled: &enabled, RawConfig: raw},
	}
	return cfg
}

func TestSQLiteStoreMigrateLegacyRoutingProfileShape(t *testing.T) {
	legacyTable := func(profiles map[string]any) map[string]any {
		return map[string]any{"table": profiles}
	}
	profileA := map[string]any{
		"display_name": "Profile A",
		"slots": map[string]any{
			"sol":   map[string]any{"provider": "p", "upstream_model": "m-flash", "mode": "thinking", "reasoning": "max"},
			"terra": map[string]any{"provider": "p", "upstream_model": "m-flash", "mode": "thinking", "reasoning": "high"},
			"luna":  map[string]any{"provider": "p", "upstream_model": "m-flash", "mode": "normal"},
		},
	}

	t.Run("single profile promotes and establishes active_profile", func(t *testing.T) {
		s, _ := newSQLiteStoreWithRoutingProfiles(t)
		if err := s.SeedFromConfig(legacyRoutingProfileConfig(legacyTable(map[string]any{"profile-a": profileA}))); err != nil {
			t.Fatalf("SeedFromConfig() error = %v", err)
		}
		loaded, err := s.LoadAll()
		if err != nil {
			t.Fatalf("LoadAll() error = %v", err)
		}
		raw := loaded.Extensions["routing_profiles"].RawConfig
		if _, ok := raw["table"]; ok {
			t.Fatalf("RawConfig still has legacy %q key: %#v", "table", raw)
		}
		profiles, ok := raw["profiles"].(map[string]any)
		if !ok {
			t.Fatalf("RawConfig[profiles] = %#v, want map", raw["profiles"])
		}
		if _, ok := profiles["profile-a"]; !ok {
			t.Fatalf("profiles = %#v, want profile-a", profiles)
		}
		if raw["active_profile"] != "profile-a" {
			t.Fatalf("active_profile = %#v, want profile-a", raw["active_profile"])
		}

		// Idempotent: a second LoadAll is a no-op and returns the same canonical
		// shape.
		again, err := s.LoadAll()
		if err != nil {
			t.Fatalf("second LoadAll() error = %v", err)
		}
		raw2 := again.Extensions["routing_profiles"].RawConfig
		if _, ok := raw2["table"]; ok {
			t.Fatalf("second LoadAll left %q key: %#v", "table", raw2)
		}
		if raw2["active_profile"] != "profile-a" {
			t.Fatalf("second LoadAll active_profile = %#v, want profile-a", raw2["active_profile"])
		}
	})

	t.Run("multiple profiles promote without guessing active_profile", func(t *testing.T) {
		s, _ := newSQLiteStoreWithRoutingProfiles(t)
		profiles := map[string]any{
			"profile-a": profileA,
			"profile-b": map[string]any{"display_name": "Profile B", "slots": map[string]any{}},
		}
		if err := s.SeedFromConfig(legacyRoutingProfileConfig(legacyTable(profiles))); err != nil {
			t.Fatalf("SeedFromConfig() error = %v", err)
		}
		loaded, err := s.LoadAll()
		if err != nil {
			t.Fatalf("LoadAll() error = %v", err)
		}
		raw := loaded.Extensions["routing_profiles"].RawConfig
		if _, ok := raw["table"]; ok {
			t.Fatalf("RawConfig still has legacy %q key: %#v", "table", raw)
		}
		got, ok := raw["profiles"].(map[string]any)
		if !ok || len(got) != 2 {
			t.Fatalf("RawConfig[profiles] = %#v, want 2 promoted profiles", raw["profiles"])
		}
		if raw["active_profile"] != nil {
			t.Fatalf("active_profile = %#v, want unset (do not guess among multiple profiles)", raw["active_profile"])
		}
	})

	t.Run("non-map table is left untouched", func(t *testing.T) {
		s, _ := newSQLiteStoreWithRoutingProfiles(t)
		cfg := legacyRoutingProfileConfig(map[string]any{"table": "not-a-map"})
		if err := s.SeedFromConfig(cfg); err != nil {
			t.Fatalf("SeedFromConfig() error = %v", err)
		}
		loaded, err := s.LoadAll()
		if err != nil {
			t.Fatalf("LoadAll() error = %v", err)
		}
		raw := loaded.Extensions["routing_profiles"].RawConfig
		if raw["table"] != "not-a-map" {
			t.Fatalf("RawConfig[table] = %#v, want unchanged", raw["table"])
		}
		if _, ok := raw["profiles"]; ok {
			t.Fatalf("RawConfig gained profiles from a non-map table: %#v", raw)
		}
	})

	t.Run("empty table is left untouched", func(t *testing.T) {
		s, _ := newSQLiteStoreWithRoutingProfiles(t)
		cfg := legacyRoutingProfileConfig(map[string]any{"table": map[string]any{}})
		if err := s.SeedFromConfig(cfg); err != nil {
			t.Fatalf("SeedFromConfig() error = %v", err)
		}
		loaded, err := s.LoadAll()
		if err != nil {
			t.Fatalf("LoadAll() error = %v", err)
		}
		raw := loaded.Extensions["routing_profiles"].RawConfig
		table, ok := raw["table"].(map[string]any)
		if !ok || len(table) != 0 {
			t.Fatalf("RawConfig[table] = %#v, want unchanged empty map", raw["table"])
		}
		if _, ok := raw["profiles"]; ok {
			t.Fatalf("RawConfig gained profiles from an empty table: %#v", raw)
		}
	})
}
