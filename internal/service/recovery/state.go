// Package recovery persists and reconciles the Moon Bridge crash-recovery state.
// The on-disk JSON schema (camelCase field names) matches the old Rust
// RecoveryState in desktop/src-tauri/src/traffic_analysis.rs so the Go port can
// read existing v2 files. Secrets and absolute paths are never stored.
package recovery

import (
	"encoding/json"
	"errors"
	"time"
)

// SchemaVersion is the Recovery v2 schema version written by the Go port.
const SchemaVersion = 2

// Phase is the lifecycle phase of a traffic-analysis / recovery transaction.
// The four base values come from the Rust contract
// (prepared/capture_started/integration_applied/aborted); the extended values
// are used by reconciliation and capture restart.
type Phase string

const (
	PhasePrepared           Phase = "prepared"
	PhaseCaptureStarted     Phase = "capture_started"
	PhaseIntegrationApplied Phase = "integration_applied"
	PhaseAborted            Phase = "aborted"

	PhaseRestartPrepared    Phase = "restart_prepared"
	PhaseRestartFailed      Phase = "restart_failed"
	PhaseCaptureRestarted   Phase = "capture_restarted"
	PhaseReconciliationReq  Phase = "reconciliation_required"
	PhaseReconciledRestored Phase = "reconciled_restored"
	PhaseReconciliationConf Phase = "reconciliation_conflict"
	PhaseRecovered          Phase = "recovered"
	PhaseInactive           Phase = "inactive"
)

// IntegrationTarget is the layer that currently owns the Codex openai_base_url
// redirection. It extends the legacy two-state model (original vs capture) with
// the Gateway integration layer, so a crash can be rolled back to the true
// original upstream regardless of which layer was active.
type IntegrationTarget string

const (
	TargetOriginal IntegrationTarget = "original"
	// TargetGateway means "Codex openai_base_url is integrated at the stable
	// front door :38440". Gateway ON/OFF now only flips the front door's
	// forwarding target; the config stays at :38440 until MBD shutdown.
	TargetGateway  IntegrationTarget = "gateway"
	// TargetAnalysis is legacy/dead in the front-door model: the config is never
	// rewritten to the capture listener (:38441) anymore. It is retained so old
	// records still classify without a schema migration.
	TargetAnalysis IntegrationTarget = "analysis"
)

// FrontDoorBaseURL is the stable endpoint Codex's openai_base_url points to once
// integrated in the front-door model. It mirrors traffictransaction.FrontDoorAddress;
// recovery keeps a local copy because importing the transaction layer would create
// an import cycle. New-model records store exactly this value (no trailing slash)
// in AppliedOpenaiBaseURL.
const FrontDoorBaseURL = "http://127.0.0.1:38440"

// Target resolves the current integration layer. Records written before the
// three-state model have no integrationTarget; they are inferred from
// IntegrationActive (legacy only ever activated the analysis/capture layer).
func (s *State) Target() IntegrationTarget {
	if s == nil {
		return TargetOriginal
	}
	if s.IntegrationTarget != "" {
		return s.IntegrationTarget
	}
	if s.IntegrationActive {
		return TargetAnalysis
	}
	return TargetOriginal
}

// OriginalBaseURL returns the true original upstream to restore to when a full
// integration teardown is required. New-model records store it explicitly
// (Gateway layer only); a legacy record predating the Gateway layer stored the
// original upstream in PreviousOpenaiBaseURL, so that is the fallback.
func (s *State) OriginalBaseURL() (*string, bool) {
	if s == nil {
		return nil, false
	}
	if s.OriginalOpenaiBaseURLPresent {
		return s.OriginalOpenaiBaseURL, true
	}
	if s.IntegrationTarget != "" {
		// New three-state record: the Gateway layer is the sole owner of the true
		// original, so OriginalOpenaiBaseURLPresent=false means the original had no
		// openai_base_url key. The inner PreviousOpenaiBaseURL is the gateway URL,
		// not the original, and must not be used as a restore target.
		return nil, false
	}
	// Legacy two-state record: the original upstream was stored in
	// PreviousOpenaiBaseURL.
	return s.PreviousOpenaiBaseURL, s.PreviousOpenaiBaseURLPresent
}

// IsKnownPhase reports whether a persisted Recovery phase is part of the
// schema-v2 contract. Unknown strings are decoded for forward compatibility,
// but callers must classify them as recovery-required before taking action.
func IsKnownPhase(phase Phase) bool {
	switch phase {
	case PhasePrepared, PhaseCaptureStarted, PhaseIntegrationApplied,
		PhaseAborted, PhaseRestartPrepared, PhaseRestartFailed,
		PhaseCaptureRestarted, PhaseReconciliationReq,
		PhaseReconciledRestored, PhaseReconciliationConf,
		PhaseRecovered, PhaseInactive:
		return true
	default:
		return false
	}
}

// DefaultPhase matches the Rust default of "integration_applied".
const DefaultPhase = PhaseIntegrationApplied

// ReconciliationStatus records the outcome of a startup reconciliation. It
// never rewrites the codex config and never starts gateway/capture.
type ReconciliationStatus string

const (
	StatusPendingRestore    ReconciliationStatus = "pending_restore"
	StatusAlreadyRestored   ReconciliationStatus = "already_restored"
	StatusConfigConflict    ReconciliationStatus = "config_conflict"
	StatusConfigUnreadable  ReconciliationStatus = "config_unreadable"
	StatusConfigPathInvalid ReconciliationStatus = "config_path_invalid"
	StatusInactive          ReconciliationStatus = "inactive"
	// StatusIntegrated is a coherent integrated state: the Codex config is at the
	// stable front door and the record is consistent with it. No recovery is
	// required; the caller just reopens the front door.
	StatusIntegrated ReconciliationStatus = "integrated"
)

// AutoLogRecoveryState mirrors the Rust autoLog sub-object.
type AutoLogRecoveryState struct {
	SessionID              string  `json:"sessionId"`
	Path                   string  `json:"path"` // traffic-log root 相対
	LastCheckpointSequence uint64  `json:"lastCheckpointSequence"`
	Finalized              bool    `json:"finalized"`
	LastCheckpointAt       *string `json:"lastCheckpointAt,omitempty"`
}

// RecoveryMigrationState records a completed v1→v2 migration. sourcePath is
// stored as a logical identifier or APPDATA-relative, never an arbitrary path.
type RecoveryMigrationState struct {
	SourcePath          string `json:"sourcePath"`
	SourceSchemaVersion int    `json:"sourceSchemaVersion"`
	MigratedAt          string `json:"migratedAt"`
}

// CleanupPending records a backup cleanup that is independent from route recovery.
type CleanupPending struct {
	TransactionID       string `json:"transactionId"`
	BackupID            string `json:"backupId"`
	RouteMutationResult string `json:"routeMutationResult"`
	Status              string `json:"status"`
}

// State is the persisted crash-recovery record. JSON keys are camelCase and
// match the Rust RecoveryState; startup uses SchemaVersion to reject unsupported
// versions and ignores unknown fields.
type State struct {
	SchemaVersion     int   `json:"schemaVersion"`
	IntegrationActive bool  `json:"integrationActive"`
	Phase             Phase `json:"phase"`

	OperationID string `json:"operationId"`
	// TransitionID is the route epoch. It is intentionally separate from
	// OperationID, which belongs to an individual Traffic transaction.
	TransitionID         string `json:"transitionId,omitempty"`
	RoutePhase           string `json:"routePhase,omitempty"`
	DesiredRoute         string `json:"desiredRoute,omitempty"`
	RouteEvidence        string `json:"routeEvidence,omitempty"`
	ConfigPath           string `json:"configPath"`                     // CODEX_HOME 相対
	CodexHomeFingerprint string `json:"codexHomeFingerprint,omitempty"` // 開始時 CODEX_HOME 正規化値の SHA-256（相対 configPath の root 照合）

	PreviousOpenaiBaseURLPresent bool    `json:"previousOpenaiBaseUrlPresent"`
	PreviousOpenaiBaseURL        *string `json:"previousOpenaiBaseUrl,omitempty"` // 適用前（存在時）
	AppliedOpenaiBaseURL         string  `json:"appliedOpenaiBaseUrl"`

	// IntegrationTarget records which layer currently owns the Codex
	// openai_base_url redirection (original/gateway/analysis). OriginalOpenaiBaseURL
	// is the true original upstream recorded by the Gateway layer and preserved
	// across the inner (analysis) layer's checkpoints; only the Gateway layer
	// creates or clears it.
	IntegrationTarget          IntegrationTarget `json:"integrationTarget,omitempty"`
	OriginalOpenaiBaseURLPresent bool            `json:"originalOpenaiBaseUrlPresent"`
	OriginalOpenaiBaseURL        *string         `json:"originalOpenaiBaseUrl,omitempty"`

	ConfigHashBeforeApply string  `json:"configHashBeforeApply"` // 全文 SHA-256
	ConfigHashAfterApply  string  `json:"configHashAfterApply"`  // 全文 SHA-256
	BackupPath            *string `json:"backupPath,omitempty"`  // backup root 相対

	StartedAt string  `json:"startedAt"` // rfc3339
	UpdatedAt *string `json:"updatedAt,omitempty"`

	AutoLog       *AutoLogRecoveryState `json:"autoLog,omitempty"`
	AutoLogStatus *string               `json:"autoLogStatus,omitempty"`

	UnsavedObservationsMayRemain bool `json:"unsavedObservationsMayRemain"`
	UnsavedDiscardConfirmed      bool `json:"unsavedDiscardConfirmed"`

	Migration      *RecoveryMigrationState `json:"migration,omitempty"`
	CleanupPending *CleanupPending         `json:"cleanupPending,omitempty"`

	CaptureStateLastKnown string `json:"captureStateLastKnown,omitempty"`
	RelayActiveLastKnown  bool   `json:"relayActiveLastKnown"`

	ReconciliationStatus *string `json:"reconciliationStatus,omitempty"`
	ReconciledAt         *string `json:"reconciledAt,omitempty"`
	ReconciliationDetail *string `json:"reconciliationDetail,omitempty"`

	RestartAttempted bool `json:"restartAttempted"`
}

// New returns a fresh zero-value State stamped with SchemaVersion. It does NOT
// set a phase: a new session state must be explicitly transitioned (e.g. to
// prepared/capture_started) before write, and writeLocked/normalizeForWrite
// rejects a phase-less state rather than silently persisting `phase:""`.
// DefaultPhase is the serde default applied when DESERIALIZING a file that
// omitted phase (matching Rust), not a value New auto-assigns.
func New() *State {
	return &State{SchemaVersion: SchemaVersion}
}

// WithUpdatedAt returns a copy with UpdatedAt set to now (rfc3339). SchemaVersion
// is left untouched: only the v1→v2 migration writes a specific version, and
// write-time validation rejects anything other than 2.
func (s *State) WithUpdatedAt(now time.Time) *State {
	out := *s
	if now.IsZero() {
		now = time.Now()
	}
	ts := now.UTC().Format(time.RFC3339)
	out.UpdatedAt = &ts
	return &out
}

// UnmarshalJSON mirrors the Rust serde phase contract. Rust's
// `#[serde(default = "default_recovery_phase")] phase: String` means a MISSING
// key deserializes to integration_applied, but a null into a String is a
// deserialize error and an explicit "" is kept verbatim. encoding/json has no
// serde-style defaults, so the missing-key branch is implemented here; the
// null branch returns an error (Rust would reject the file), and an explicit
// empty value is kept as-is (write-time validation rejects phase=="").
func (s *State) UnmarshalJSON(data []byte) error {
	type state State
	var present struct {
		Phase json.RawMessage `json:"phase"`
	}
	if err := json.Unmarshal(data, &present); err != nil {
		return err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	*s = State(st)
	switch {
	case len(present.Phase) == 0:
		s.Phase = DefaultPhase
	case string(present.Phase) == "null":
		return errors.New("recovery phase must not be null")
	}
	return nil
}

// StringPtr is a helper for constructing optional string fields.
func StringPtr(s string) *string {
	return &s
}
