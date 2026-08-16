package traffictransaction

import "testing"

// TestCheckpointForDisableDemoteAlwaysTargetsGateway pins the demote contract: an
// S2→S1 demote (Traffic Analysis stop) switches the front door back to the
// gateway backend, so the recovery record must always demote to TargetGateway.
// OriginalPresent only decides whether the later S1→S0 gateway Disable deletes
// the key or restores a recorded value — it must never flip the demote target
// back to original. The Codex config stays at the stable front door URL (:38440)
// throughout, so AppliedValue is always frontDoorURL.
func TestCheckpointForDisableDemoteAlwaysTargetsGateway(t *testing.T) {
	for _, originalPresent := range []bool{false, true} {
		source := Checkpoint{
			OperationID:       "op",
			IntegrationTarget: TargetAnalysis,
			OriginalPresent:   originalPresent,
			BackupID:          "bk",
			GatewayInstance:   "gw",
			GatewayAddress:    "127.0.0.1:38440",
		}

		got := checkpointForDisableDemote("op", source, 7)

		if got.IntegrationTarget != TargetGateway {
			t.Fatalf("OriginalPresent=%v: IntegrationTarget = %q, want gateway",
				originalPresent, got.IntegrationTarget)
		}
		if got.OriginalPresent != originalPresent {
			t.Fatalf("OriginalPresent=%v: OriginalPresent not preserved (got %v)",
				originalPresent, got.OriginalPresent)
		}
		if got.AppliedValue != frontDoorURL {
			t.Fatalf("OriginalPresent=%v: AppliedValue = %q, want %q (config stays at the front door)",
				originalPresent, got.AppliedValue, frontDoorURL)
		}
		if got.DurablePhase != DurableInactive || got.Phase != PhaseDisableCompleted {
			t.Fatalf("OriginalPresent=%v: phase = %q/%q, want inactive/disable_completed",
				originalPresent, got.DurablePhase, got.Phase)
		}
		if got.IntegrationActive {
			t.Fatalf("OriginalPresent=%v: IntegrationActive = true, want false", originalPresent)
		}
	}
}
