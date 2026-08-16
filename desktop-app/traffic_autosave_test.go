package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/service/trafficanalysis"
)

// fakeObs is a thread-safe observation source for writer tests. It hands out
// only the sequences after the requested watermark so the writer can advance
// its own cursor.
type fakeObs struct {
	mu      sync.Mutex
	items   []trafficanalysis.Observation
	dropped uint64
}

func (f *fakeObs) obs(after uint64) ([]trafficanalysis.Observation, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []trafficanalysis.Observation
	for _, o := range f.items {
		if o.Sequence > after {
			out = append(out, o)
		}
	}
	return out, f.dropped
}

func (f *fakeObs) add(seq uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, trafficTestObs(seq))
}

func trafficTestObs(seq uint64) trafficanalysis.Observation {
	return trafficanalysis.Observation{
		Sequence:        seq,
		Timestamp:       time.Date(2026, 8, 3, 0, 0, 0, int(seq%1e9), time.UTC),
		Direction:       trafficanalysis.DirectionClientToUpstream,
		Transport:       trafficanalysis.TransportHTTP,
		Method:          "POST",
		StatusCode:      200,
		PayloadKind:     trafficanalysis.PayloadJSON,
		ContentEncoding: "zstd",
		PayloadShape: &trafficanalysis.PayloadShape{
			InputItemCount:      2,
			InputItemTypeCounts: map[string]int{"message": 1, "function_call": 1},
			InputRoleCounts:     map[string]int{"user": 1},
			InputItemFingerprints: []trafficanalysis.InputItemFingerprint{{
				Index: 1, Fields: []string{"type", "role", "content"}, Type: "message", Role: "user", ContentCount: 1, ObjectCount: 2, ArrayCount: 1,
			}},
		},
		RawPayloadSize:         120,
		DecodedObservationSize: 110,
		DecodingStatus:         trafficanalysis.DecodingDecoded,
		Disposition:            trafficanalysis.DispositionRecorded,
		Usage:                  &trafficanalysis.UsageSummary{InputTokens: intPtr(500), OutputTokens: intPtr(100), TotalTokens: intPtr(600), CachedInputTokens: intPtr(0), ReasoningTokens: intPtr(50)},
		// Fields that must never reach the log; desktopObservations drops them.
		ReceivedHost:   "SENTINEL_HOST",
		ReceivedPath:   "/responses?api_key=SENTINEL_KEY",
		UpstreamHost:   "SENTINEL_UPSTREAM",
		RawPayloadHMAC: "SENTINEL_HMAC",
		// shape fields are configured above so the log can verify safe summaries.
		HeaderSummary:       trafficanalysis.HeaderSummary{PresentNames: []string{"authorization"}},
		QueryParameterNames: []string{"api_key"},
	}
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(data)
}

func TestTrafficLogWriterRendersSecretFreeAndFooters(t *testing.T) {
	dir := t.TempDir()
	source := &fakeObs{}
	source.add(1)
	source.add(2)
	source.add(3)

	w, err := newTrafficLogWriter(dir, source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	w.flushNew()
	w.Close(true)
	if got := w.safeStatus(); got != "finalized" {
		t.Fatalf("safeStatus() = %q, want finalized", got)
	}

	content := readLogFile(t, w.path)
	if !strings.HasPrefix(content, "Moon Bridge Codex Traffic Analysis\n") {
		t.Fatalf("missing header prefix: %q", content)
	}
	for _, want := range []string{"Status: active\n", "Session-ID: ", "Started-At: "} {
		if !strings.Contains(content, want) {
			t.Fatalf("header missing %q: %q", want, content)
		}
	}
	for _, want := range []string{"#1", "#2", "#3", "POST", "status_code: 200", "payload_kind: json",
		"content_encoding: zstd", "input_item_count: 2", "input_item_type_counts: function_call=1,message=1", "input_role_counts: user=1", "input_item_fingerprint: index=1 fields=type,role,content type=message role=user content_count=1 object_count=2 array_count=1", "raw_payload_size: 120", "decoded_size: 110", "decoding_status: decoded", "disposition: recorded"} {
		if !strings.Contains(content, want) {
			t.Fatalf("log missing %q: %q", want, content)
		}
	}
	if !strings.Contains(content, "Status: completed") || !strings.Contains(content, "Observations: 3") {
		t.Fatalf("completed footer missing: %q", content)
	}
	for _, sentinel := range []string{"SENTINEL_HOST", "SENTINEL_KEY", "SENTINEL_UPSTREAM", "SENTINEL_HMAC",
		"SENTINEL_MODEL", "authorization", "api_key", "/responses"} {
		if strings.Contains(content, sentinel) {
			t.Fatalf("log leaked secret %q: %q", sentinel, content)
		}
	}
	if !strings.Contains(content, "input_tokens: 500") {
		t.Fatalf("usage input_tokens missing: %q", content)
	}
	if !strings.Contains(content, "output_tokens: 100") {
		t.Fatalf("usage output_tokens missing: %q", content)
	}
	if !strings.Contains(content, "cached_input_tokens: 0") {
		t.Fatalf("usage cached_input_tokens missing: %q", content)
	}
}

func TestRenderObservationLineIncludesBaselineState(t *testing.T) {
	o := TrafficObservation{
		Kind:      "routing_resolution_diagnosed",
		Sequence:  1,
		Timestamp: "2026-08-03T00:00:00Z",
		Direction: "client_to_upstream",
		Transport: "http",
		GatewayEvent: &TrafficGatewayEvent{
			Resolver: &TrafficResolverDiagnostic{
				RequestedModel: "gpt-5.6-sol",
				SlotCount:      3,
				SolState:       "ready",
				TerraState:     "ready",
				LunaState:      "ready",
				BaselineState:  "ready",
			},
		},
	}
	got := renderObservationLine(o)
	if !strings.Contains(got, "baseline_state: ready") {
		t.Fatalf("renderObservationLine() missing baseline_state: %q", got)
	}
}

func TestTrafficLogWriterCloseWithoutFooterLeavesActive(t *testing.T) {
	dir := t.TempDir()
	source := &fakeObs{}
	source.add(1)

	w, err := newTrafficLogWriter(dir, source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	w.flushNew()
	w.Close(false)

	content := readLogFile(t, w.path)
	if strings.Contains(content, "Status: completed") {
		t.Fatalf("Close(false) wrote a completed footer: %q", content)
	}
	if !strings.Contains(content, "Status: active") {
		t.Fatalf("Close(false) lost the active marker: %q", content)
	}
	if !strings.Contains(content, "#1") {
		t.Fatalf("Close(false) dropped flushed observations: %q", content)
	}
}

func TestTrafficLogWriterCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	source := &fakeObs{}
	source.add(1)

	w, err := newTrafficLogWriter(dir, source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	w.Close(true)
	first := readLogFile(t, w.path)
	w.Close(true)
	w.Close(false)
	second := readLogFile(t, w.path)
	if first != second {
		t.Fatalf("second Close changed the log:\nfirst:\n%q\nsecond:\n%q", first, second)
	}
	if got := w.safeStatus(); got != "finalized" {
		t.Fatalf("safeStatus() = %q, want finalized", got)
	}
}

func TestTrafficLogWriterFinalDrainWritesLateObservations(t *testing.T) {
	dir := t.TempDir()
	source := &fakeObs{}
	source.add(1)
	source.add(2)

	w, err := newTrafficLogWriter(dir, source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	w.flushNew()  // flush 1-2
	source.add(3) // arrives after the last flush, before Close
	w.Close(true)

	content := readLogFile(t, w.path)
	if !strings.Contains(content, "#3") {
		t.Fatalf("Close() did not drain late observations: %q", content)
	}
	if !strings.Contains(content, "Observations: 3") {
		t.Fatalf("footer count = wrong: %q", content)
	}
}

func TestTrafficLogWriterCollisionSuffix(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 3, 12, 34, 56, 0, time.UTC)
	base := filepath.Join(dir, "traffic-analysis-20260803-123456.log")
	if err := os.WriteFile(base, []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, file, err := createTrafficLogFileAt(dir, now)
	if err != nil {
		t.Fatalf("createTrafficLogFileAt() error = %v", err)
	}
	_ = file.Close()
	if filepath.Base(path) != "traffic-analysis-20260803-123456-001.log" {
		t.Fatalf("collision suffix path = %q, want -001", path)
	}
}

func TestTrafficLogWriterCopyToFlushesActiveWriter(t *testing.T) {
	dir := t.TempDir()
	source := &fakeObs{}
	source.add(1)

	w, err := newTrafficLogWriter(dir, source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	defer w.Close(false)
	w.flushNew()
	source.add(2) // pending; copyTo must flush it before copying

	dst := filepath.Join(t.TempDir(), "copy.log")
	if err := w.copyTo(dst); err != nil {
		t.Fatalf("copyTo() error = %v", err)
	}
	content := readLogFile(t, dst)
	if !strings.Contains(content, "#1") || !strings.Contains(content, "#2") {
		t.Fatalf("copyTo() missed pending observations: %q", content)
	}
}

func TestRetainTrafficLogsKeepsNewest30(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 35; i++ {
		name := filepath.Join(dir, fmt.Sprintf("traffic-analysis-20260803-%02d0000.log", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	retainTrafficLogs(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 30 {
		t.Fatalf("retained %d logs, want 30", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "traffic-analysis-20260803-000000.log")); !os.IsNotExist(err) {
		t.Fatal("oldest log was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "traffic-analysis-20260803-340000.log")); err != nil {
		t.Fatalf("newest log was removed: %v", err)
	}
}

func TestIsTrafficLogName(t *testing.T) {
	valid := []string{
		"traffic-analysis-20260803-123456.log",
		"traffic-analysis-20260803-123456-007.log",
	}
	for _, name := range valid {
		if !isTrafficLogName(name) {
			t.Errorf("isTrafficLogName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"traffic-analysis.log",
		"traffic-analysis-20260803.log",
		"traffic-analysis-20260803-1234.log",
		"traffic-analysis-20260803-123456-01.log",
		"traffic-analysis-20260803-12345x.log",
		"other-20260803-123456.log",
		"traffic-analysis-20260803-123456.txt",
	}
	for _, name := range invalid {
		if isTrafficLogName(name) {
			t.Errorf("isTrafficLogName(%q) = true, want false", name)
		}
	}
}

func TestTrafficLogWriterSafeStatusClassifies(t *testing.T) {
	dir := t.TempDir()
	source := &fakeObs{}
	source.add(1)
	w, err := newTrafficLogWriter(dir, source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	if got := w.safeStatus(); got != "active" {
		t.Fatalf("safeStatus() before any event = %q, want active", got)
	}
	// A write error while the writer is active classifies it as failed.
	w.mu.Lock()
	w.safeErr = "autosave_write_failed"
	w.mu.Unlock()
	if got := w.safeStatus(); got != "failed" {
		t.Fatalf("safeStatus() with safeErr = %q, want failed", got)
	}
	// Closing terminates the classification. finalized wins over failed: the
	// footer is skipped because of the prior write error, but the writer's
	// terminal state is still finalized.
	w.Close(true)
	if got := w.safeStatus(); got != "finalized" {
		t.Fatalf("safeStatus() after close = %q, want finalized", got)
	}
}
