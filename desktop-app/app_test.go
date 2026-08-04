package main

import "testing"

func TestRoundTripReturnsConcreteSuccessEnvelope(t *testing.T) {
	result := (&App{}).RoundTrip(RoundTripRequest{Payload: " hello "})
	if !result.OK {
		t.Fatalf("RoundTrip() ok = false, error = %#v", result.Error)
	}
	if result.Value == nil || result.Value.Payload != "hello" {
		t.Fatalf("RoundTrip() value = %#v, want trimmed payload", result.Value)
	}
	if result.Error != nil {
		t.Fatalf("RoundTrip() error = %#v, want nil", result.Error)
	}
}

func TestRoundTripPreservesStructuredError(t *testing.T) {
	result := (&App{}).RoundTrip(RoundTripRequest{})
	if result.OK {
		t.Fatal("RoundTrip() ok = true, want false")
	}
	if result.Value != nil {
		t.Fatalf("RoundTrip() value = %#v, want nil", result.Value)
	}
	if result.Error == nil || result.Error.Code != "invalid_payload" {
		t.Fatalf("RoundTrip() error = %#v, want invalid_payload", result.Error)
	}
}

func TestCommandErrorCarriesFullContract(t *testing.T) {
	err := (&App{}).RoundTrip(RoundTripRequest{}).Error
	if err == nil {
		t.Fatal("RoundTrip() error = nil, want non-nil")
	}
	if err.Operation != "RoundTrip" {
		t.Errorf("error.operation = %q, want RoundTrip", err.Operation)
	}
	if err.Stage != "validation" {
		t.Errorf("error.stage = %q, want validation", err.Stage)
	}
	if err.Code != "invalid_payload" {
		t.Errorf("error.code = %q, want invalid_payload", err.Code)
	}
	if err.Message == "" {
		t.Error("error.message = empty, want non-empty")
	}
	if err.Field != nil {
		t.Errorf("error.field = %v, want nil", *err.Field)
	}
	if err.Retryable {
		t.Error("error.retryable = true, want false")
	}
	if err.MutationStarted {
		t.Error("error.mutationStarted = true, want false")
	}
	if err.GatewayLeftRunning {
		t.Error("error.gatewayLeftRunning = true, want false")
	}
	if err.GatewaySnapshot != nil {
		t.Errorf("error.gatewaySnapshot = %#v, want nil", err.GatewaySnapshot)
	}
	if err.Details != nil {
		t.Errorf("error.details = %#v, want nil", err.Details)
	}
}
