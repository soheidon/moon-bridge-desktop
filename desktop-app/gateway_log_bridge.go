package main

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"time"
)

// gatewayLogStream is the stream label applied to every replicated line. The
// gateway's Errors writer is the process stderr stream, so lines carry the
// "stderr" label, matching the frontend GatewayLog contract and the
// ProcessLogPanel error styling.
const gatewayLogStream = "stderr"

// GatewayLogDTO is the JSON shape emitted on the "gateway-log" Wails event. It
// mirrors the frontend GatewayLog type {stream, line, timestamp}.
type GatewayLogDTO struct {
	Stream    string `json:"stream"`
	Line      string `json:"line"`
	Timestamp string `json:"timestamp"`
}

// ansiPattern matches CSI escape sequences (color, cursor movement) so the GUI
// log never carries raw terminal control bytes.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// Redaction patterns are defense-in-depth: current startup output contains no
// secrets, but a future line must never leak one into the GUI.
var (
	// authorization is redacted to end of line: matching only the next token
	// would leave a "Bearer abc…" value in place.
	authHeaderPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*).*$`)
	credentialPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret)\s*[=:]\s*["']?)[^\s"']+`)
	skTokenPattern    = regexp.MustCompile(`\bsk-[A-Za-z0-9]{16,}\b`)
)

func redactSecrets(line string) string {
	line = authHeaderPattern.ReplaceAllString(line, `${1}[REDACTED]`)
	line = credentialPattern.ReplaceAllString(line, `${1}[REDACTED]`)
	line = skTokenPattern.ReplaceAllString(line, `sk-[REDACTED]`)
	return line
}

// gatewayLogBridge converts a byte stream into per-line "gateway-log" events
// without replacing the original output. emit may be nil; Write never returns
// an error, so the gateway's log writes cannot fail even when no frontend is
// connected.
type gatewayLogBridge struct {
	emit func(name string, payload any)
	mu   sync.Mutex
	buf  []byte
}

func newGatewayLogBridge(emit func(name string, payload any)) *gatewayLogBridge {
	return &gatewayLogBridge{emit: emit}
}

// Write buffers input and emits one event per complete line. The mutex is held
// only to extract completed lines and update the buffer; emitting happens after
// unlock so a reentrant or blocking emit cannot deadlock.
func (b *gatewayLogBridge) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	var lines []string
	for {
		index := bytes.IndexByte(b.buf, '\n')
		if index < 0 {
			break
		}
		lines = append(lines, strings.TrimSuffix(string(b.buf[:index]), "\r"))
		b.buf = b.buf[index+1:]
	}
	b.mu.Unlock()

	for _, line := range lines {
		b.emitLine(line)
	}
	return len(p), nil
}

func (b *gatewayLogBridge) emitLine(line string) {
	if line == "" {
		return
	}
	dto := GatewayLogDTO{
		Stream:    gatewayLogStream,
		Line:      redactSecrets(ansiPattern.ReplaceAllString(line, "")),
		Timestamp: time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	if b.emit != nil {
		// A panicking emitter (e.g. Wails events during shutdown) must never
		// fail the gateway's log write.
		defer func() { _ = recover() }()
		b.emit(gatewayLogEvent, dto)
	}
}
