package recovery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewStampsSchemaVersion(t *testing.T) {
	s := New()
	if s.SchemaVersion != SchemaVersion {
		t.Fatalf("New().SchemaVersion = %d, want %d", s.SchemaVersion, SchemaVersion)
	}
	if s.Phase != "" {
		t.Fatalf("New().Phase = %q, want empty", s.Phase)
	}
	if s.IntegrationActive {
		t.Fatalf("New().IntegrationActive = true, want false")
	}
}

func TestWithUpdatedAt(t *testing.T) {
	base := New()
	base.SchemaVersion = 1 // WithUpdatedAt must NOT change the schema version
	now := time.Date(2026, 8, 5, 12, 30, 0, 0, time.UTC)
	out := base.WithUpdatedAt(now)
	if out.SchemaVersion != 1 {
		t.Fatalf("WithUpdatedAt changed SchemaVersion to %d, want it untouched", out.SchemaVersion)
	}
	if out.UpdatedAt == nil {
		t.Fatal("WithUpdatedAt.UpdatedAt is nil")
	}
	if *out.UpdatedAt != "2026-08-05T12:30:00Z" {
		t.Fatalf("UpdatedAt = %q, want rfc3339 2026-08-05T12:30:00Z", *out.UpdatedAt)
	}
}

func TestWithUpdatedAtZeroNow(t *testing.T) {
	out := New().WithUpdatedAt(time.Time{})
	if out.UpdatedAt == nil {
		t.Fatal("UpdatedAt is nil with zero now")
	}
	ts, err := time.Parse(time.RFC3339, *out.UpdatedAt)
	if err != nil {
		t.Fatalf("UpdatedAt is not rfc3339: %v", err)
	}
	if ts.IsZero() {
		t.Fatal("UpdatedAt is zero")
	}
}

// TestStateJSONCamelCaseAndNoSecrets asserts the JSON keys match the Rust
// RecoveryState contract and that a naturally-populated state never leaks
// secret-looking values into the encoded payload.
func TestStateJSONCamelCaseAndNoSecrets(t *testing.T) {
	prev := "https://api.openai.com/v1"
	backup := "backups/codex-config/blah"
	st := New()
	st.IntegrationActive = true
	st.Phase = PhaseIntegrationApplied
	st.OperationID = "op-1"
	st.TransitionID = "550e8400-e29b-41d4-a716-446655440000"
	st.RoutePhase = "activating_deepseek"
	st.DesiredRoute = "deepseek"
	st.RouteEvidence = "none"
	st.ConfigPath = "config.toml"
	st.CodexHomeFingerprint = strings.Repeat("a", 64)
	st.PreviousOpenaiBaseURLPresent = true
	st.PreviousOpenaiBaseURL = &prev
	st.AppliedOpenaiBaseURL = "http://127.0.0.1:38441/"
	st.ConfigHashBeforeApply = "aaa"
	st.ConfigHashAfterApply = "bbb"
	st.BackupPath = &backup
	st.StartedAt = "2026-08-05T12:30:00Z"
	st.AutoLogStatus = StringPtr("running")
	st.UnsavedObservationsMayRemain = true
	st.UnsavedDiscardConfirmed = false
	st.ReconciliationStatus = StringPtr("pending_restore")
	st.ReconciledAt = StringPtr("2026-08-05T12:31:00Z")
	st.ReconciliationDetail = StringPtr("ok")
	st.RestartAttempted = false
	st = st.WithUpdatedAt(time.Date(2026, 8, 5, 12, 32, 0, 0, time.UTC))

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)

	for _, key := range []string{
		"schemaVersion", "integrationActive", "phase", "operationId", "transitionId", "routePhase", "desiredRoute", "routeEvidence", "configPath",
		"codexHomeFingerprint",
		"previousOpenaiBaseUrlPresent", "previousOpenaiBaseUrl", "appliedOpenaiBaseUrl",
		"configHashBeforeApply", "configHashAfterApply", "backupPath", "startedAt",
		"updatedAt", "unsavedObservationsMayRemain", "unsavedDiscardConfirmed",
		"reconciliationStatus", "reconciledAt", "reconciliationDetail", "restartAttempted",
	} {
		if !strings.Contains(raw, "\""+key+"\"") {
			t.Errorf("JSON missing expected camelCase key %q\n%s", key, raw)
		}
	}
	// No secret-ish JSON keys must appear in the encoded state.
	for _, key := range []string{"apiKey", "api_key", "secret", "authorization", "bearer", "servertoken", "controltoken"} {
		if strings.Contains(strings.ToLower(raw), key) {
			t.Errorf("JSON leaked a secret-ish key %q\n%s", key, raw)
		}
	}
	// Secret values never appear in the payload of a normally-populated state.
	for _, v := range []string{"sk-", "Bearer ", "authorization:", "__SECRET"} {
		if strings.Contains(raw, v) {
			t.Errorf("JSON contains a secret-looking value %q\n%s", v, raw)
		}
	}
}

func TestStringPtrHelper(t *testing.T) {
	v := StringPtr("x")
	if v == nil || *v != "x" {
		t.Fatalf("StringPtr returned %v", v)
	}
}

// TestStateUnmarshalDefaultsPhaseWhenMissing mirrors the Rust serde default
// (default_recovery_phase = integration_applied): a missing phase key defaults on
// deserialization; an explicit value is preserved.
func TestStateUnmarshalDefaultsPhaseWhenMissing(t *testing.T) {
	st := New()
	if err := json.Unmarshal([]byte(`{"schemaVersion":2,"integrationActive":true}`), st); err != nil {
		t.Fatalf("unmarshal without phase: %v", err)
	}
	if st.Phase != PhaseIntegrationApplied {
		t.Fatalf("phase = %q, want integration_applied (Rust serde default)", st.Phase)
	}
	if err := json.Unmarshal([]byte(`{"schemaVersion":2,"phase":"prepared"}`), st); err != nil {
		t.Fatalf("unmarshal with explicit phase: %v", err)
	}
	if st.Phase != PhasePrepared {
		t.Fatalf("phase = %q, want prepared", st.Phase)
	}
}

// TestStateUnmarshalKeepsExplicitPhase: an explicit phase value (even an explicit
// empty string) is kept verbatim; only an absent key takes the default. A null
// phase is rejected (Rust String-from-null fails).
func TestStateUnmarshalKeepsExplicitPhase(t *testing.T) {
	st := New()
	if err := json.Unmarshal([]byte(`{"schemaVersion":2,"phase":""}`), st); err != nil {
		t.Fatalf("unmarshal explicit empty phase: %v", err)
	}
	if st.Phase != "" {
		t.Fatalf("explicit empty phase must be kept verbatim, got %q", st.Phase)
	}
}

// TestStateUnmarshalRejectsNullPhase: a null phase is a deserialize error in Rust
// (String), and the Go port mirrors that. A missing key still defaults to
// integration_applied; an explicit empty value is kept verbatim.
func TestStateUnmarshalRejectsNullPhase(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"schemaVersion":2,"phase":null}`), &st); err == nil {
		t.Fatal("null phase must be rejected (Rust String-from-null fails)")
	}
	st2 := New()
	if err := json.Unmarshal([]byte(`{"schemaVersion":2}`), st2); err != nil {
		t.Fatalf("missing phase must default: %v", err)
	}
	if st2.Phase != PhaseIntegrationApplied {
		t.Fatalf("missing phase default = %q, want integration_applied", st2.Phase)
	}
	st3 := New()
	if err := json.Unmarshal([]byte(`{"schemaVersion":2,"phase":""}`), st3); err != nil {
		t.Fatalf("explicit empty phase: %v", err)
	}
	if st3.Phase != "" {
		t.Fatalf("explicit empty must be kept, got %q", st3.Phase)
	}
}
