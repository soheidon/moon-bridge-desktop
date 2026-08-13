package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"moonbridge/internal/config"
	"moonbridge/internal/db"
	dbsqlite "moonbridge/internal/extension/db/sqlite"
	"moonbridge/internal/secretstore"
	"moonbridge/internal/service/app"
	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/routingprofile"
	"moonbridge/internal/service/store"
)

// routingProfileController is the subset of the routingprofile service the App
// drives. It mirrors deepSeekController: a factory builds one controller per
// operation from the live session, so a gateway restart's fresh control token
// is always used.
type routingProfileController interface {
	Load(ctx context.Context) (*routingprofile.Snapshot, error)
	Save(ctx context.Context, input routingprofile.Input) (*routingprofile.Snapshot, error)
	// Deprecated: Use ActivateProfile instead. Retained for backward compatibility.
	ActivateSlot(ctx context.Context, profileID, slotID string) (*routingprofile.Snapshot, error)
	ActivateProfile(ctx context.Context, profileID string) (*routingprofile.Snapshot, error)
}

// LoadRoutingProfiles returns the current Codex routing profile table.
// When the gateway is running, it reads through the live session's control
// token. When stopped, it reads persisted config directly so the UI can still
// display saved profiles.
func (a *App) LoadRoutingProfiles() DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("LoadRoutingProfiles")
	}
	// Live path: read through the gateway's management API.
	if session, ok := a.ensureActiveSession(); ok {
		ctrl := a.newRoutingProfile("http://"+session.Address, session.ControlToken)
		snap, err := ctrl.Load(a.appCtx)
		if err != nil {
			return routingProfileError("LoadRoutingProfiles", "load", "routing_profile_load_failed", err)
		}
		return okDesktop(&DesktopSnapshot{RoutingProfiles: desktopRoutingProfiles(snap)})
	}
	// Stopped path: read persisted config to surface saved profiles. SQLite is
	// the source of truth when the gateway has persisted a store; YAML is used
	// only when no persisted store is configured or it is still unseeded.
	snap, err := a.loadRoutingProfilesFromStore()
	if err != nil {
		return routingProfileError("LoadRoutingProfiles", "load", "routing_profile_load_failed", err)
	}
	return okDesktop(&DesktopSnapshot{RoutingProfiles: desktopRoutingProfiles(snap)})
}

// openPersistedConfigStore opens a fresh SQLite config store for a stopped-state
// read. The gateway run owns the live registry and closes it on stop, so a fresh
// provider + registry reads the same WAL file. Registry.Init may run
// CREATE TABLE IF NOT EXISTS against the shared DB.
func openPersistedConfigStore(dbPath string, extensionSpecs []config.ExtensionConfigSpec) (store.ConfigStore, func(), error) {
	reg := db.NewRegistry(slog.Default())
	reg.RegisterProvider(dbsqlite.ProviderFor(dbPath))
	consumer := store.NewConfigStoreConsumer(slog.Default())
	consumer.SetExtensionSpecs(extensionSpecs)
	reg.RegisterConsumer(consumer)
	if err := reg.Init(context.Background(), dbsqlite.PluginName); err != nil {
		return nil, nil, err
	}
	cs := consumer.Store()
	if sqlStore, ok := cs.(*store.SQLiteConfigStore); ok {
		// Inject the codec so LoadAll migrates legacy plaintext keys and writes
		// never persist plaintext.
		sqlStore.SetCodec(persistedStoreCodec())
	}
	return cs, func() { _ = reg.Shutdown() }, nil
}

// persistedStoreCodec is the secret codec injected into stopped-state SQLite
// reads. It is a seam so tests can force a migration outcome (e.g. a codec that
// cannot encrypt produces a migration_failed issue on LoadAll).
var persistedStoreCodec = func() secretstore.SecretCodec { return secretstore.New() }

// resolveSQLiteDBPath returns the absolute DB path the gateway uses for
// persistence, or ok=false when persistence is not configured (no db_sqlite
// extension path — the gateway then runs with no persisted store). The path is
// resolved by dbsqlite.ResolvePath so relative paths resolve exactly as they do
// at gateway startup; desktop-app does not interpret path semantics itself.
func (a *App) resolveSQLiteDBPath() (dbPath string, ok bool, err error) {
	configPath, err := a.resolveConfigPath("")
	if err != nil {
		return "", false, err
	}
	loadOpts := config.LoadOptions{ExtensionSpecs: app.BuiltinExtensions().ConfigSpecs()}
	cfg, err := config.LoadFromFileWithOptions(configPath, loadOpts)
	if err != nil {
		return "", false, err
	}
	ext, present := cfg.Extensions["db_sqlite"]
	rawPath, _ := ext.RawConfig["path"].(string)
	if !present || rawPath == "" {
		return "", false, nil // gateway has no persisted store → YAML fallback
	}
	absPath, err := dbsqlite.ResolvePath(rawPath)
	if err != nil {
		return "", false, err
	}
	return absPath, true, nil
}

// loadRoutingProfilesFromStore reads the persisted config graph without a live
// gateway session, so the UI can display saved profiles while stopped. The
// gateway persists to SQLite; YAML is a one-time seed. Contract: LoadAll
// success → SQLite is the source of truth; ErrConfigNotSeeded (or no persisted
// store) → fall back to YAML.
func (a *App) loadRoutingProfilesFromStore() (*routingprofile.Snapshot, error) {
	dbPath, hasStore, err := a.resolveSQLiteDBPath()
	if err != nil {
		return nil, err
	}
	if !hasStore {
		return a.loadRoutingProfilesFromYAML()
	}
	// Never-started first run: don't create the DB file during a read.
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return a.loadRoutingProfilesFromYAML()
	}
	cs, closeStore, err := openPersistedConfigStore(dbPath, app.BuiltinExtensions().ConfigSpecs())
	if err != nil {
		return nil, err
	}
	defer closeStore()
	dbCfg, err := cs.LoadAll()
	if err != nil {
		if errors.Is(err, store.ErrConfigNotSeeded) {
			return a.loadRoutingProfilesFromYAML() // unseeded/empty store → YAML
		}
		return nil, err
	}
	// No extra ProviderDefs/Routes shape checks — a persisted routing_profiles
	// extension with no routes is still valid and must not fall back to YAML.
	graph := configgraph.BuildGraph(*dbCfg, "")
	snap := routingprofile.SnapshotFromGraph(graph, false)
	return &snap, nil
}

// loadRoutingProfilesFromYAML reads the config file directly and builds a
// routing profile snapshot. It is the fallback for a stopped read when no
// persisted SQLite store is configured or it is still unseeded.
func (a *App) loadRoutingProfilesFromYAML() (*routingprofile.Snapshot, error) {
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
	snap := routingprofile.SnapshotFromGraph(graph, false)
	return &snap, nil
}

// ActivateRoutingSlotRequest selects the slot of a profile to activate.
//
// Deprecated: Use ActivateProfileRequest + ActivateProfile instead. Slot-level
// activation is retained only for backward compatibility with older frontends.
// New UI must call ActivateProfile; this binding will be removed after frontend
// migration completes.
type ActivateRoutingSlotRequest struct {
	ProfileID string `json:"profileId"`
	SlotID    string `json:"slotId"`
}

// ActivateRoutingSlot activates a slot of a profile, then refreshes the session
// config so a subsequent codex launch uses the new routing. A failed session
// refresh is partial success: the profile is active, but codex launches are
// refused until the gateway restarts.
//
// Deprecated: Use ActivateProfile instead. Slot-level activation patches routes
// and reasoning on every call, which is no longer required. ActivateProfile
// changes only active_profile and lets the resolver handle slot lookup at
// request time. This binding is retained for backward compatibility and will be
// removed after frontend migration completes.
func (a *App) ActivateRoutingSlot(req ActivateRoutingSlotRequest) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("ActivateRoutingSlot")
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		return routingProfileGatewayNotRunning("ActivateRoutingSlot")
	}
	ctrl := a.newRoutingProfile("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.ActivateSlot(a.appCtx, req.ProfileID, req.SlotID)
	if err != nil {
		return routingProfileError("ActivateRoutingSlot", "activate", "routing_profile_activate_failed", err)
	}
	cfg, derr := a.deriveConfigCodex(session)
	if derr != nil {
		session.ConfigValid = false
		return DesktopCommandResult{
			OK: false,
			Error: &CommandError{
				Operation:       "ActivateRoutingSlot",
				Stage:           "refresh_session_config",
				Code:            "routing_profile_activated_session_refresh_failed",
				Message:         "profile activated but session config refresh failed",
				Retryable:       true,
				MutationStarted: true,
				Details: map[string]any{
					"saved":                  true,
					"sessionConfigRefreshed": false,
					"requiresGatewayRestart": true,
				},
			},
		}
	}
	session.Config = cfg
	session.ConfigValid = true
	if a.routingProfileRefresh != nil {
		a.routingProfileRefresh(cfg)
	}
	return okDesktop(&DesktopSnapshot{RoutingProfiles: desktopRoutingProfiles(snap)})
}

// ActivateProfileRequest selects a profile to activate. Only active_profile
// is changed; slot definitions are not modified.
type ActivateProfileRequest struct {
	ProfileID string `json:"profileId"`
}

// ActivateProfile changes only active_profile in the routing_profiles extension.
// Slot definitions, route resources, and model reasoning are left untouched.
func (a *App) ActivateProfile(req ActivateProfileRequest) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("ActivateProfile")
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		return routingProfileGatewayNotRunning("ActivateProfile")
	}
	ctrl := a.newRoutingProfile("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.ActivateProfile(a.appCtx, req.ProfileID)
	if err != nil {
		return routingProfileError("ActivateProfile", "activate", "routing_profile_activate_failed", err)
	}
	cfg, derr := a.deriveConfigCodex(session)
	if derr != nil {
		session.ConfigValid = false
		return DesktopCommandResult{
			OK: false,
			Error: &CommandError{
				Operation:       "ActivateProfile",
				Stage:           "refresh_session_config",
				Code:            "routing_profile_activated_session_refresh_failed",
				Message:         "profile activated but session config refresh failed",
				Retryable:       true,
				MutationStarted: true,
				Details: map[string]any{
					"saved":                  true,
					"sessionConfigRefreshed": false,
					"requiresGatewayRestart": true,
				},
			},
		}
	}
	session.Config = cfg
	session.ConfigValid = true
	if a.routingProfileRefresh != nil {
		a.routingProfileRefresh(cfg)
	}
	return okDesktop(&DesktopSnapshot{RoutingProfiles: desktopRoutingProfiles(snap)})
}

// SaveRoutingProfile persists one profile edit, then refreshes the session
// config. Validation runs first so a malformed edit fails even while the
// gateway is stopped.
func (a *App) SaveRoutingProfile(input routingprofile.Input) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("SaveRoutingProfile")
	}
	if err := input.Validate(); err != nil {
		return routingProfileError("SaveRoutingProfile", "validation", "routing_profile_validate_failed", err)
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		snap, err := a.saveRoutingProfileToStore(input)
		if err != nil {
			return routingProfileError("SaveRoutingProfile", "save", "routing_profile_save_failed", err)
		}
		return okDesktop(&DesktopSnapshot{RoutingProfiles: desktopRoutingProfiles(snap)})
	}
	ctrl := a.newRoutingProfile("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.Save(a.appCtx, input)
	if err != nil {
		return routingProfileError("SaveRoutingProfile", "save", "routing_profile_save_failed", err)
	}
	cfg, derr := a.deriveConfigCodex(session)
	if derr != nil {
		session.ConfigValid = false
		return DesktopCommandResult{
			OK: false,
			Error: &CommandError{
				Operation:       "SaveRoutingProfile",
				Stage:           "refresh_session_config",
				Code:            "routing_profile_saved_session_refresh_failed",
				Message:         "profile saved but session config refresh failed",
				Retryable:       true,
				MutationStarted: true,
				Details: map[string]any{
					"saved":                  true,
					"sessionConfigRefreshed": false,
					"requiresGatewayRestart": true,
				},
			},
		}
	}
	session.Config = cfg
	session.ConfigValid = true
	if a.routingProfileRefresh != nil {
		a.routingProfileRefresh(cfg)
	}
	return okDesktop(&DesktopSnapshot{RoutingProfiles: desktopRoutingProfiles(snap)})
}

// saveRoutingProfileToStore persists a routing profile edit to the SQLite store
// without a live gateway session. When no persisted store exists or it is
// unseeded, it seeds from the YAML config first then applies the edit.
func (a *App) saveRoutingProfileToStore(input routingprofile.Input) (*routingprofile.Snapshot, error) {
	dbPath, hasStore, err := a.resolveSQLiteDBPath()
	if err != nil {
		return nil, err
	}
	if !hasStore {
		return nil, errors.New("no persisted store configured")
	}
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
		if err := a.seedStoreFromYAML(cs); err != nil {
			return nil, err
		}
		reload, rerr := cs.LoadAll()
		if rerr != nil {
			return nil, rerr
		}
		dbCfg = reload
	}
	// routing_profiles extensionの更新
	ext, ok := dbCfg.Extensions["routing_profiles"]
	if !ok {
		ext = config.ExtensionSettings{}
	}
	if ext.RawConfig == nil {
		ext.RawConfig = map[string]any{}
	}
	tableRaw, _ := ext.RawConfig["table"].(map[string]any)
	if tableRaw == nil {
		tableRaw = map[string]any{}
	}
	profileRaw, _ := tableRaw[input.Profile.ID].(map[string]any)
	if profileRaw == nil {
		profileRaw = map[string]any{}
	}
	profileRaw["display_name"] = input.Profile.DisplayName
	slotsRaw, _ := profileRaw["slots"].(map[string]any)
	if slotsRaw == nil {
		slotsRaw = map[string]any{}
	}
	for slotID, slotInput := range input.Profile.Slots {
		slotRaw, _ := slotsRaw[slotID].(map[string]any)
		if slotRaw == nil {
			slotRaw = map[string]any{}
		}
		slotRaw["provider"] = slotInput.Provider
		slotRaw["upstream_model"] = slotInput.UpstreamModel
		slotRaw["mode"] = slotInput.Mode
		if slotInput.Reasoning != nil {
			slotRaw["reasoning"] = *slotInput.Reasoning
		}
		slotsRaw[slotID] = slotRaw
	}
	profileRaw["slots"] = slotsRaw
	tableRaw[input.Profile.ID] = profileRaw
	ext.RawConfig["table"] = tableRaw
	dbCfg.Extensions["routing_profiles"] = ext
	if _, err := cs.SaveConfig(context.Background(), dbCfg); err != nil {
		return nil, err
	}
	reloaded, err := cs.LoadAll()
	if err != nil {
		return nil, err
	}
	graph := configgraph.BuildGraph(*reloaded, "")
	snap := routingprofile.SnapshotFromGraph(graph, false)
	return &snap, nil
}
