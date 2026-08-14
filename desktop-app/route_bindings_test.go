package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/routingswitch"
	"moonbridge/internal/service/trafficanalysis"
)

func TestTransitionRecoveryDesktopRouteStatusIntegration(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"gpt-test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := recovery.NewStore(&recovery.Paths{RecoveryDir: filepath.Join(home, "recovery"), CodexHome: home}, filepath.Join(home, "recovery", "recovery-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	state := recovery.New()
	state.Phase = recovery.PhaseInactive
	state.TransitionID = "550e8400-e29b-41d4-a716-446655440000"
	state.RoutePhase = string(routingswitch.PhaseActivatingDeepSeek)
	state.DesiredRoute = string(routingswitch.DesiredRouteDeepSeek)
	state.RouteEvidence = string(routingswitch.RouteEvidenceNone)
	if err := store.Write(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	traffic := trafficanalysis.NewService()
	beforeTraffic := traffic.Status()
	app := NewApp(AppOptions{
		Service:      newScriptedController(gateway.State{Status: gateway.StatusStopped}),
		Traffic:      traffic,
		Recovery:     store,
		RecoveryHome: home,
		ConfigPath:   configPath,
		CodexConfig:  codexconfig.New(codexconfig.Options{Home: home}),
		EmitEvents:   noopEmit,
	})

	result := app.RouteStatus()
	if !result.OK || result.Value == nil {
		t.Fatalf("RouteStatus() = %#v, want safe success", result)
	}
	status := result.Value
	if status.Phase != routingswitch.PhaseActivatingDeepSeek || status.DesiredRoute != routingswitch.DesiredRouteDeepSeek {
		t.Fatalf("RouteStatus phase/desired = %#v/%#v", status.Phase, status.DesiredRoute)
	}
	if status.TransitionID != routingswitch.TransitionID(state.TransitionID) {
		t.Fatalf("RouteStatus transitionId = %q, want opaque epoch", status.TransitionID)
	}
	journalID := routingswitch.TransitionID(state.TransitionID)
	if err := routingswitch.ValidateTransition(status.TransitionID, journalID, journalID); err != nil {
		t.Fatalf("matching transition validation failed: %v", err)
	}
	var legacy recovery.State
	if err := json.Unmarshal([]byte(`{"schemaVersion":2,"phase":"inactive"}`), &legacy); err != nil {
		t.Fatalf("legacy recovery decode failed: %v", err)
	}
	if legacy.Phase != recovery.PhaseInactive || legacy.TransitionID != "" {
		t.Fatalf("legacy recovery decode = %+v, want missing transition fields accepted", legacy)
	}
	if !reflect.DeepEqual(beforeTraffic, app.traffic.Status()) {
		t.Fatal("RouteStatus mutated Traffic state")
	}
	afterState, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeState) != string(afterState) {
		t.Fatal("RouteStatus mutated Recovery state")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"config.toml", "https://", "backup", "Authorization", "api_key", "recovery-state", home} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("RouteStatus DTO contains forbidden sentinel %q: %s", forbidden, encoded)
		}
	}
	if err := routingswitch.ValidateTransition(status.TransitionID, routingswitch.TransitionID("550e8400-e29b-41d4-a716-446655440001"), journalID); err != routingswitch.ErrStaleTransition {
		t.Fatalf("stale transition error = %v, want %v", err, routingswitch.ErrStaleTransition)
	}
}
