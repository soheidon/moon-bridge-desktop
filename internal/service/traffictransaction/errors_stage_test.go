package traffictransaction

import (
	"errors"
	"testing"

	"moonbridge/internal/service/trafficanalysis"
)

func TestCaptureStartFailureStage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain", errors.New("boom"), ""},
		{"already active", &trafficanalysis.Error{Kind: trafficanalysis.KindCaptureAlreadyActive}, "traffic_capture_already_active"},
		{"start failed with bind stage", &trafficanalysis.Error{Kind: trafficanalysis.KindCaptureStartFailed, Stage: "bind"}, "bind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := captureStartFailureStage(tc.err); got != tc.want {
				t.Fatalf("captureStartFailureStage(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
