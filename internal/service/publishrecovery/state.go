// Package publishrecovery implements the durable crash journal for Codex home
// publishes (models_catalog.json / auth.json / config.toml). A transaction is
// recorded phase-by-phase so a crash mid-publish can be reconciled on the next
// startup. The journal never stores absolute paths or raw secrets: the target
// home is referenced by fingerprint and file expectations by SHA-256 hash.
package publishrecovery

import (
	"regexp"
	"time"
)

// SchemaVersion is the current publish journal schema version.
const SchemaVersion = 1

// Phase is a journal phase. The forward phases form the publish pipeline; the
// rollback phases branch off any of them and are validated against
// RollbackFromPhase — phase ordering is never compared as a linear sequence.
type Phase string

const (
	// Forward phases.
	PhasePrepared         Phase = "prepared"
	PhaseBackoutCopied    Phase = "backout_copied"
	PhaseCatalogPublished Phase = "catalog_published"
	PhaseAuthPublished    Phase = "auth_published"
	PhaseConfigPublished  Phase = "config_published"
	PhaseVerified         Phase = "verified"
	PhaseCompleted        Phase = "completed"
	// Terminal discard state: the intermediate phase a discard journal is advanced
	// to before its backout and journal are removed, so a crash mid-cleanup never
	// leaves an orphaned backout (see discardAfterBackout).
	PhaseDiscarded Phase = "discarded"
	// Rollback phases.
	PhaseRollbackRequired Phase = "rollback_required"
	PhaseRolledBack       Phase = "rolled_back"
	PhaseRollbackFailed   Phase = "rollback_failed"
)

// FileID identifies one of the three files a publish may touch. Only these enum
// values are valid: the journal never carries arbitrary paths.
type FileID string

const (
	FileModelsCatalog FileID = "models_catalog"
	FileAuth          FileID = "auth"
	FileConfig        FileID = "config"
)

// ExpectedFile records the post-publish expectation for one target file: whether
// it must exist and, if so, the SHA-256 of its expected content. An expected
// hash is confidentiality-bearing metadata (it can be used to test a secret
// against the file) and must never be surfaced in UI, logs, or error details.
type ExpectedFile struct {
	File          FileID `json:"file"`
	ExpectedExist bool   `json:"expectedExists"`
	SHA256        string `json:"sha256,omitempty"`
}

// Journal is the durable publish crash journal.
type Journal struct {
	SchemaVersion         int            `json:"schemaVersion"`
	TransactionID         string         `json:"transactionId"`
	Phase                 Phase          `json:"phase"`
	StartedAt             string         `json:"startedAt"`
	UpdatedAt             string         `json:"updatedAt"`
	CompletedAt           *string        `json:"completedAt,omitempty"`
	TargetHomeFingerprint string         `json:"targetHomeFingerprint"`
	ExpectedFiles         []ExpectedFile `json:"expectedFiles"`
	PublishedFiles        []FileID       `json:"publishedFiles,omitempty"`
	AuthRequired          bool           `json:"authRequired"`
	CommitMarkerPublished bool           `json:"commitMarkerPublished"`
	BackoutManifestSHA256 string         `json:"backoutManifestSha256,omitempty"`
	RollbackAttempted     bool           `json:"rollbackAttempted"`
	// TargetHomeInitiallyAbsent records that the target CODEX_HOME directory
	// did not exist when this transaction started (first-run). Rollback and
	// startup reconciliation remove the (now-empty) target directory when this
	// flag is set and all publish files have been restored to their pre-publish
	// (absent) state, so a first-run crash never leaves an orphaned empty
	// directory.
	TargetHomeInitiallyAbsent bool   `json:"targetHomeInitiallyAbsent,omitempty"`
	RollbackFromPhase         *Phase `json:"rollbackFromPhase,omitempty"`
}

var forwardPhases = map[Phase]bool{
	PhasePrepared:         true,
	PhaseBackoutCopied:    true,
	PhaseCatalogPublished: true,
	PhaseAuthPublished:    true,
	PhaseConfigPublished:  true,
	PhaseVerified:         true,
	PhaseCompleted:        true,
}

var rollbackPhases = map[Phase]bool{
	PhaseRollbackRequired: true,
	PhaseRolledBack:       true,
	PhaseRollbackFailed:   true,
}

// rollbackFromAllowed are the forward branch points a rollback phase may derive
// from. Rollback never branches from verified/completed (a completed publish has
// nothing to restore) nor from another rollback phase.
var rollbackFromAllowed = map[Phase]bool{
	PhasePrepared:         true,
	PhaseBackoutCopied:    true,
	PhaseCatalogPublished: true,
	PhaseAuthPublished:    true,
	PhaseConfigPublished:  true,
}

func validPhase(p Phase) bool        { return forwardPhases[p] || rollbackPhases[p] || p == PhaseDiscarded }
func isRollbackPhase(p Phase) bool   { return rollbackPhases[p] }
func validRollbackFrom(p Phase) bool { return rollbackFromAllowed[p] }

var hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate enforces the journal schema contract. Phase rules are split into
// explicit forward and rollback branches — rollback phases derive their
// publishedFiles/commitMarkerPublished expectations from RollbackFromPhase, so
// no phase string is ever compared by ordering. On error the journal must not
// be persisted (the Store validates before writing).
func (j *Journal) Validate() error {
	if j.SchemaVersion != SchemaVersion {
		return newError(KindTransactionInvalid, "unsupported publish journal schema version")
	}
	if err := ValidateTransactionID(j.TransactionID); err != nil {
		return err
	}
	if !validPhase(j.Phase) {
		return newError(KindTransactionInvalid, "unknown publish phase")
	}
	if isRollbackPhase(j.Phase) != j.RollbackAttempted {
		return newError(KindTransactionInvalid, "rollbackAttempted must match the phase")
	}
	if !hex64RE.MatchString(j.TargetHomeFingerprint) {
		return newError(KindTransactionInvalid, "target home fingerprint must be a sha256 hex")
	}
	started, err := parseUTCRFC3339("startedAt", j.StartedAt)
	if err != nil {
		return err
	}
	updated, err := parseUTCRFC3339("updatedAt", j.UpdatedAt)
	if err != nil {
		return err
	}
	if updated.Before(started) {
		return newError(KindTransactionInvalid, "updatedAt must not be before startedAt")
	}
	expected, marker, err := j.expectedPublishFor()
	if err != nil {
		return err
	}
	if err := validateExpectedFiles(j); err != nil {
		return err
	}
	if !fileIDsEqual(j.PublishedFiles, expected) {
		return newError(KindTransactionInvalid, "publishedFiles do not match the phase")
	}
	if !subsetOfExpectedFiles(j.PublishedFiles, j.ExpectedFiles) {
		return newError(KindTransactionInvalid, "publishedFiles reference a file outside expectedFiles")
	}
	if j.CommitMarkerPublished != marker {
		return newError(KindTransactionInvalid, "commitMarkerPublished does not match the phase")
	}
	if j.Phase == PhaseCompleted {
		if j.CompletedAt == nil || *j.CompletedAt == "" {
			return newError(KindTransactionInvalid, "completed phase requires completedAt")
		}
		completed, err := parseUTCRFC3339("completedAt", *j.CompletedAt)
		if err != nil {
			return err
		}
		if completed.Before(started) {
			return newError(KindTransactionInvalid, "completedAt must not be before startedAt")
		}
	} else if j.CompletedAt != nil {
		return newError(KindTransactionInvalid, "completedAt is only valid on the completed phase")
	}
	if j.BackoutManifestSHA256 != "" && !hex64RE.MatchString(j.BackoutManifestSHA256) {
		return newError(KindTransactionInvalid, "backout manifest hash must be a sha256 hex")
	}
	if j.Phase != PhasePrepared && j.Phase != PhaseDiscarded && j.BackoutManifestSHA256 == "" {
		return newError(KindTransactionInvalid, "backout manifest hash is required after prepared")
	}
	return nil
}

// parseUTCRFC3339 parses a required timestamp that must be RFC3339 and UTC.
func parseUTCRFC3339(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, newError(KindTransactionInvalid, name+" is required")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, newError(KindTransactionInvalid, name+" must be RFC3339")
	}
	if _, offset := parsed.Zone(); offset != 0 {
		return time.Time{}, newError(KindTransactionInvalid, name+" must be UTC")
	}
	return parsed, nil
}

// expectedPublishFor returns the (publishedFiles, commitMarkerPublished) pair the
// journal must carry for its phase. Rollback phases delegate to
// RollbackFromPhase (the durable branch point), so the expectations are always
// derived without comparing phase strings in order.
func (j *Journal) expectedPublishFor() ([]FileID, bool, error) {
	if isRollbackPhase(j.Phase) {
		if j.RollbackFromPhase == nil {
			return nil, false, newError(KindTransactionInvalid, "rollback phase requires rollbackFromPhase")
		}
		if !validRollbackFrom(*j.RollbackFromPhase) {
			return nil, false, newError(KindTransactionInvalid, "rollbackFromPhase must be a forward branch phase")
		}
		return publishForForward(*j.RollbackFromPhase)
	}
	if j.RollbackFromPhase != nil {
		return nil, false, newError(KindTransactionInvalid, "forward phase must not set rollbackFromPhase")
	}
	if j.Phase == PhaseDiscarded {
		return nil, false, nil
	}
	return publishForForward(j.Phase)
}

// publishForForward maps a forward phase (or a valid rollback branch point) to
// its publishedFiles/commitMarkerPublished expectations. The publish order is
// catalog, then auth, then config (config is committed last).
func publishForForward(phase Phase) ([]FileID, bool, error) {
	switch phase {
	case PhasePrepared, PhaseBackoutCopied:
		return nil, false, nil
	case PhaseCatalogPublished:
		return []FileID{FileModelsCatalog}, false, nil
	case PhaseAuthPublished:
		return []FileID{FileModelsCatalog, FileAuth}, false, nil
	case PhaseConfigPublished, PhaseVerified, PhaseCompleted:
		return []FileID{FileModelsCatalog, FileAuth, FileConfig}, true, nil
	default:
		return nil, false, newError(KindTransactionInvalid, "unknown publish phase")
	}
}

func validateExpectedFiles(j *Journal) error {
	want := []FileID{FileModelsCatalog, FileAuth, FileConfig}
	if len(j.ExpectedFiles) != len(want) {
		return newError(KindTransactionInvalid, "expectedFiles must contain exactly the three publish targets")
	}
	for i, id := range want {
		if j.ExpectedFiles[i].File != id {
			return newError(KindTransactionInvalid, "expectedFiles must be ordered models_catalog, auth, config")
		}
	}
	for _, ef := range j.ExpectedFiles {
		if ef.ExpectedExist {
			if !hex64RE.MatchString(ef.SHA256) {
				return newError(KindTransactionInvalid, "an expected existing file requires its sha256 hash")
			}
		} else if ef.SHA256 != "" {
			return newError(KindTransactionInvalid, "an expected absent file must not carry a sha256 hash")
		}
		if ef.File == FileAuth && ef.ExpectedExist != j.AuthRequired {
			return newError(KindTransactionInvalid, "auth expectedExist must match authRequired")
		}
	}
	return nil
}

// fileIDsEqual compares two file id lists in order: publishedFiles is recorded
// in publish order (catalog, auth, config), so order is part of the contract.
func fileIDsEqual(a, b []FileID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func subsetOfExpectedFiles(published []FileID, expected []ExpectedFile) bool {
	in := make(map[FileID]bool, len(expected))
	for _, ef := range expected {
		in[ef.File] = true
	}
	for _, id := range published {
		if !in[id] {
			return false
		}
	}
	return true
}
