package traffictransaction

import "testing"

// TestCheckpointForDisableDemoteAlwaysTargetsGateway pins the Plan 9-21 fix: an
// S2→S1 demote (Traffic Analysis stop) restores Codex to the gateway URL, so the
// recovery record must always demote to TargetGateway. OriginalPresent only
// decides whether the later S1→S0 gateway Disable deletes the key or restores a
// recorded value — it must never flip the demote target back to original, which
// would orphan the :38440 config (config=gateway / recovery=original).
func TestCheckpointForDisableDemoteAlwaysTargetsGateway(t *testing.T) {
	for _, originalPresent := range []bool{false, true} {
		source := Checkpoint{
			OperationID:      "op",
			IntegrationTarget: TargetAnalysis,
			PreviousValue:    "http://127.0.0.1:38440",
			OriginalPresent:  originalPresent,
			BackupID:         "bk",
			GatewayInstance:  "gw",
			GatewayAddress:   "127.0.0.1:38440",
		}

		got := checkpointForDisableDemote("op", source, 7, "hash")

		if got.IntegrationTarget != TargetGateway {
			t.Fatalf("OriginalPresent=%v: IntegrationTarget = %q, want gateway",
				originalPresent, got.IntegrationTarget)
		}
		if got.OriginalPresent != originalPresent {
			t.Fatalf("OriginalPresent=%v: OriginalPresent not preserved (got %v)",
				originalPresent, got.OriginalPresent)
		}
		if got.AppliedValue != source.PreviousValue {
			t.Fatalf("OriginalPresent=%v: AppliedValue = %q, want %q (config must be gateway URL)",
				originalPresent, got.AppliedValue, source.PreviousValue)
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
