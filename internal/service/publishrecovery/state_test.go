package publishrecovery

import (
	"encoding/json"
	"strings"
	"testing"
)

const testTransactionID = "11111111-1111-4111-8111-111111111111"

func testFingerprint() string { return strings.Repeat("a", 64) }

func expectedAllFiles() []ExpectedFile {
	return []ExpectedFile{
		{File: FileModelsCatalog, ExpectedExist: true, SHA256: strings.Repeat("b", 64)},
		{File: FileAuth, ExpectedExist: true, SHA256: strings.Repeat("c", 64)},
		{File: FileConfig, ExpectedExist: true, SHA256: strings.Repeat("d", 64)},
	}
}

func ptr(p Phase) *Phase { return &p }

// journalFor returns a journal whose publishedFiles/commitMarkerPublished/
// completedAt/backoutManifest expectations are phase-consistent. The expectations
// are hardcoded here (independent of publishForForward) so the positive tests
// genuinely exercise the validator.
func journalFor(phase Phase, rollbackFrom *Phase) *Journal {
	j := &Journal{
		SchemaVersion:         SchemaVersion,
		TransactionID:         testTransactionID,
		Phase:                 phase,
		StartedAt:             "2026-08-06T00:00:00Z",
		UpdatedAt:             "2026-08-06T00:00:00Z",
		TargetHomeFingerprint: testFingerprint(),
		ExpectedFiles:         expectedAllFiles(),
		AuthRequired:          true,
		BackoutManifestSHA256: strings.Repeat("e", 64),
		RollbackFromPhase:     rollbackFrom,
		RollbackAttempted:     isRollbackPhase(phase),
	}
	branch := phase
	if rollbackFrom != nil {
		branch = *rollbackFrom
	}
	switch branch {
	case PhasePrepared, PhaseBackoutCopied:
		if phase == PhasePrepared {
			j.BackoutManifestSHA256 = "" // no backout before it is copied
		}
	case PhaseCatalogPublished:
		j.PublishedFiles = []FileID{FileModelsCatalog}
	case PhaseAuthPublished:
		j.PublishedFiles = []FileID{FileModelsCatalog, FileAuth}
	case PhaseConfigPublished, PhaseVerified, PhaseCompleted:
		j.PublishedFiles = []FileID{FileModelsCatalog, FileAuth, FileConfig}
		j.CommitMarkerPublished = true
		if phase == PhaseCompleted {
			at := "2026-08-06T00:00:01Z"
			j.CompletedAt = &at
		}
	}
	return j
}

func TestValidateValidJournals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase Phase
		roll  *Phase
	}{
		{"prepared", PhasePrepared, nil},
		{"backout_copied", PhaseBackoutCopied, nil},
		{"catalog_published", PhaseCatalogPublished, nil},
		{"auth_published", PhaseAuthPublished, nil},
		{"config_published", PhaseConfigPublished, nil},
		{"verified", PhaseVerified, nil},
		{"completed", PhaseCompleted, nil},
		{"rollback_required from prepared", PhaseRollbackRequired, ptr(PhasePrepared)},
		{"rollback_required from backout_copied", PhaseRollbackRequired, ptr(PhaseBackoutCopied)},
		{"rollback_required from catalog", PhaseRollbackRequired, ptr(PhaseCatalogPublished)},
		{"rollback_required from auth", PhaseRollbackRequired, ptr(PhaseAuthPublished)},
		{"rollback_required from config", PhaseRollbackRequired, ptr(PhaseConfigPublished)},
		{"rolled_back from config", PhaseRolledBack, ptr(PhaseConfigPublished)},
		{"rollback_failed from auth", PhaseRollbackFailed, ptr(PhaseAuthPublished)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := journalFor(tc.phase, tc.roll).Validate(); err != nil {
				t.Fatalf("expected valid journal: %v", err)
			}
		})
	}
}

func TestValidateAuthNotRequired(t *testing.T) {
	j := journalFor(PhasePrepared, nil)
	j.AuthRequired = false
	j.ExpectedFiles = []ExpectedFile{
		{File: FileModelsCatalog, ExpectedExist: true, SHA256: strings.Repeat("b", 64)},
		{File: FileAuth, ExpectedExist: false},
		{File: FileConfig, ExpectedExist: true, SHA256: strings.Repeat("d", 64)},
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("expected valid journal: %v", err)
	}
}

func TestValidateRejectsSchemaVersionNot1(t *testing.T) {
	j := journalFor(PhasePrepared, nil)
	j.SchemaVersion = 2
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestValidateRejectsUnknownPhase(t *testing.T) {
	j := journalFor(PhasePrepared, nil)
	j.Phase = Phase("bogus")
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestValidateRejectsBadTransactionID(t *testing.T) {
	for _, id := range []string{
		"",
		"11111111-1111-4111-8111-11111111111",   // 35 chars
		"11111111-1111-4111-8111-1111111111111", // 37 chars
		"zzzzzzzz-1111-4111-8111-111111111111",  // non-hex
		"11111111-1111-1111-8111-111111111111",  // version 1, not 4
		"11111111-1111-4111-7111-111111111111",  // variant 7, not 8-9-a-b
		"11111111-1111-4111-8111-AAAAAAAAAAAA",  // uppercase
		"../etc/passwd",
		"C:\\publish",
		"a/b",
		"..\\..\\journal",
	} {
		j := journalFor(PhasePrepared, nil)
		j.TransactionID = id
		if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("transactionID %q: expected transaction_invalid, got %v", id, err)
		}
	}
}

func TestValidateRejectsBadTargetHomeFingerprint(t *testing.T) {
	for _, fp := range []string{
		"",
		"abc",
		strings.Repeat("a", 63),
		strings.Repeat("g", 64),
		strings.Repeat("A", 64),
	} {
		j := journalFor(PhasePrepared, nil)
		j.TargetHomeFingerprint = fp
		if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("fingerprint %q: expected transaction_invalid, got %v", fp, err)
		}
	}
}

func TestValidateRollbackRequiresRollbackFromPhase(t *testing.T) {
	for _, phase := range []Phase{PhaseRollbackRequired, PhaseRolledBack, PhaseRollbackFailed} {
		j := journalFor(phase, nil)
		if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("phase %s: expected transaction_invalid, got %v", phase, err)
		}
	}
}

func TestValidateRollbackFromPhaseMustBeForwardBranch(t *testing.T) {
	for _, rp := range []Phase{PhaseVerified, PhaseCompleted, PhaseRollbackRequired, PhaseRolledBack, PhaseRollbackFailed} {
		j := journalFor(PhaseRollbackRequired, ptr(rp))
		if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("rollbackFrom %s: expected transaction_invalid, got %v", rp, err)
		}
	}
}

func TestValidateRollbackFromPhaseMismatchPublishedFiles(t *testing.T) {
	// rollbackFrom=catalog requires [models_catalog]; carry auth too.
	j := journalFor(PhaseRollbackRequired, ptr(PhaseCatalogPublished))
	j.PublishedFiles = []FileID{FileModelsCatalog, FileAuth}
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestValidateRollbackFromPhaseMismatchMarker(t *testing.T) {
	// rollbackFrom=config requires marker=true.
	j := journalFor(PhaseRollbackRequired, ptr(PhaseConfigPublished))
	j.CommitMarkerPublished = false
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestValidateForwardPhaseRejectsRollbackFromPhase(t *testing.T) {
	j := journalFor(PhaseCatalogPublished, ptr(PhasePrepared))
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestValidateForwardPhasePublishedFilesMismatch(t *testing.T) {
	j := journalFor(PhaseCatalogPublished, nil)
	j.PublishedFiles = []FileID{FileModelsCatalog, FileAuth}
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestValidateForwardPhaseMarkerMismatch(t *testing.T) {
	j := journalFor(PhaseConfigPublished, nil)
	j.CommitMarkerPublished = false
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestValidateCompletedAtStrict(t *testing.T) {
	// completed requires a non-empty completedAt.
	j := journalFor(PhaseCompleted, nil)
	j.CompletedAt = nil
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("nil completedAt: expected transaction_invalid, got %v", err)
	}
	empty := ""
	j.CompletedAt = &empty
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("empty completedAt: expected transaction_invalid, got %v", err)
	}
	// Non-completed phases must not carry a completedAt.
	at := "2026-08-06T00:00:01Z"
	for _, jj := range []*Journal{
		journalFor(PhasePrepared, nil),
		journalFor(PhaseVerified, nil),
		journalFor(PhaseRollbackRequired, ptr(PhaseConfigPublished)),
	} {
		jj.CompletedAt = &at
		if err := jj.Validate(); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("phase %s with completedAt: expected transaction_invalid, got %v", jj.Phase, err)
		}
	}
}

func TestValidateExpectedFiles(t *testing.T) {
	// config must be present.
	j := journalFor(PhasePrepared, nil)
	j.ExpectedFiles = []ExpectedFile{
		{File: FileModelsCatalog, ExpectedExist: true, SHA256: strings.Repeat("b", 64)},
	}
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("missing config: expected transaction_invalid, got %v", err)
	}

	// duplicates rejected.
	j = journalFor(PhasePrepared, nil)
	j.ExpectedFiles = append(j.ExpectedFiles, ExpectedFile{File: FileConfig, ExpectedExist: true, SHA256: strings.Repeat("d", 64)})
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("duplicate config: expected transaction_invalid, got %v", err)
	}

	// unknown enum rejected.
	j = journalFor(PhasePrepared, nil)
	j.ExpectedFiles = append(j.ExpectedFiles, ExpectedFile{File: FileID("backup"), ExpectedExist: true, SHA256: strings.Repeat("b", 64)})
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("unknown file: expected transaction_invalid, got %v", err)
	}

	// expected exist without hash rejected.
	j = journalFor(PhasePrepared, nil)
	j.ExpectedFiles[0].SHA256 = ""
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected exist without hash: expected transaction_invalid, got %v", err)
	}

	// expected absent with hash rejected.
	j = journalFor(PhasePrepared, nil)
	j.ExpectedFiles[0].ExpectedExist = false
	j.ExpectedFiles[0].SHA256 = strings.Repeat("b", 64)
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected absent with hash: expected transaction_invalid, got %v", err)
	}

	// auth expectedExist must match authRequired (both directions).
	j = journalFor(PhasePrepared, nil)
	j.ExpectedFiles[1].ExpectedExist = false
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("auth expectedExist=false with authRequired=true: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhasePrepared, nil)
	j.AuthRequired = false // expectedAllFiles still has auth ExpectedExist=true
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("authRequired=false with auth expectedExist=true: expected transaction_invalid, got %v", err)
	}
}

func TestValidateBackoutManifestHash(t *testing.T) {
	// prepared may carry an empty backout hash.
	if err := journalFor(PhasePrepared, nil).Validate(); err != nil {
		t.Fatalf("prepared with empty backout hash: %v", err)
	}
	// non-prepared requires a non-empty hash.
	j := journalFor(PhaseBackoutCopied, nil)
	j.BackoutManifestSHA256 = ""
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("empty backout hash after prepared: expected transaction_invalid, got %v", err)
	}
	// non-64-hex rejected.
	j = journalFor(PhaseBackoutCopied, nil)
	j.BackoutManifestSHA256 = "xyz"
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("bad backout hash: expected transaction_invalid, got %v", err)
	}
}

func TestValidatePublishedFilesSubsetOfExpectedFiles(t *testing.T) {
	// auth_published expects [models_catalog, auth] but expectedFiles omits auth.
	j := journalFor(PhaseAuthPublished, nil)
	j.ExpectedFiles = []ExpectedFile{
		{File: FileModelsCatalog, ExpectedExist: true, SHA256: strings.Repeat("b", 64)},
		{File: FileConfig, ExpectedExist: true, SHA256: strings.Repeat("d", 64)},
	}
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestJournalJSONFieldNames(t *testing.T) {
	j := journalFor(PhaseRollbackRequired, ptr(PhaseConfigPublished))
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"transactionId", "expectedFiles", "backoutManifestSha256", "rollbackFromPhase"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("journal JSON is missing key %q", key)
		}
	}
	for _, key := range []string{"transactionID", "expectedExist", "backoutManifestSHA256"} {
		if _, ok := m[key]; ok {
			t.Fatalf("journal JSON still carries the old key %q", key)
		}
	}
	var files []map[string]json.RawMessage
	if err := json.Unmarshal(m["expectedFiles"], &files); err != nil {
		t.Fatalf("unmarshal expectedFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 expectedFiles, got %d", len(files))
	}
	for i, f := range files {
		if _, ok := f["expectedExists"]; !ok {
			t.Fatalf("expectedFiles[%d] is missing key %q", i, "expectedExists")
		}
		if _, ok := f["expectedExist"]; ok {
			t.Fatalf("expectedFiles[%d] still carries the old key %q", i, "expectedExist")
		}
	}
}

func TestValidateTimeFields(t *testing.T) {
	// startedAt: required, RFC3339, UTC.
	j := journalFor(PhasePrepared, nil)
	j.StartedAt = ""
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("empty startedAt: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhasePrepared, nil)
	j.StartedAt = "yesterday"
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("non-RFC3339 startedAt: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhasePrepared, nil)
	j.StartedAt = "2026-08-06T00:00:00+02:00"
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("non-UTC startedAt: expected transaction_invalid, got %v", err)
	}

	// updatedAt: required, RFC3339, UTC, >= startedAt.
	j = journalFor(PhasePrepared, nil)
	j.UpdatedAt = ""
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("empty updatedAt: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhasePrepared, nil)
	j.UpdatedAt = "yesterday"
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("non-RFC3339 updatedAt: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhasePrepared, nil)
	j.UpdatedAt = "2026-08-06T00:00:00.000Z"
	if err := j.Validate(); err != nil {
		t.Fatalf("fractional-seconds RFC3339 updatedAt should be valid: %v", err)
	}
	j = journalFor(PhasePrepared, nil)
	j.UpdatedAt = "2026-08-05T23:59:59Z"
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("updatedAt before startedAt: expected transaction_invalid, got %v", err)
	}

	// completedAt: RFC3339, UTC, >= startedAt; equal is valid.
	j = journalFor(PhaseCompleted, nil)
	bad := "abc"
	j.CompletedAt = &bad
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("non-RFC3339 completedAt: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhaseCompleted, nil)
	nonUTC := "2026-08-06T00:00:01+02:00"
	j.CompletedAt = &nonUTC
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("non-UTC completedAt: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhaseCompleted, nil)
	beforeStart := "2026-08-05T23:59:59Z"
	j.CompletedAt = &beforeStart
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("completedAt before startedAt: expected transaction_invalid, got %v", err)
	}
	j = journalFor(PhaseCompleted, nil)
	equal := "2026-08-06T00:00:00Z"
	j.CompletedAt = &equal
	if err := j.Validate(); err != nil {
		t.Fatalf("completedAt equal to startedAt should be valid: %v", err)
	}
}

func TestValidateRollbackAttemptedPhaseConsistency(t *testing.T) {
	// A rollback phase must have RollbackAttempted=true.
	j := journalFor(PhaseRollbackRequired, ptr(PhaseConfigPublished))
	j.RollbackAttempted = false
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("rollback phase with rollbackAttempted=false: expected transaction_invalid, got %v", err)
	}
	// A forward phase must have RollbackAttempted=false.
	j = journalFor(PhaseConfigPublished, nil)
	j.RollbackAttempted = true
	if err := j.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("forward phase with rollbackAttempted=true: expected transaction_invalid, got %v", err)
	}
}
