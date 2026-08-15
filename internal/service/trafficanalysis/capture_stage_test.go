package trafficanalysis

import (
	"errors"
	"net"
	"testing"
)

func TestCaptureStartStage(t *testing.T) {
	if got := captureStartStage(startFailure("bind", errors.New("boom"))); got != "bind" {
		t.Fatalf("captureStartStage(bind) = %q, want bind", got)
	}
	if got := captureStartStage(errors.New("plain")); got != "" {
		t.Fatalf("captureStartStage(plain) = %q, want empty", got)
	}
	f := &captureStartFailure{stage: "bind", err: errors.New("secret-addr-127.0.0.1:38441")}
	if got := f.Error(); got != "bind" {
		t.Fatalf("captureStartFailure.Error() = %q, want bind (raw error must not leak)", got)
	}
	if !errors.Is(startFailure("bind", errStageSentinel), errStageSentinel) {
		t.Fatal("errors.Is(startFailure(...)) = false, want true")
	}
}

var errStageSentinel = errors.New("stage sentinel")

func TestStartCaptureBindFailureSetsStage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().String()

	svc := NewService()
	svc.BindGatewayRun("gateway-x", "127.0.0.1:38440")
	_, err = svc.StartCapture(StartOptions{ListenAddr: taken})
	mustErrKind(t, err, KindCaptureStartFailed)
	if se := err.(*Error); se.Stage != "bind" {
		t.Fatalf("Stage after bind failure = %q, want bind", se.Stage)
	}
}
