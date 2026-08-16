package gatewayintegration

import (
	"context"
	"errors"
	"testing"

	"moonbridge/internal/service/codexconfig"
)

// fakeConfigEditor is a minimal ConfigEditor for the no-op diagnostic test. It
// returns a fixed snapshot and records whether a change was attempted.
type fakeConfigEditor struct {
	snapshot     codexconfig.RootURLSnapshot
	prepareCalls int
	commitCalls  int
	lastDesired  *string // root URL passed to PrepareRootURLChange (nil = delete key)
}

func (f *fakeConfigEditor) ReadRootURL(context.Context) (codexconfig.RootURLSnapshot, error) {
	return f.snapshot, nil
}

func (f *fakeConfigEditor) PrepareRootURLChange(_ context.Context, desired *string, _ string) (*codexconfig.PreparedRootURLChange, error) {
	f.prepareCalls++
	f.lastDesired = desired
	return &codexconfig.PreparedRootURLChange{}, nil
}

func (f *fakeConfigEditor) CommitPreparedRootURLChange(context.Context, *codexconfig.PreparedRootURLChange) error {
	f.commitCalls++
	return nil
}

// fakeRecoveryWriter is a minimal RecoveryWriter that returns a fixed checkpoint.
type fakeRecoveryWriter struct {
	cp              *Checkpoint
	checkpointCalls int
	lastCheckpoint  Checkpoint
}

func (f *fakeRecoveryWriter) Current(context.Context) (*Checkpoint, error) {
	return f.cp, nil
}

func (f *fakeRecoveryWriter) Checkpoint(_ context.Context, cp Checkpoint) error {
	f.checkpointCalls++
	f.lastCheckpoint = cp
	return nil
}

// TestDisableWithReportNoOpOnOrphanedGatewayConfig pins the orphan signature:
// recovery is already original (no recorded upstream) but the config still
// points at the gateway URL. Enable reports already-integrated, and the later
// Disable must not restore — but its report must surface the mismatch
// (before=gateway) so the binding layer can log it.
func TestDisableWithReportNoOpOnOrphanedGatewayConfig(t *testing.T) {
	const gatewayURL = "http://127.0.0.1:38440"
	cfg := &fakeConfigEditor{snapshot: codexconfig.RootURLSnapshot{Present: true, Value: gatewayURL}}
	rec := &fakeRecoveryWriter{cp: &Checkpoint{Target: TargetOriginal, OriginalPresent: false}}
	s := New(cfg, rec, "http://127.0.0.1:38441")

	if err := s.Enable(context.Background(), gatewayURL); !errors.Is(err, ErrAlreadyIntegrated) {
		t.Fatalf("Enable() error = %v, want ErrAlreadyIntegrated", err)
	}

	report, err := s.DisableWithReport(context.Background())
	if err != nil {
		t.Fatalf("DisableWithReport() error = %v, want nil", err)
	}
	if report.RecoveryTarget != TargetOriginal {
		t.Fatalf("report.RecoveryTarget = %q, want original", report.RecoveryTarget)
	}
	if report.OriginalPresent {
		t.Fatal("report.OriginalPresent = true, want false")
	}
	if report.Before != CurrentTargetGateway || report.After != CurrentTargetGateway {
		t.Fatalf("report before/after = %q/%q, want gateway/gateway (orphan signature)", report.Before, report.After)
	}
	if report.Restored {
		t.Fatal("report.Restored = true, want false (no-op must not restore)")
	}
	if cfg.prepareCalls != 0 || cfg.commitCalls != 0 || rec.checkpointCalls != 0 {
		t.Fatalf("no-op mutated config/recovery: prepare=%d commit=%d checkpoint=%d",
			cfg.prepareCalls, cfg.commitCalls, rec.checkpointCalls)
	}
}

// TestDisableWithReportDeletesKeyWhenNoOriginalRecorded pins the S1→S0 gateway
// Disable: when the Gateway layer never recorded a true original upstream
// (OriginalPresent=false), restoring S1 means deleting the openai_base_url key
// entirely (PrepareRootURLChange with a nil desired) and clearing the recovery
// record back to original/inactive. This is the transition that follows the
// fixed S2→S1 demote, so it must never restore a stale :38440 value.
func TestDisableWithReportDeletesKeyWhenNoOriginalRecorded(t *testing.T) {
	const gatewayURL = "http://127.0.0.1:38440"
	cfg := &fakeConfigEditor{snapshot: codexconfig.RootURLSnapshot{Present: true, Value: gatewayURL}}
	rec := &fakeRecoveryWriter{cp: &Checkpoint{
		Target:          TargetGateway,
		OriginalPresent: false,
		AppliedValue:    gatewayURL,
		Active:          true,
	}}
	s := New(cfg, rec, "http://127.0.0.1:38441")

	report, err := s.DisableWithReport(context.Background())
	if err != nil {
		t.Fatalf("DisableWithReport() error = %v, want nil", err)
	}
	if report.RecoveryTarget != TargetGateway {
		t.Fatalf("report.RecoveryTarget = %q, want gateway", report.RecoveryTarget)
	}
	if report.Before != CurrentTargetGateway {
		t.Fatalf("report.Before = %q, want gateway", report.Before)
	}
	if !report.Restored || report.After != CurrentTargetOriginal {
		t.Fatalf("report.Restored/After = %v/%q, want true/original (S1→S0 deletes key)", report.Restored, report.After)
	}
	if cfg.prepareCalls != 1 || cfg.commitCalls != 1 {
		t.Fatalf("prepare/commit = %d/%d, want 1/1", cfg.prepareCalls, cfg.commitCalls)
	}
	if cfg.lastDesired != nil {
		t.Fatalf("desired = %q, want nil (delete openai_base_url)", *cfg.lastDesired)
	}
	if rec.checkpointCalls != 1 {
		t.Fatalf("checkpoint calls = %d, want 1", rec.checkpointCalls)
	}
	if rec.lastCheckpoint.Target != TargetOriginal || rec.lastCheckpoint.OriginalPresent {
		t.Fatalf("final checkpoint = target %q originalPresent %v, want original/false",
			rec.lastCheckpoint.Target, rec.lastCheckpoint.OriginalPresent)
	}
}
