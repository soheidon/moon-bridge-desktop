package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/db"
	dbsqlite "moonbridge/internal/extension/db/sqlite"
	"moonbridge/internal/secretstore"
	bridgeapp "moonbridge/internal/service/app"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/store"
)

// failingMigrationCodec is Supported (so the legacy-migration path treats stored
// plaintext as encodable) but its Encrypt always fails, forcing a
// migration_failed issue on LoadAll — the "another Windows user / foreign DPAPI
// blob" analog for a stopped-state read.
type failingMigrationCodec struct{}

func (failingMigrationCodec) Supported() bool                     { return true }
func (failingMigrationCodec) Encrypt(string) (string, error)      { return "", errors.New("encrypt failed") }
func (failingMigrationCodec) Decrypt(string) (string, error)      { return "", errors.New("decrypt failed") }

// unsupportedCodec reports an unsupported platform so stored keys surface
// unsupported_platform (the non-Windows stopped-read analog).
type unsupportedCodec struct{}

func (unsupportedCodec) Supported() bool                { return false }
func (unsupportedCodec) Encrypt(string) (string, error) { return "", secretstore.ErrUnsupported }
func (unsupportedCodec) Decrypt(string) (string, error) { return "", secretstore.ErrUnsupported }

// openStoreNoCodec opens the persisted config store without injecting a secret
// codec, so SaveConfig writes provider api_keys verbatim (legacy plaintext).
func openStoreNoCodec(t *testing.T, dbPath string) (store.ConfigStore, func()) {
	t.Helper()
	reg := db.NewRegistry(slog.Default())
	reg.RegisterProvider(dbsqlite.ProviderFor(dbPath))
	consumer := store.NewConfigStoreConsumer(slog.Default())
	consumer.SetExtensionSpecs(bridgeapp.BuiltinExtensions().ConfigSpecs())
	reg.RegisterConsumer(consumer)
	if err := reg.Init(context.Background(), dbsqlite.PluginName); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	return consumer.Store(), func() { _ = reg.Shutdown() }
}

// seedPlaintextDeepSeekKey writes a deepseek provider carrying a legacy
// plaintext API key into the persisted store (no codec → not encrypted).
func seedPlaintextDeepSeekKey(t *testing.T, configPath, dbPath, key string) {
	t.Helper()
	base, err := config.LoadFromFileWithOptions(configPath, config.LoadOptions{ExtensionSpecs: bridgeapp.BuiltinExtensions().ConfigSpecs()})
	if err != nil {
		t.Fatalf("LoadFromFileWithOptions() error = %v", err)
	}
	base.ProviderDefs[deepseek.ProviderID] = config.ProviderDef{
		BaseURL: deepseek.BaseURL, APIKey: key, Protocol: deepseek.Protocol,
	}
	cs, closeStore := openStoreNoCodec(t, dbPath)
	defer closeStore()
	if _, err := cs.SaveConfig(context.Background(), &base); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
}

// TestSaveDeepSeekSettingsUnsupportedPlatformGuard pins the non-Windows save
// guard: storing an API key on a platform without secret encryption is refused
// with deepseek_unsupported_platform, and the environment path is unaffected.
func TestSaveDeepSeekSettingsUnsupportedPlatformGuard(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	cfg := writeCaptureAnthropicConfig(t, t.TempDir())
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  cfg,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	orig := storedKeyEncryptionSupported
	storedKeyEncryptionSupported = func() bool { return false }
	defer func() { storedKeyEncryptionSupported = orig }()

	input := validDeepSeekInput()
	input.APIKey = "sk-typed-key-12345"
	res := app.SaveDeepSeekSettings(input)
	if res.OK || res.Error == nil {
		t.Fatalf("SaveDeepSeekSettings(key) = %#v, want deepseek_unsupported_platform", res)
	}
	if res.Error.Code != "deepseek_unsupported_platform" {
		t.Fatalf("SaveDeepSeekSettings(key) code = %q, want deepseek_unsupported_platform", res.Error.Code)
	}

	// Empty key (environment path) passes the guard and falls through to the
	// stopped persistent-save path. This fixture has no persisted store, so the
	// save failure proves the unsupported-platform guard only blocks stored keys.
	envInput := validDeepSeekInput()
	res = app.SaveDeepSeekSettings(envInput)
	if res.Error == nil || res.Error.Code != "deepseek_save_failed" {
		t.Fatalf("SaveDeepSeekSettings(env) = %#v, want deepseek_save_failed (guard bypassed)", res)
	}
}

// TestLoadDeepSeekSettingsStoppedSurfacesMigrationFailure proves the stopped
// read surfaces a legacy-key migration failure: the graph still holds the stored
// key (kept valid), but the snapshot reports stored/unavailable/migration_failed
// so the UI can offer delete and re-entry instead of pretending the key works.
func TestLoadDeepSeekSettingsStoppedSurfacesMigrationFailure(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	configPath, dbPath := integrationConfig(t, "server-tok")
	seedPlaintextDeepSeekKey(t, configPath, dbPath, "sk-legacy-plaintext-12345")

	orig := persistedStoreCodec
	persistedStoreCodec = func() secretstore.SecretCodec { return failingMigrationCodec{} }
	defer func() { persistedStoreCodec = orig }()

	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.LoadDeepSeekSettings()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadDeepSeekSettings() = %#v, want ok stopped-state snapshot", res)
	}
	snap := res.Value.DeepSeek
	if snap == nil {
		t.Fatal("LoadDeepSeekSettings() value.DeepSeek = nil")
	}
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateUnavailable || snap.CredentialErrorCode != provider.ErrCodeMigrationFailed {
		t.Fatalf("credential = %q/%q/%q, want stored/unavailable/migration_failed",
			snap.CredentialSource, snap.CredentialState, snap.CredentialErrorCode)
	}
}

// TestLoadDeepSeekSettingsStoppedSurfacesUnsupportedPlatform is the non-Windows
// stopped-read analog: a stored plaintext key that cannot be migrated on an
// unsupported platform reports stored/unavailable/unsupported_platform.
func TestLoadDeepSeekSettingsStoppedSurfacesUnsupportedPlatform(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	configPath, dbPath := integrationConfig(t, "server-tok")
	seedPlaintextDeepSeekKey(t, configPath, dbPath, "sk-legacy-plaintext-12345")

	orig := persistedStoreCodec
	persistedStoreCodec = func() secretstore.SecretCodec { return unsupportedCodec{} }
	defer func() { persistedStoreCodec = orig }()

	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.LoadDeepSeekSettings()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadDeepSeekSettings() = %#v, want ok stopped-state snapshot", res)
	}
	snap := res.Value.DeepSeek
	if snap == nil {
		t.Fatal("LoadDeepSeekSettings() value.DeepSeek = nil")
	}
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateUnavailable || snap.CredentialErrorCode != provider.ErrCodeUnsupportedPlatform {
		t.Fatalf("credential = %q/%q/%q, want stored/unavailable/unsupported_platform",
			snap.CredentialSource, snap.CredentialState, snap.CredentialErrorCode)
	}
}

// TestLoadDeepSeekSettingsStoppedHealthyMigration reports unverified (no probe):
// with a working codec the legacy key migrates to ciphertext on LoadAll and the
// stopped snapshot keeps stored/unverified until the gateway runs.
func TestLoadDeepSeekSettingsStoppedHealthyMigration(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	configPath, dbPath := integrationConfig(t, "server-tok")
	seedPlaintextDeepSeekKey(t, configPath, dbPath, "sk-legacy-plaintext-12345")

	// Default seam → real codec; on unsupported platforms the store reports
	// unsupported_platform instead, so skip the assertion where DPAPI is absent.
	svc := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{
		Service:     svc,
		NewIdentity: fixedIdentity("inst-1", "token-1"),
		ConfigPath:  configPath,
		EmitEvents:  noopEmit,
	})
	defer app.shutdown(context.Background())

	res := app.LoadDeepSeekSettings()
	if !res.OK || res.Error != nil {
		t.Fatalf("LoadDeepSeekSettings() = %#v, want ok stopped-state snapshot", res)
	}
	snap := res.Value.DeepSeek
	if snap == nil {
		t.Fatal("LoadDeepSeekSettings() value.DeepSeek = nil")
	}
	// Stored/unverified on a supported codec (Windows); stored/unavailable/
	// unsupported_platform on other platforms. Either way the key is not probed.
	if snap.CredentialSource != provider.SourceStored {
		t.Fatalf("credential source = %q, want stored", snap.CredentialSource)
	}
	switch snap.CredentialState {
	case provider.StateUnverified:
	case provider.StateUnavailable:
		if snap.CredentialErrorCode != provider.ErrCodeUnsupportedPlatform {
			t.Fatalf("unavailable error = %q, want unsupported_platform", snap.CredentialErrorCode)
		}
	default:
		t.Fatalf("credential state = %q, want unverified (or unsupported on non-Windows)", snap.CredentialState)
	}
}
