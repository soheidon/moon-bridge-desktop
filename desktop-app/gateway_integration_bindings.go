package main

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/gatewayintegration"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/traffictransaction"
)

// gatewayRecoveryWriter bridges the outer Gateway integration checkpoint model
// onto the shared Recovery store. The OriginalOpenaiBaseURL fields are owned
// exclusively by the Gateway layer; the inner Traffic Analysis writer never
// touches them, so this writer is the only place they are created or cleared.
type gatewayRecoveryWriter struct {
	store      *recovery.Store
	configHome string
}

func (w gatewayRecoveryWriter) Current(ctx context.Context) (*gatewayintegration.Checkpoint, error) {
	st, err := w.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	cp := &gatewayintegration.Checkpoint{Target: gatewayintegration.TargetOriginal}
	if st == nil {
		return cp, nil
	}
	switch st.Target() {
	case recovery.TargetGateway:
		cp.Target = gatewayintegration.TargetGateway
	case recovery.TargetAnalysis:
		cp.Target = gatewayintegration.TargetAnalysis
	default:
		cp.Target = gatewayintegration.TargetOriginal
	}
	cp.OriginalPresent = st.OriginalOpenaiBaseURLPresent
	cp.OriginalValue = st.OriginalOpenaiBaseURL
	cp.AppliedValue = st.AppliedOpenaiBaseURL
	cp.Active = st.IntegrationActive
	return cp, nil
}

func (w gatewayRecoveryWriter) Checkpoint(ctx context.Context, cp gatewayintegration.Checkpoint) error {
	err := w.store.UpdateOrCreate(ctx, func(current *recovery.State) error {
		fp, err := recovery.CodexHomeFingerprint(w.configHome)
		if err != nil {
			return err
		}
		current.SchemaVersion = recovery.SchemaVersion
		switch cp.Target {
		case gatewayintegration.TargetGateway:
			current.IntegrationTarget = recovery.TargetGateway
		case gatewayintegration.TargetAnalysis:
			current.IntegrationTarget = recovery.TargetAnalysis
		default:
			current.IntegrationTarget = recovery.TargetOriginal
		}
		current.IntegrationActive = cp.Active
		current.OriginalOpenaiBaseURLPresent = cp.OriginalPresent
		current.OriginalOpenaiBaseURL = cp.OriginalValue
		current.AppliedOpenaiBaseURL = cp.AppliedValue
		current.ConfigHashBeforeApply = cp.ConfigHashBeforeApply
		current.ConfigHashAfterApply = cp.ConfigHashAfterApply
		current.ConfigPath = "config.toml"
		current.CodexHomeFingerprint = fp
		if current.StartedAt == "" {
			current.StartedAt = time.Now().UTC().Format(time.RFC3339)
		}
		switch cp.Phase {
		case gatewayintegration.PhaseIntegrationApplied:
			current.Phase = recovery.PhaseIntegrationApplied
		case gatewayintegration.PhaseRecoveryRequired:
			current.Phase = recovery.PhaseReconciliationReq
		default:
			current.Phase = recovery.PhaseInactive
		}
		return nil
	})
	if err != nil {
		log.Printf("gateway integration checkpoint failed: target=%q phase=%q", cp.Target, cp.Phase)
		return err
	}
	return nil
}

// ensureGatewayIntegration binds the outer Gateway integration service to the
// resolved Codex config path and the shared Recovery store. It is lazy like
// ensureTrafficTransaction so the normal Wails path and injected test seams
// resolve to the same profile.
func (a *App) ensureGatewayIntegration() (*gatewayintegration.Service, error) {
	configPath, err := a.resolveCodexConfigPath(context.Background())
	if err != nil {
		return nil, err
	}
	configHome := filepath.Dir(configPath)
	if a.gatewayInt != nil {
		if a.gatewayIntConfigPath == configPath {
			return a.gatewayInt, nil
		}
		return nil, errors.New("gateway integration is bound to another codex profile")
	}
	if err := a.ensureRecoveryStore(configHome); err != nil {
		return nil, err
	}
	store := a.recovery
	configEditor := codexconfig.New(codexconfig.Options{Home: configHome, BackupDir: a.trafficBackupDir})
	a.gatewayInt = gatewayintegration.New(configEditor, gatewayRecoveryWriter{store: store, configHome: configHome}, "http://"+traffictransaction.CaptureListenAddress)
	a.gatewayIntConfigPath = configPath
	return a.gatewayInt, nil
}

// logGatewayIntegrationError writes a single safe diagnostic line for a failed
// Enable/Disable. It never logs URLs, config bodies, or secrets: the stage and
// current-target are fixed enums, and the kind is derived via safeError.
func logGatewayIntegrationError(operation string, err error) {
	var ge *gatewayintegration.Error
	if errors.As(err, &ge) {
		log.Printf("gateway integration error: operation=%q stage=%q kind=%q current_target=%q rollback=%q",
			ge.Operation, ge.Stage, safeError(err), ge.CurrentTarget, ge.Rollback)
		return
	}
	log.Printf("gateway integration error: operation=%q kind=%q", operation, safeError(err))
}

// logGatewayDisable writes a single safe diagnostic line for a Disable call,
// success or failure. It never logs URLs, config bodies, or secrets: the
// recovery target and before/after classifications are fixed enums, and the
// kind/stage are derived via safeError.
func logGatewayDisable(ok bool, report gatewayintegration.DisableReport, err error) {
	if ok {
		log.Printf("gateway integration disable: ok=true recovery_target=%q original_present=%t before=%q restored=%t after=%q",
			report.RecoveryTarget, report.OriginalPresent, report.Before, report.Restored, report.After)
		return
	}
	var ge *gatewayintegration.Error
	if errors.As(err, &ge) {
		log.Printf("gateway integration disable: ok=false stage=%q kind=%q recovery_target=%q original_present=%t before=%q",
			ge.Stage, safeError(err), report.RecoveryTarget, report.OriginalPresent, report.Before)
		return
	}
	log.Printf("gateway integration disable: ok=false kind=%q recovery_target=%q original_present=%t before=%q",
		safeError(err), report.RecoveryTarget, report.OriginalPresent, report.Before)
}
