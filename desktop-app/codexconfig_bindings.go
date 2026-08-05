package main

import "moonbridge/internal/service/codexconfig"

// LoadCodexConfig loads the user's real codex config and the backup list. A
// missing config is not an error: OK=true with Exists=false.
func (a *App) LoadCodexConfig() DesktopCommandResult {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if a.closed.Load() {
		return hostClosed("LoadCodexConfig")
	}
	snap, err := a.codexConfig.Load(a.appCtx)
	if err != nil {
		return codexConfigError("LoadCodexConfig", "load", err)
	}
	backups, err := a.codexConfig.ListBackups(a.appCtx)
	if err != nil {
		return codexConfigError("LoadCodexConfig", "backups", err)
	}
	return okDesktop(&DesktopSnapshot{Config: desktopCodexConfig(snap), Backups: desktopBackups(backups)})
}

// UpdateCodexConfig backs up, edits, atomically writes and verifies the user's
// real codex config. A missing or unparseable config is an error (no
// auto-generation).
func (a *App) UpdateCodexConfig(input codexconfig.Input) DesktopCommandResult {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if a.closed.Load() {
		return hostClosed("UpdateCodexConfig")
	}
	snap, err := a.codexConfig.Update(a.appCtx, input)
	if err != nil {
		return codexConfigError("UpdateCodexConfig", "update", err)
	}
	return okDesktop(&DesktopSnapshot{Config: desktopCodexConfig(snap)})
}

type RestoreCodexConfigBackupRequest struct {
	ID string `json:"id"`
}

// RestoreCodexConfigBackup saves a pre-restore backup of the current config,
// applies the selected backup atomically, and rolls back on verification
// failure. Only backup IDs from the list are accepted (traversal-guarded).
func (a *App) RestoreCodexConfigBackup(req RestoreCodexConfigBackupRequest) DesktopCommandResult {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if a.closed.Load() {
		return hostClosed("RestoreCodexConfigBackup")
	}
	snap, err := a.codexConfig.Restore(a.appCtx, req.ID)
	if err != nil {
		return codexConfigError("RestoreCodexConfigBackup", "restore", err)
	}
	return okDesktop(&DesktopSnapshot{Config: desktopCodexConfig(snap)})
}

func (a *App) CodexConfigBackups() DesktopCommandResult {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if a.closed.Load() {
		return hostClosed("CodexConfigBackups")
	}
	backups, err := a.codexConfig.ListBackups(a.appCtx)
	if err != nil {
		return codexConfigError("CodexConfigBackups", "backups", err)
	}
	return okDesktop(&DesktopSnapshot{Backups: desktopBackups(backups)})
}
