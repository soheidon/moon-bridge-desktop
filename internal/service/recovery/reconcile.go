package recovery

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// Result is the output of a startup reconciliation. Reconciliation never
// rewrites the codex config, never starts gateway/capture, and never auto-
// restores; it only records the classification into the recovery-state JSON.
type Result struct {
	StatusReconciled bool
	Status           ReconciliationStatus
	Phase            *Phase // desired new phase, or nil to leave unchanged
	Detail           string
}

// ReconcileStartup performs the startup reconciliation: it reads the persisted
// recovery state and the current codex config, classifies the outcome, and
// records only the diagnosis to the recovery-state JSON (permitted). It never
// modifies the codex config, never boots gateway/capture, and never auto-
// restores.
//
// The whole classify-and-persist runs inside a single Store transaction
// (updateReconciled holds the Store mutex for raw-read->mutate->raw-patch), so
// a concurrent Delete cannot leave the callback with a nil state: the callback
// treats nil as "no work" and aborts without persisting, returning an inactive
// result.
func (s *Store) ReconcileStartup(ctx context.Context, readConfig func(path string) ([]byte, error)) (*Result, error) {
	var res *Result
	err := s.updateReconciled(ctx, func(cur *State) error {
		if cur == nil {
			// State was deleted between loss/startup; nothing to reconcile.
			res = &Result{StatusReconciled: false, Status: StatusInactive, Detail: "no recovery state"}
			return errChangesSkipped
		}
		if readConfig == nil {
			// A persisted state needs the config reader to classify; a nil reader
			// must not panic.
			return newError(KindReconcileFailed, "readConfig is nil")
		}
		// Resolve configPath: relative (new) is root-bound via codexHomeFingerprint;
		// absolute (legacy) must stay physically inside the current CODEX_HOME.
		configPath, err := s.resolveConfigPath(cur)
		if err != nil {
			// The stored path is unusable this boot (empty, outside CodexHome,
			// fingerprint absent/mismatched, reparse escape). The diagnosis is
			// recorded WITHOUT clearing the stored path: the classification writer
			// preserves the original evidence (scalar/string values unchanged,
			// unknown/nested fields semantically), so the legacy value stays on
			// disk for the next boot / management decision. Restore is disabled by
			// the recorded status, not by erasing the path.
			st := StatusConfigPathInvalid
			cur.ReconciliationStatus = strPtr(string(st))
			now := nowString()
			cur.ReconciledAt = &now
			cur.ReconciliationDetail = strPtr("codex config path is invalid for this boot")
			p := PhaseReconciliationConf
			cur.Phase = p
			res = &Result{StatusReconciled: true, Status: st, Phase: &p, Detail: err.Error()}
			return nil
		}
		data, rerr := readConfig(configPath)
		if rerr != nil || data == nil {
			st := StatusConfigUnreadable
			now := nowString()
			cur.ReconciliationStatus = strPtr(string(st))
			cur.ReconciledAt = &now
			cur.ReconciliationDetail = strPtr("codex config unreadable")
			p := PhaseReconciliationConf
			cur.Phase = p
			res = &Result{StatusReconciled: true, Status: st, Phase: &p, Detail: "config unreadable"}
			return nil
		}
		curHash := HashBytes(data)
		classify := s.classifyCur(cur, curHash)
		now := nowString()
		cur.ReconciliationStatus = strPtr(string(classify.Status))
		cur.ReconciledAt = &now
		if classify.Detail == "" {
			// A classification with no detail (e.g. inactive) must not leave a
			// stale warning from a previous boot (config_conflict etc.): clear it
			// so the persisted reconciliationDetail matches Result.Detail.
			cur.ReconciliationDetail = nil
		} else {
			cur.ReconciliationDetail = strPtr(classify.Detail)
		}
		if classify.Phase != "" {
			cur.Phase = classify.Phase
		}
		if classify.Status == StatusAlreadyRestored {
			cur.IntegrationActive = false
		}
		res = &Result{StatusReconciled: true, Status: classify.Status, Phase: classify.PhasePtr(), Detail: classify.Detail}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// classification mirrors the Rust classify_recovery_after_startup ordering.
type classification struct {
	Status ReconciliationStatus
	Phase  Phase
	Detail string
}

func (c classification) PhasePtr() *Phase {
	if c.Phase == "" {
		return nil
	}
	p := c.Phase
	return &p
}

// classifyCur classifies the current config hash against the persisted recovery
// state, mirroring the Rust classify_recovery_after_startup. It never rewrites
// the codex config, never boots gateway/capture, and never auto-restores; the
// caller records only the classification into the recovery-state JSON.
//
//   - applied: current hash == configHashAfterApply (capture-applied candidate)
//   - original: current hash == configHashBeforeApply (reverted)
//   - neither:  external change during an active integration → conflict
func (s *Store) classifyCur(cur *State, curHash string) classification {
	if !IsKnownPhase(cur.Phase) {
		return classification{Status: StatusPendingRestore, Phase: PhaseReconciliationReq,
			Detail: "Recovery phase is not supported; explicit recovery handling is required"}
	}
	applied := cur.ConfigHashAfterApply != "" && curHash == cur.ConfigHashAfterApply
	original := cur.ConfigHashBeforeApply != "" && curHash == cur.ConfigHashBeforeApply

	switch {
	case applied && (cur.IntegrationActive || cur.Phase == PhasePrepared || cur.Phase == PhaseCaptureStarted):
		return classification{Status: StatusPendingRestore, Phase: PhaseReconciliationReq,
			Detail: "Codex設定はCapture用の適用値です。自動再適用は行わず、復元確認が必要です"}
	case original && cur.IntegrationActive:
		return classification{Status: StatusAlreadyRestored, Phase: PhaseReconciledRestored,
			Detail: "Codex設定は元の接続先へ戻されています"}
	case cur.IntegrationActive:
		// active integration but current config is neither applied nor original
		// → externally changed → conflict (no auto-restore).
		return classification{Status: StatusConfigConflict, Phase: PhaseReconciliationConf,
			Detail: "外部変更が検出されました。自動復元しません"}
	case applied:
		// not integration-active but still capture-applied → pending manual restore.
		return classification{Status: StatusPendingRestore, Phase: PhaseReconciliationReq,
			Detail: "Codex設定はCapture用の適用値です。復元確認が必要です"}
	default:
		return classification{Status: StatusInactive, Phase: PhaseInactive, Detail: ""}
	}
}

// resolveConfigPath resolves the stored config path against the current codex
// home. Absolute (legacy) paths must stay physically inside the current home
// (lexical boundary + symlinks/junctions/reparse cannot escape). Relative (new
// contract) paths are root-bound by codexHomeFingerprint: an absent fingerprint
// or a CODEX_HOME change since the session is config_path_invalid (conservative;
// no auto-restore).
func (s *Store) resolveConfigPath(st *State) (string, error) {
	if st == nil {
		return "", &Error{Kind: KindConfigPathInvalid, Message: "recovery state is nil"}
	}
	stored := st.ConfigPath
	if stored == "" {
		return "", &Error{Kind: KindConfigPathInvalid, Message: "recovery config path is empty"}
	}
	stored = stripVerbatimPrefix(stored)
	if filepath.IsAbs(stored) {
		if s.paths.CodexHome == "" {
			return "", &Error{Kind: KindConfigPathChangedError, Message: "codex home is unavailable for legacy absolute path"}
		}
		if !pathWithinPhysical(s.paths.CodexHome, stored) {
			return "", &Error{Kind: KindConfigPathChangedError, Message: "recovery config path points outside the current codex home"}
		}
		return filepath.Clean(stored), nil
	}
	if s.paths.CodexHome == "" {
		return "", &Error{Kind: KindConfigPathInvalid, Message: "codex home is unavailable"}
	}
	if st.CodexHomeFingerprint == "" {
		return "", &Error{Kind: KindConfigPathInvalid, Message: "recovery config path has no codex home fingerprint"}
	}
	fp, err := CodexHomeFingerprint(s.paths.CodexHome)
	if err != nil {
		return "", &Error{Kind: KindConfigPathInvalid, Message: "cannot fingerprint the current codex home"}
	}
	if fp != st.CodexHomeFingerprint {
		return "", &Error{Kind: KindConfigPathChangedError, Message: "codex home changed since the traffic session"}
	}
	// The stored relative path was root-bound at session start, but the config
	// file or any directory on the way may have been swapped to a symlink /
	// junction since then. Validate the PHYSICAL location at use time so the
	// resolved path can never point outside the current codex home.
	resolved, err := Resolve(s.paths.CodexHome, stored)
	if err != nil {
		return "", &Error{Kind: KindConfigPathInvalid, Message: "recovery config path escapes codex home"}
	}
	if !pathWithinPhysical(s.paths.CodexHome, resolved) {
		return "", &Error{Kind: KindConfigPathInvalid, Message: "recovery config path resolves outside the current codex home"}
	}
	return resolved, nil
}

// ConfigPathFor returns the absolute codex config path for the current recovery
// state (used by Restore). Returns "" if unresolved or missing on disk.
func (s *Store) ConfigPathFor(st *State) string {
	if st == nil {
		return ""
	}
	p, err := s.resolveConfigPath(st)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func nowString() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func strPtr(s string) *string { return &s }
