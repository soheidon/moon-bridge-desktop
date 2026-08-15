package egressobservation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderEgressEventsAreSafeAndOrdered(t *testing.T) {
	var events []Event
	client := WrapClient(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})})
	ctx := WithMetadata(context.Background(), Event{Correlation: "raw-request-secret", ProfileID: "raw-profile-secret", Provider: "deepseek", Model: "deepseek-v4-flash"}, func(event Event) {
		events = append(events, event)
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://provider.invalid/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	MarkForwarded(ctx)
	if len(events) != 3 || events[0].Kind != RequestDispatched || events[1].Kind != ResponseReceived || events[2].Kind != ResponseForwarded {
		t.Fatalf("events = %#v", events)
	}
	if events[1].StatusCode != http.StatusOK || events[2].ExchangeIndex != 1 {
		t.Fatalf("response metadata = %#v", events)
	}
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "raw-request-secret") || strings.Contains(string(encoded), "raw-profile-secret") {
			t.Fatalf("serialized event leaked process-local identifiers: %s", encoded)
		}
	}
}

func TestProviderEgressForwardedRequiresSuccessfulResponse(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantEvents int
	}{
		{name: "success", status: http.StatusOK, wantEvents: 3},
		{name: "client error", status: http.StatusNotFound, wantEvents: 2},
		{name: "server error", status: http.StatusBadGateway, wantEvents: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []Event
			client := WrapClient(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader("response")), Header: make(http.Header)}, nil
			})})
			ctx := WithMetadata(context.Background(), Event{Provider: "deepseek", UpstreamModel: "deepseek-v4-flash"}, func(event Event) {
				events = append(events, event)
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://provider.invalid/v1", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			MarkForwarded(ctx)
			if len(events) != tt.wantEvents {
				t.Fatalf("events = %#v, want %d events", events, tt.wantEvents)
			}
			if tt.wantEvents == 3 && events[2].Kind != ResponseForwarded {
				t.Fatalf("last event = %#v, want forwarded", events[2])
			}
		})
	}
}
