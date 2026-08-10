package main

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// capturedEvent is one emitted event (any name). The App also emits
// gateway-status during construction/shutdown, so assertions filter by name.
type capturedEvent struct {
	name    string
	payload any
}

// captureEmit records every emitted event for assertions.
func captureEmit(t *testing.T) (func(string, any), func() []capturedEvent) {
	var mu sync.Mutex
	var events []capturedEvent
	return func(name string, payload any) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, capturedEvent{name: name, payload: payload})
		}, func() []capturedEvent {
			mu.Lock()
			defer mu.Unlock()
			out := make([]capturedEvent, len(events))
			copy(out, events)
			return out
		}
}

// gatewayLogs filters captured events down to "gateway-log" DTOs.
func gatewayLogs(t *testing.T, events []capturedEvent) []GatewayLogDTO {
	var out []GatewayLogDTO
	for _, e := range events {
		if e.name != gatewayLogEvent {
			continue
		}
		dto, ok := e.payload.(GatewayLogDTO)
		if !ok {
			t.Fatalf("gateway-log payload type = %T, want GatewayLogDTO", e.payload)
		}
		out = append(out, dto)
	}
	return out
}

// millisRFC3339 matches the emitted timestamp format: RFC3339 with local offset
// and millisecond precision (e.g. 2026-08-07T06:13:54.426+09:00).
var millisRFC3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}[+-]\d{2}:\d{2}$`)

func TestGatewayLogBridgeEmitsPerLine(t *testing.T) {
	emit, events := captureEmit(t)
	b := newGatewayLogBridge(emit)

	input := "Moon Bridge 监听于 127.0.0.1:38440\nWeb Console: http://127.0.0.1:38440/console/\n"
	if n, err := b.Write([]byte(input)); err != nil || n != len(input) {
		t.Fatalf("Write() = (%d, %v), want (%d, nil)", n, err, len(input))
	}

	got := gatewayLogs(t, events())
	if len(got) != 2 {
		t.Fatalf("emitted %d events, want 2", len(got))
	}
	if got[0].Stream != gatewayLogStream {
		t.Errorf("line 0 stream = %q, want %q", got[0].Stream, gatewayLogStream)
	}
	if got[0].Line != "Moon Bridge 监听于 127.0.0.1:38440" {
		t.Errorf("line 0 = %q, want listening line", got[0].Line)
	}
	if got[1].Line != "Web Console: http://127.0.0.1:38440/console/" {
		t.Errorf("line 1 = %q, want console line", got[1].Line)
	}
	for i, dto := range got {
		if !millisRFC3339.MatchString(dto.Timestamp) {
			t.Errorf("line %d timestamp = %q, want RFC3339 with millis", i, dto.Timestamp)
		}
	}
}

func TestGatewayLogBridgeBuffersPartialLine(t *testing.T) {
	emit, events := captureEmit(t)
	b := newGatewayLogBridge(emit)

	if _, err := b.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write(hello ) error = %v", err)
	}
	if got := gatewayLogs(t, events()); len(got) != 0 {
		t.Fatalf("emitted %d events before newline, want 0", len(got))
	}
	if _, err := b.Write([]byte("world\n")); err != nil {
		t.Fatalf("Write(world) error = %v", err)
	}

	got := gatewayLogs(t, events())
	if len(got) != 1 || got[0].Line != "hello world" {
		t.Fatalf("events = %#v, want single \"hello world\"", got)
	}
}

func TestGatewayLogBridgeStripsANSI(t *testing.T) {
	emit, events := captureEmit(t)
	b := newGatewayLogBridge(emit)

	if _, err := b.Write([]byte("\x1b[32mgreen\x1b[0m\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got := gatewayLogs(t, events())
	if len(got) != 1 || got[0].Line != "green" {
		t.Fatalf("line = %q, want \"green\"", got[0].Line)
	}
}

func TestGatewayLogBridgeStripsCRLF(t *testing.T) {
	emit, events := captureEmit(t)
	b := newGatewayLogBridge(emit)

	if _, err := b.Write([]byte("line\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got := gatewayLogs(t, events())
	if len(got) != 1 || got[0].Line != "line" {
		t.Fatalf("line = %q, want \"line\"", got[0].Line)
	}
}

func TestGatewayLogBridgeRedactsAuthorizationToEndOfLine(t *testing.T) {
	emit, events := captureEmit(t)
	b := newGatewayLogBridge(emit)

	if _, err := b.Write([]byte("Authorization: Bearer abcdefghijklmnopqrstuvwxyz\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got := gatewayLogs(t, events())
	if len(got) != 1 {
		t.Fatalf("emitted %d events, want 1", len(got))
	}
	if strings.Contains(got[0].Line, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("line leaked bearer token: %q", got[0].Line)
	}
	if !strings.Contains(got[0].Line, "[REDACTED]") {
		t.Fatalf("line not redacted: %q", got[0].Line)
	}
}

func TestGatewayLogBridgeRedactsSkToken(t *testing.T) {
	emit, events := captureEmit(t)
	b := newGatewayLogBridge(emit)

	if _, err := b.Write([]byte("api key sk-abcdefghijklmnop set\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got := gatewayLogs(t, events())
	if len(got) != 1 {
		t.Fatalf("emitted %d events, want 1", len(got))
	}
	if strings.Contains(got[0].Line, "sk-abcdefghijklmnop") {
		t.Fatalf("line leaked sk token: %q", got[0].Line)
	}
	if !strings.Contains(got[0].Line, "sk-[REDACTED]") {
		t.Fatalf("line not redacted: %q", got[0].Line)
	}
}

func TestGatewayLogBridgeWriteNeverFailsWhenEmitPanics(t *testing.T) {
	b := newGatewayLogBridge(func(string, any) { panic("boom") })

	if n, err := b.Write([]byte("line\n")); err != nil || n != 5 {
		t.Fatalf("Write() = (%d, %v), want (5, nil)", n, err)
	}
}

func TestGatewayLogBridgeWriteSucceedsWithNilEmit(t *testing.T) {
	b := newGatewayLogBridge(nil)

	if n, err := b.Write([]byte("line\n")); err != nil || n != 5 {
		t.Fatalf("Write() = (%d, %v), want (5, nil)", n, err)
	}
}

func TestNewAppWiresGatewayLogBridgeToEmitEvents(t *testing.T) {
	emit, events := captureEmit(t)
	app := NewApp(AppOptions{EmitEvents: emit})
	defer app.shutdown(context.Background())

	if app.gatewayLogs == nil {
		t.Fatal("NewApp() gatewayLogs = nil, want bridge")
	}
	if _, err := app.gatewayLogs.Write([]byte("Moon Bridge 监听于 127.0.0.1:38440\n")); err != nil {
		t.Fatalf("bridge Write() error = %v", err)
	}

	got := gatewayLogs(t, events())
	if len(got) != 1 || got[0].Line != "Moon Bridge 监听于 127.0.0.1:38440" {
		t.Fatalf("events = %#v, want single listening line", got)
	}
}

func TestSafeEmitDoesNotPanicWhenCtxUnset(t *testing.T) {
	// a.ctx stays nil until startup; the default emitEvents must drop the event
	// instead of panicking (frontend-not-connected must not fail gateway runs).
	app := NewApp(AppOptions{})
	defer app.shutdown(context.Background())

	app.safeEmit(gatewayLogEvent, GatewayLogDTO{Stream: gatewayLogStream, Line: "x", Timestamp: "t"})
}
