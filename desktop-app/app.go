package main

import (
	"context"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const roundTripEvent = "desktop:roundtrip"

type CommandError struct {
	Operation          string         `json:"operation"`
	Stage              string         `json:"stage"`
	Code               string         `json:"code"`
	Message            string         `json:"message"`
	Field              *string        `json:"field"`
	Retryable          bool           `json:"retryable"`
	MutationStarted    bool           `json:"mutationStarted"`
	GatewayLeftRunning bool           `json:"gatewayLeftRunning"`
	GatewaySnapshot    any            `json:"gatewaySnapshot"`
	Details            map[string]any `json:"details,omitempty"`
}

type RoundTripRequest struct {
	Payload string `json:"payload"`
}

type RoundTripValue struct {
	Payload string `json:"payload"`
}

type RoundTripResult struct {
	OK    bool            `json:"ok"`
	Value *RoundTripValue `json:"value,omitempty"`
	Error *CommandError   `json:"error,omitempty"`
}

type App struct {
	ctx context.Context
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) RoundTrip(req RoundTripRequest) RoundTripResult {
	payload := strings.TrimSpace(req.Payload)
	if payload == "" {
		return RoundTripResult{
			OK: false,
			Error: &CommandError{
				Operation: "RoundTrip",
				Stage:     "validation",
				Code:      "invalid_payload",
				Message:   "payload must not be empty",
			},
		}
	}

	value := &RoundTripValue{Payload: payload}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, roundTripEvent, value)
	}
	return RoundTripResult{OK: true, Value: value}
}

func (a *App) roundTripEventName() string {
	return roundTripEvent
}
