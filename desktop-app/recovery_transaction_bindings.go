package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/trafficanalysis"
)

// RestoreRecoveryInput is the explicit conflict confirmation for a local
// top-level openai_base_url restore. It never permits a backup-wide restore.
type RestoreRecoveryInput struct {
	ConfirmConflict bool `json:"confirmConflict"`
}

// DiscardRecoveryInput controls deletion of an already resolved Recovery
// record. Discard never changes Codex, Gateway, or Capture state, but it does
// clean the transaction backup owned by the Recovery record.
type DiscardRecoveryInput struct {
	Confirm        bool `json:"confirm"`
	DiscardUnsaved bool `json:"discardUnsaved"`
}

type recoveryRootEditor interface {
	ReadRootURL(context.Context) (codexconfig.RootURLSnapshot, error)
	PrepareRootURLChange(context.Context, *string, string) (*codexconfig.PreparedRootURLChange, error)
	CommitPreparedRootURLChange(context.Context, *codexconfig.PreparedRootURLChange) error
}

func (a *App) rootRecoveryEditor() (recoveryRootEditor, error) {
	editor, ok := a.codexConfig.(recoveryRootEditor)
	if !ok {
		return nil, errors.New("codex root editor unavailable")
	}
	return editor, nil
}

func (a *App) RestoreRecovery(input RestoreRecoveryInput) DesktopCommandResult {
	return a.executeRecoveryMutation("RestoreRecovery", func(ctx context.Context) (*DesktopSnapshot, error) {
		return a.restoreRecovery(ctx, input)
	})
}

func (a *App) DiscardRecovery(input DiscardRecoveryInput) DesktopCommandResult {
	return a.executeRecoveryMutation("DiscardRecovery", func(ctx context.Context) (*DesktopSnapshot, error) {
		return a.discardRecovery(ctx, input)
	})
}

func (a *App) executeRecoveryMutation(operation string, fn func(context.Context) (*DesktopSnapshot, error)) DesktopCommandResult {
	if !a.trafficMutationAllowed(operation) {
		return errDesktop(operation, "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	if a.closed.Load() {
		return hostClosed(operation)
	}
	ctx, cancel := context.WithTimeout(a.appCtx, trafficCommandTimeout)
	defer cancel()
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()
	value, err := fn(ctx)
	if err != nil {
		return recoveryBindingError(operation, err)
	}
	return okDesktop(value)
}

func recoveryBindingError(operation string, err error) DesktopCommandResult {
	if err == nil {
		return errDesktop(operation, "recovery", "recovery_operation_failed", "recovery operation failed", true)
	}
	code := "recovery_operation_failed"
	message := "recovery operation failed"
	retryable := true
	confirmation := false
	recoveryRequired := false
	switch {
	case errors.Is(err, errRecoveryConfirmationRequired):
		code, message, confirmation, retryable = "recovery_confirmation_required", "explicit recovery confirmation is required", true, false
	case errors.Is(err, errRecoveryConflict):
		code, message, confirmation, retryable = "recovery_config_conflict", "codex configuration changed; confirmation is required", true, false
	case errors.Is(err, errRecoveryUnknownPhase):
		code, message, recoveryRequired, retryable = "recovery_unknown_phase", "Recovery state requires explicit recovery handling", true, false
	case errors.Is(err, errRecoveryStateChanged):
		code, message, retryable = "recovery_state_changed", "Recovery state changed; retry the operation", true
	case errors.Is(err, errRecoveryUnsafe):
		code, message, recoveryRequired, retryable = "recovery_required", "Recovery state cannot be changed safely", true, false
	}
	result := errDesktop(operation, "recovery", code, message, retryable)
	result.Error.ConfirmationRequired = confirmation
	result.Error.RecoveryRequired = recoveryRequired
	return result
}

var (
	errRecoveryConfirmationRequired = errors.New("recovery confirmation required")
	errRecoveryConflict             = errors.New("recovery config conflict")
	errRecoveryUnknownPhase         = errors.New("recovery unknown phase")
	errRecoveryStateChanged         = errors.New("recovery state changed")
	errRecoveryUnsafe               = errors.New("recovery unsafe")
)

func (a *App) restoreRecovery(ctx context.Context, input RestoreRecoveryInput) (*DesktopSnapshot, error) {
	if a.recovery == nil {
		if _, err := a.ensureTrafficTransaction(); err != nil {
			return nil, errRecoveryUnsafe
		}
	}
	editor, err := a.rootRecoveryEditor()
	if err != nil {
		return nil, errRecoveryUnsafe
	}
	state, err := a.recovery.Load(ctx)
	if err != nil || state == nil {
		return nil, errRecoveryUnsafe
	}
	if state.SchemaVersion != recovery.SchemaVersion {
		return nil, errRecoveryUnknownPhase
	}
	if !recovery.IsKnownPhase(state.Phase) {
		return nil, errRecoveryUnknownPhase
	}
	if err := a.validateRecoveryConfigProfile(ctx, state); err != nil {
		return nil, err
	}
	traffic := a.traffic.Status()
	ownerID := ""
	releaseOwnership := false
	switch traffic.Mode {
	case trafficanalysis.ModeDesktop:
		// Live (same-process) conflict: the Desktop capture is owned by this
		// process's transaction. Enable stores the same identity in the tx
		// Service's ownerID and the capture's desktopOwnerID, and a failed
		// Disable keeps both. The recovery state's OperationID is the last
		// Disable's transaction, NOT the owner, so ownership is proven from the
		// live transaction. Pausing is idempotent for the already-passthrough
		// relay and mirrors Tauri's pause-before-restore ordering. A restarted
		// process has a fresh transaction with no owner, so the ownership-
		// guarded pause fails closed (errRecoveryUnsafe).
		tx, err := a.ensureTrafficTransaction()
		if err != nil {
			return nil, errRecoveryUnsafe
		}
		ownerID = tx.OwnerID()
		if ownerID == "" {
			return nil, errRecoveryUnsafe
		}
		if _, err := a.traffic.PauseDesktopExpected(ctx, traffic.Generation, traffic.GatewayInstanceID, traffic.GatewayAddress, ownerID); err != nil {
			return nil, errRecoveryUnsafe
		}
		releaseOwnership = true
	case trafficanalysis.ModeRecovery:
		// A restarted process has no safe owner proof. Never guess-release a
		// desktop-managed Capture while restoring configuration.
		return nil, errRecoveryUnsafe
	}
	current, err := editor.ReadRootURL(ctx)
	if err != nil {
		return nil, errRecoveryUnsafe
	}
	if current.ConfigHash != state.ConfigHashBeforeApply && current.ConfigHash != state.ConfigHashAfterApply {
		if !input.ConfirmConflict {
			_ = a.markRecoveryConflict(ctx, state)
			return nil, errRecoveryConflict
		}
	}
	if current.ConfigHash == state.ConfigHashBeforeApply {
		if err := a.cleanupRecoveryBackup(state); err != nil {
			return nil, errRecoveryUnsafe
		}
		if err := a.markRecoveryRestored(ctx, state); err != nil {
			return nil, errRecoveryUnsafe
		}
		if err := a.releaseDesktopOwnership(traffic, ownerID, releaseOwnership); err != nil {
			return nil, errRecoveryUnsafe
		}
		return a.desktopSnapshot(ctx)
	}
	if current.ConfigHash != state.ConfigHashAfterApply && !input.ConfirmConflict {
		return nil, errRecoveryConflict
	}
	var desired *string
	if state.PreviousOpenaiBaseURLPresent {
		value := ""
		if state.PreviousOpenaiBaseURL != nil {
			value = *state.PreviousOpenaiBaseURL
		}
		desired = &value
	}
	prepared, err := editor.PrepareRootURLChange(ctx, desired, current.ConfigHash)
	if err != nil {
		return nil, errRecoveryConflict
	}
	if err := editor.CommitPreparedRootURLChange(ctx, prepared); err != nil {
		return nil, errRecoveryUnsafe
	}
	verified, err := editor.ReadRootURL(ctx)
	if err != nil || verified.ConfigHash != prepared.AfterHash || !samePreviousURL(verified, state) {
		return nil, errRecoveryUnsafe
	}
	if err := a.cleanupRecoveryBackup(state); err != nil {
		return nil, errRecoveryUnsafe
	}
	if err := a.markRecoveryRestored(ctx, state); err != nil {
		return nil, errRecoveryUnsafe
	}
	if err := a.releaseDesktopOwnership(traffic, ownerID, releaseOwnership); err != nil {
		return nil, errRecoveryUnsafe
	}
	return a.desktopSnapshot(ctx)
}

// releaseDesktopOwnership hands a live Desktop capture back to capture-only
// mode after a successful restore. It is called only after the durable
// recovered marker is written; error paths keep ownership so the restore stays
// retryable. The ownership guard fails closed for a restarted process.
func (a *App) releaseDesktopOwnership(traffic trafficanalysis.State, ownerID string, release bool) error {
	if !release {
		return nil
	}
	_, err := a.traffic.ReleaseDesktopExpected(traffic.Generation, ownerID)
	return err
}

func (a *App) validateRecoveryConfigProfile(ctx context.Context, state *recovery.State) error {
	loaded, err := a.codexConfig.Load(ctx)
	if err != nil || !loaded.Exists {
		return errRecoveryUnsafe
	}
	home := filepath.Dir(loaded.Path)
	if state.ConfigPath != "config.toml" || filepath.Clean(loaded.Path) != filepath.Join(home, state.ConfigPath) {
		return errRecoveryUnsafe
	}
	fingerprint, err := recovery.CodexHomeFingerprint(home)
	if err != nil || state.CodexHomeFingerprint != fingerprint {
		return errRecoveryUnsafe
	}
	return nil
}

func samePreviousURL(current codexconfig.RootURLSnapshot, state *recovery.State) bool {
	if current.Present != state.PreviousOpenaiBaseURLPresent {
		return false
	}
	if !current.Present {
		return true
	}
	return state.PreviousOpenaiBaseURL != nil && current.Value == *state.PreviousOpenaiBaseURL
}

func (a *App) markRecoveryRestored(ctx context.Context, expected *recovery.State) error {
	return a.recovery.Update(ctx, func(current *recovery.State) error {
		if !sameRecoveryState(current, expected) {
			return errRecoveryStateChanged
		}
		current.IntegrationActive = false
		current.Phase = recovery.PhaseReconciledRestored
		current.CleanupPending = nil
		status := string(recovery.StatusAlreadyRestored)
		current.ReconciliationStatus = &status
		return nil
	})
}

func (a *App) markRecoveryConflict(ctx context.Context, expected *recovery.State) error {
	return a.recovery.Update(ctx, func(current *recovery.State) error {
		if !sameRecoveryState(current, expected) {
			return errRecoveryStateChanged
		}
		current.Phase = recovery.PhaseReconciliationConf
		status := string(recovery.StatusConfigConflict)
		current.ReconciliationStatus = &status
		return nil
	})
}

func sameRecoveryState(a, b *recovery.State) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SchemaVersion == b.SchemaVersion && a.IntegrationActive == b.IntegrationActive &&
		a.Phase == b.Phase && a.OperationID == b.OperationID &&
		a.ConfigHashBeforeApply == b.ConfigHashBeforeApply && a.ConfigHashAfterApply == b.ConfigHashAfterApply &&
		stringValue(a.BackupPath) == stringValue(b.BackupPath) &&
		cleanupPendingEqual(a.CleanupPending, b.CleanupPending) &&
		a.UnsavedObservationsMayRemain == b.UnsavedObservationsMayRemain &&
		a.UnsavedDiscardConfirmed == b.UnsavedDiscardConfirmed &&
		stringValue(a.UpdatedAt) == stringValue(b.UpdatedAt)
}

func cleanupPendingEqual(a, b *recovery.CleanupPending) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.TransactionID == b.TransactionID && a.BackupID == b.BackupID &&
		a.RouteMutationResult == b.RouteMutationResult && a.Status == b.Status
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (a *App) discardRecovery(ctx context.Context, input DiscardRecoveryInput) (*DesktopSnapshot, error) {
	if !input.Confirm {
		return nil, errRecoveryConfirmationRequired
	}
	if a.recovery == nil {
		return a.desktopSnapshot(ctx)
	}
	state, err := a.recovery.Load(ctx)
	if err != nil || state == nil {
		return a.desktopSnapshot(ctx)
	}
	if !recovery.IsKnownPhase(state.Phase) {
		return nil, errRecoveryUnknownPhase
	}
	if state.IntegrationActive || state.Phase == recovery.PhaseReconciliationReq || state.Phase == recovery.PhaseReconciliationConf {
		return nil, errRecoveryUnsafe
	}
	if state.UnsavedObservationsMayRemain && !input.DiscardUnsaved {
		return nil, errRecoveryConfirmationRequired
	}
	traffic := a.traffic.Status()
	if traffic.Mode == trafficanalysis.ModeDesktop || traffic.ListeningAddress != "" || traffic.Mode == trafficanalysis.ModeRecovery {
		return nil, errRecoveryUnsafe
	}
	if state.CleanupPending != nil {
		if err := a.cleanupRecoveryBackup(state); err != nil {
			return nil, errRecoveryUnsafe
		}
		err := a.recovery.Update(ctx, func(current *recovery.State) error {
			if !sameRecoveryState(current, state) || current.CleanupPending == nil {
				return errRecoveryStateChanged
			}
			current.IntegrationActive = false
			current.Phase = recovery.PhaseInactive
			current.CleanupPending = nil
			return nil
		})
		if err != nil {
			return nil, errRecoveryUnsafe
		}
		return a.desktopSnapshot(ctx)
	}
	deleted, err := a.recovery.DeleteIf(ctx, func(current *recovery.State) (bool, error) {
		return sameRecoveryState(current, state), nil
	})
	if err != nil {
		return nil, errRecoveryUnsafe
	}
	if !deleted {
		return nil, errRecoveryStateChanged
	}
	return a.desktopSnapshot(ctx)
}

// cleanupRecoveryBackup removes only the backup named by the current
// transaction ownership record. A missing expected artifact is already
// cleaned and therefore succeeds idempotently; invalid IDs and other delete
// failures remain retryable through the unchanged Recovery state.
func (a *App) cleanupRecoveryBackup(state *recovery.State) error {
	if state == nil {
		return nil
	}
	id := ""
	if state.CleanupPending != nil {
		id = state.CleanupPending.BackupID
	}
	if id == "" && state.BackupPath != nil {
		id = filepath.Base(*state.BackupPath)
	}
	if id == "" {
		return nil
	}
	if a.trafficBackupDir == "" {
		return errRecoveryUnsafe
	}
	path, err := codexconfig.ResolveBackupPath(a.trafficBackupDir, id)
	if err != nil {
		return errRecoveryUnsafe
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errRecoveryUnsafe
	}
	return nil
}
