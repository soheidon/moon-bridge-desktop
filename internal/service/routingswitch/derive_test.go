package routingswitch

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRouteStatusDerivationContract(t *testing.T) {
	tests := []struct {
		name  string
		input Inputs
		want  Phase
	}{
		{name: "clean native", input: Inputs{DesiredRoute: DesiredRouteNative, ConfigRoute: ConfigRouteOriginal, GatewayState: GatewayStopped, RelayState: RelayStopped}, want: PhaseNative},
		{name: "capture without evidence", input: Inputs{DesiredRoute: DesiredRouteDeepSeek, ConfigRoute: ConfigRouteCapture, GatewayState: GatewayRunning, RelayState: RelayCapturing, IntegrationActive: true}, want: PhaseDeepSeekRestartRequired},
		{name: "correlated evidence", input: Inputs{DesiredRoute: DesiredRouteDeepSeek, ConfigRoute: ConfigRouteCapture, GatewayState: GatewayRunning, RelayState: RelayCapturing, RecordingActive: true, RouteEvidence: RouteEvidenceDeepSeekObserved}, want: PhaseDeepSeekActive},
		{name: "native restart required", input: Inputs{DesiredRoute: DesiredRouteNative, ConfigRoute: ConfigRouteOriginal, GatewayState: GatewayRunning, RelayState: RelayPassthrough}, want: PhaseNativeRestartRequired},
		{name: "recovery wins", input: Inputs{DesiredRoute: DesiredRouteDeepSeek, ConfigRoute: ConfigRouteCapture, RecoveryRequired: true}, want: PhaseRecoveryRequired},
		{name: "contradiction", input: Inputs{DesiredRoute: DesiredRouteNative, ConfigRoute: ConfigRouteUnknown, Contradiction: true}, want: PhaseRecoveryRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Derive(tt.input)
			if got.Phase != tt.want {
				t.Fatalf("Derive().Phase = %q, want %q", got.Phase, tt.want)
			}
		})
	}

	encoded, err := json.Marshal(Derive(Inputs{
		DesiredRoute: DesiredRouteDeepSeek, ConfigRoute: ConfigRouteCapture,
		GatewayState: GatewayRunning, RelayState: RelayCapturing,
		RouteEvidence: RouteEvidenceDeepSeekObserved,
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"https://", "backup", "config.toml", "api_key", "Authorization", "sid", "username", "recovery-state"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("RouteStatus JSON contains forbidden sentinel %q: %s", forbidden, encoded)
		}
	}
}
