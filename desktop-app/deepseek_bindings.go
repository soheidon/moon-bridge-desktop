package main

import (
	"context"
	"errors"
	"os"

	"moonbridge/internal/config"
	"moonbridge/internal/secretstore"
	"moonbridge/internal/service/app"
	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/store"
)

// LoadDeepSeekSettings returns the current provider configuration through the
// live gateway session's control token. When the gateway is stopped, it reads
// persisted config directly so the UI can still show the saved key state.
func (a *App) LoadDeepSeekSettings() DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("LoadDeepSeekSettings")
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		snap, err := a.loadDeepSeekFromStore()
		if err != nil {
			return deepSeekError("LoadDeepSeekSettings", "load", "deepseek_load_failed", err)
		}
		return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
	}
	ctrl := a.newDeepSeek("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.Load(a.appCtx)
	if err != nil {
		return deepSeekError("LoadDeepSeekSettings", "load", "deepseek_load_failed", err)
	}
	return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
}

// loadDeepSeekFromStore reads the persisted config graph without a live gateway
// session so the UI can show the saved DeepSeek state while stopped. The
// gateway persists to SQLite; YAML is a one-time seed. A stored key is not
// decrypted here (no probe): it reports unverified until the gateway runs.
func (a *App) loadDeepSeekFromStore() (*deepseek.Snapshot, error) {
	dbPath, hasStore, err := a.resolveSQLiteDBPath()
	if err != nil {
		return nil, err
	}
	if !hasStore {
		return a.loadDeepSeekFromYAML()
	}
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return a.loadDeepSeekFromYAML()
	}
	cs, closeStore, err := openPersistedConfigStore(dbPath, app.BuiltinExtensions().ConfigSpecs())
	if err != nil {
		return nil, err
	}
	defer closeStore()
	dbCfg, err := cs.LoadAll()
	if err != nil {
		if errors.Is(err, store.ErrConfigNotSeeded) {
			return a.loadDeepSeekFromYAML()
		}
		return nil, err
	}
	graph := configgraph.BuildGraph(*dbCfg, "")
	snap := deepseek.SnapshotFromGraph(graph, false)
	// A legacy plaintext key that could not be migrated (unsupported platform or
	// encryption failure) stays in the graph to keep it valid; surface the
	// migration issue so the UI shows stored/unavailable instead of unverified.
	if sqlStore, ok := cs.(*store.SQLiteConfigStore); ok {
		deepseek.ApplyMigrationIssues(&snap, sqlStore.LastMigrationIssues())
	}
	return &snap, nil
}

// loadDeepSeekFromYAML reads the config file directly and builds a DeepSeek
// snapshot. It is the fallback for a stopped read when no persisted SQLite
// store is configured or it is still unseeded.
func (a *App) loadDeepSeekFromYAML() (*deepseek.Snapshot, error) {
	path, err := a.resolveConfigPath("")
	if err != nil {
		return nil, err
	}
	loadOpts := config.LoadOptions{ExtensionSpecs: app.BuiltinExtensions().ConfigSpecs()}
	cfg, err := config.LoadFromFileWithOptions(path, loadOpts)
	if err != nil {
		return nil, err
	}
	graph := configgraph.BuildGraph(cfg, "")
	snap := deepseek.SnapshotFromGraph(graph, false)
	return &snap, nil
}

// ValidateDeepSeekSettings validates the input in isolation: it needs no
// gateway session, so it works while the gateway is stopped. The result is a
// masked preview of the normalized input, never a plaintext key.
func (a *App) ValidateDeepSeekSettings(input deepseek.Input) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("ValidateDeepSeekSettings")
	}
	if err := input.Validate(); err != nil {
		return deepSeekError("ValidateDeepSeekSettings", "validation", "deepseek_validate_failed", err)
	}
	snap := deepSeekInputSnapshot(input)
	return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(&snap)})
}

// storedKeyEncryptionSupported reports whether the platform can encrypt stored
// API keys (Windows DPAPI). It is a seam so the non-Windows guard is testable;
// on unsupported platforms only the environment-variable path may be used.
var storedKeyEncryptionSupported = func() bool { return secretstore.New().Supported() }

// SaveDeepSeekSettings reconciles the provider graph, then refreshes the
// session config so a subsequent codex launch uses the new settings.
func (a *App) SaveDeepSeekSettings(input deepseek.Input) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("SaveDeepSeekSettings")
	}
	if err := input.Validate(); err != nil {
		return deepSeekError("SaveDeepSeekSettings", "validation", "deepseek_validate_failed", err)
	}
	// On platforms without secret encryption a stored key would persist as
	// plaintext; refuse it and route the user to the env-var path. Empty input
	// keeps the existing key, which is unaffected.
	if input.APIKey != "" && !storedKeyEncryptionSupported() {
		return errDesktop("SaveDeepSeekSettings", "validation", "deepseek_unsupported_platform",
			"stored API keys require Windows; use an environment variable instead", false)
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		snap, err := a.saveDeepSeekToStore(input)
		if err != nil {
			return deepSeekError("SaveDeepSeekSettings", "save", "deepseek_save_failed", err)
		}
		return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
	}
	ctrl := a.newDeepSeek("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.Save(a.appCtx, input)
	if err != nil {
		return deepSeekError("SaveDeepSeekSettings", "save", "deepseek_save_failed", err)
	}
	if cfg, derr := a.deriveConfigCodex(session); derr != nil {
		// Saved, but the session could not pick up the new config from the live
		// gateway effective store: refuse codex launches until the gateway
		// restarts. Non-secret partial-success info rides in Details; Value stays
		// nil per the envelope contract.
		session.ConfigValid = false
		return DesktopCommandResult{
			OK: false,
			Error: &CommandError{
				Operation:       "SaveDeepSeekSettings",
				Stage:           "refresh_session_config",
				Code:            "deepseek_saved_session_refresh_failed",
				Message:         "settings saved but session config refresh failed",
				Retryable:       true,
				MutationStarted: true,
				Details: map[string]any{
					"saved":                  true,
					"sessionConfigRefreshed": false,
					"requiresGatewayRestart": true,
				},
			},
		}
	} else {
		session.Config = cfg
		session.ConfigValid = true
	}
	return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
}

// ClearDeepSeekKey removes the stored DeepSeek API key. While the gateway runs
// it reconciles through the live session (which also refreshes the session
// config); while stopped it clears the persisted SQLite provider directly. The
// api_key_env setting is left untouched so an environment variable can take
// over. The key value never appears in the response.
func (a *App) ClearDeepSeekKey(operationId string) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("ClearDeepSeekKey")
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		snap, err := a.clearDeepSeekFromStore()
		if err != nil {
			return deepSeekError("ClearDeepSeekKey", "clear", "deepseek_clear_failed", err)
		}
		return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
	}
	ctrl := a.newDeepSeek("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.Clear(a.appCtx)
	if err != nil {
		return deepSeekError("ClearDeepSeekKey", "clear", "deepseek_clear_failed", err)
	}
	cfg, derr := a.deriveConfigCodex(session)
	if derr != nil {
		session.ConfigValid = false
		return DesktopCommandResult{
			OK: false,
			Error: &CommandError{
				Operation:       "ClearDeepSeekKey",
				Stage:           "refresh_session_config",
				Code:            "deepseek_cleared_session_refresh_failed",
				Message:         "key cleared but session config refresh failed",
				Retryable:       true,
				MutationStarted: true,
				Details: map[string]any{
					"cleared":                true,
					"sessionConfigRefreshed": false,
					"requiresGatewayRestart": true,
				},
			},
		}
	}
	session.Config = cfg
	session.ConfigValid = true
	return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
}

// saveDeepSeekToStore persists the DeepSeek provider settings (API key and/or
// api_key_env) to the SQLite store without a live gateway session. When no
// persisted store exists or it is unseeded, it seeds from the YAML config first
// then applies the edit. YAML fallback that returns success without saving is
// not used here—this function always persists when a store path is configured.
func (a *App) saveDeepSeekToStore(input deepseek.Input) (*deepseek.Snapshot, error) {
	normalized := input.Normalized()
	dbPath, hasStore, err := a.resolveSQLiteDBPath()
	if err != nil {
		return nil, err
	}
	if !hasStore {
		return nil, errors.New("no persisted store configured")
	}
	// DB未作成の初回起動前：YAMLからseedしてから保存する
	needSeed := false
	if _, serr := os.Stat(dbPath); errors.Is(serr, os.ErrNotExist) {
		needSeed = true
	}
	cs, closeStore, err := openPersistedConfigStore(dbPath, app.BuiltinExtensions().ConfigSpecs())
	if err != nil {
		return nil, err
	}
	defer closeStore()
	if needSeed {
		if err := a.seedStoreFromYAML(cs); err != nil {
			return nil, err
		}
	}
	dbCfg, err := cs.LoadAll()
	if err != nil {
		if !errors.Is(err, store.ErrConfigNotSeeded) {
			return nil, err
		}
		// 未seed：YAMLからseedしてから再読み込み
		if err := a.seedStoreFromYAML(cs); err != nil {
			return nil, err
		}
		reload, rerr := cs.LoadAll()
		if rerr != nil {
			return nil, rerr
		}
		dbCfg = reload
	}
	provider, ok := dbCfg.ProviderDefs[deepseek.ProviderID]
	if !ok {
		provider = config.ProviderDef{}
	}
	// APIキーは空でない場合のみ更新（空=既存キー維持。キー削除はClearDeepSeekKeyが担当）
	if normalized.APIKey != "" {
		provider.APIKey = normalized.APIKey
	}
	// APIKeyEnvはnilでない場合のみ更新（nil=変更なし、空文字=環境変数名を削除、非空=設定）
	if normalized.APIKeyEnv != nil {
		provider.APIKeyEnv = *normalized.APIKeyEnv
	}
	dbCfg.ProviderDefs[deepseek.ProviderID] = provider
	if _, err := cs.SaveConfig(context.Background(), dbCfg); err != nil {
		return nil, err
	}
	reloaded, err := cs.LoadAll()
	if err != nil {
		return nil, err
	}
	graph := configgraph.BuildGraph(*reloaded, "")
	snap := deepseek.SnapshotFromGraph(graph, false)
	if sqlStore, ok := cs.(*store.SQLiteConfigStore); ok {
		deepseek.ApplyMigrationIssues(&snap, sqlStore.LastMigrationIssues())
	}
	return &snap, nil
}

// seedStoreFromYAML loads the YAML config file and seeds the SQLite store with
// it. This is used when saveDeepSeekToStore encounters an unseeded store.
func (a *App) seedStoreFromYAML(cs store.ConfigStore) error {
	path, err := a.resolveConfigPath("")
	if err != nil {
		return err
	}
	loadOpts := config.LoadOptions{ExtensionSpecs: app.BuiltinExtensions().ConfigSpecs()}
	cfg, err := config.LoadFromFileWithOptions(path, loadOpts)
	if err != nil {
		return err
	}
	sqlStore, ok := cs.(*store.SQLiteConfigStore)
	if !ok {
		return errors.New("store does not support seeding")
	}
	return sqlStore.SeedFromConfig(&cfg)
}

// clearDeepSeekFromStore removes the persisted DeepSeek api_key from the SQLite
// store without a live gateway session. When no persisted store exists (or it is
// unseeded) the current stopped snapshot is returned unchanged: a YAML-seeded key
// in non-persisted mode is a one-time seed that re-applies on the next gateway
// start. The api_key_env setting is preserved so an env var can take over.
func (a *App) clearDeepSeekFromStore() (*deepseek.Snapshot, error) {
	dbPath, hasStore, err := a.resolveSQLiteDBPath()
	if err != nil {
		return nil, err
	}
	if !hasStore {
		return a.loadDeepSeekFromYAML()
	}
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return a.loadDeepSeekFromYAML()
	}
	cs, closeStore, err := openPersistedConfigStore(dbPath, app.BuiltinExtensions().ConfigSpecs())
	if err != nil {
		return nil, err
	}
	defer closeStore()
	dbCfg, err := cs.LoadAll()
	if err != nil {
		if errors.Is(err, store.ErrConfigNotSeeded) {
			return a.loadDeepSeekFromYAML()
		}
		return nil, err
	}
	provider, ok := dbCfg.ProviderDefs[deepseek.ProviderID]
	if !ok || provider.APIKey == "" {
		graph := configgraph.BuildGraph(*dbCfg, "")
		snap := deepseek.SnapshotFromGraph(graph, false)
		return &snap, nil
	}
	provider.APIKey = ""
	dbCfg.ProviderDefs[deepseek.ProviderID] = provider
	if _, err := cs.SaveConfig(context.Background(), dbCfg); err != nil {
		return nil, err
	}
	reloaded, err := cs.LoadAll()
	if err != nil {
		return nil, err
	}
	graph := configgraph.BuildGraph(*reloaded, "")
	snap := deepseek.SnapshotFromGraph(graph, false)
	return &snap, nil
}

// TestDeepSeekConnection probes the DeepSeek provider's upstream connection
// through the live gateway session. The gateway resolves the credential via the
// shared resolver, so no key crosses from the frontend. The gateway stays
// running; the structured result (ok/code/message) is what the UI shows.
func (a *App) TestDeepSeekConnection(operationId string) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("TestDeepSeekConnection")
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		return deepSeekGatewayNotRunning("TestDeepSeekConnection")
	}
	ctrl := a.newDeepSeek("http://"+session.Address, session.ControlToken)
	result, err := ctrl.TestProvider(a.appCtx)
	if err != nil {
		return deepSeekError("TestDeepSeekConnection", "test", "deepseek_test_failed", err)
	}
	snap := a.snapshot()
	warning := ""
	return okDesktop(&DesktopSnapshot{
		ConnectionTestOperationID: operationId,
		ConnectionTest: &DeepSeekConnectionTest{
			OK:      result.Success,
			Code:    result.Code,
			Message: result.Message,
			Model:   result.Model,
		},
		ConnectionTestWarning:        &warning,
		ConnectionGatewaySnapshot:    &snap,
		ConnectionGatewayLeftRunning: true,
	})
}

// deepSeekInputSnapshot builds the masked preview snapshot for a validated
// input without consulting the gateway. A provided api key collapses to the
// same irreversible "configured" mask form used elsewhere; plaintext never
// appears in the result. Graph-derived flags are left false — nothing was
// read from or written to the gateway.
func deepSeekInputSnapshot(input deepseek.Input) deepseek.Snapshot {
	in := input.Normalized()
	selected := in.SelectedModel()
	reasoning := in.ProReasoning
	allowed := deepseek.AllowedReasoningEfforts(deepseek.ModelPro)
	if selected == deepseek.ModelFlash {
		reasoning = in.FlashReasoning
		allowed = deepseek.AllowedReasoningEfforts(deepseek.ModelFlash)
	}
	keySet := in.APIKey != ""
	masked := ""
	if keySet {
		masked = "configured"
	}
	// Shared formal derivation: a typed key becomes stored, otherwise the env
	// var decides. GatewayRunning is false (nothing read from the gateway), so a
	// typed key reports unverified rather than a naive available.
	source, state := deepseek.DeriveCredential(in.APIKey, in.APIKeyEnvValue(), false, os.LookupEnv)
	return deepseek.Snapshot{
		GatewayRunning:                false,
		ProviderExists:                false,
		APIKeySet:                     keySet,
		APIKeyMasked:                  masked,
		APIKeyEnv:                     in.APIKeyEnvValue(),
		CredentialSource:              string(source),
		CredentialState:               string(state),
		Configured:                    false,
		Active:                        false,
		SelectedModel:                 selected,
		DefaultModel:                  in.DefaultModel,
		ReasoningEffort:               reasoning,
		ReasoningExplicitlyConfigured: true,
		AllowedReasoningEfforts:       allowed,
		RouteAlias:                    deepseek.RouteID,
		Pro: deepseek.ModelConfig{
			ModelID:   deepseek.ModelPro,
			Reasoning: in.ProReasoning,
			Supported: deepseek.AllowedReasoningEfforts(deepseek.ModelPro),
		},
		Flash: deepseek.ModelConfig{
			ModelID:   deepseek.ModelFlash,
			Reasoning: in.FlashReasoning,
			Supported: deepseek.AllowedReasoningEfforts(deepseek.ModelFlash),
		},
	}
}
