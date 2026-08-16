package gatewayintegration

import (
	"errors"
	"testing"

	"moonbridge/internal/service/codexconfig"
)

func TestClassifyCurrentTarget(t *testing.T) {
	const gateway = "http://127.0.0.1:38440"
	const capture = "http://127.0.0.1:38441"
	tests := []struct {
		name     string
		snapshot codexconfig.RootURLSnapshot
		gateway  string
		want     CurrentTarget
	}{
		{"absent key is original", codexconfig.RootURLSnapshot{}, gateway, CurrentTargetOriginal},
		{"gateway url", codexconfig.RootURLSnapshot{Present: true, Value: gateway}, gateway, CurrentTargetGateway},
		{"analysis url", codexconfig.RootURLSnapshot{Present: true, Value: capture}, gateway, CurrentTargetAnalysis},
		{"user upstream is other", codexconfig.RootURLSnapshot{Present: true, Value: "https://api.example.com/v1"}, gateway, CurrentTargetOther},
		{"gateway url unknown classifies other", codexconfig.RootURLSnapshot{Present: true, Value: gateway}, "", CurrentTargetOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCurrentTarget(tt.snapshot, tt.gateway, capture); got != tt.want {
				t.Fatalf("classifyCurrentTarget(%#v, %q, %q) = %q, want %q", tt.snapshot, tt.gateway, capture, got, tt.want)
			}
		})
	}
}

func TestDiagnosticErrorPreservesCause(t *testing.T) {
	s := &Service{}
	err := s.enableError(stageGuardExistingTarget, CurrentTargetGateway, rollbackNone, ErrAlreadyIntegrated)

	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("errors.As(%v, *Error) = false, want true", err)
	}
	if ge.Operation != "enable" || ge.Stage != stageGuardExistingTarget || ge.CurrentTarget != CurrentTargetGateway || ge.Rollback != rollbackNone {
		t.Fatalf("diagnostic error = %#v, want enable/guard_existing_target/gateway/empty", ge)
	}
	if !errors.Is(err, ErrAlreadyIntegrated) {
		t.Fatalf("errors.Is(err, ErrAlreadyIntegrated) = false, want true (cause must be preserved)")
	}
	if got := err.Error(); got != stageGuardExistingTarget {
		t.Fatalf("Error() = %q, want stage only %q (no url/secret/cause text)", got, stageGuardExistingTarget)
	}
}

func TestDisableErrorHasNoRollback(t *testing.T) {
	s := &Service{}
	err := s.disableError(stageGuardRestoreTarget, CurrentTargetOther, ErrDisableConflict)

	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("errors.As(%v, *Error) = false, want true", err)
	}
	if ge.Operation != "disable" || ge.Stage != stageGuardRestoreTarget || ge.Rollback != "" {
		t.Fatalf("diagnostic error = %#v, want disable/guard_restore_target/empty", ge)
	}
	if !errors.Is(err, ErrDisableConflict) {
		t.Fatalf("errors.Is(err, ErrDisableConflict) = false, want true")
	}
}
