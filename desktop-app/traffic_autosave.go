package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/trafficanalysis"
)

const (
	trafficLogPollInterval = 500 * time.Millisecond
	trafficLogRetention    = 30
)

// trafficLogWriter appends secret-free observation summaries to a log file in
// the traffic-analysis folder while a capture session runs. It is the Go
// desktop equivalent of Tauri's initialize_autosave / sync_autosave /
// finalize_autosave, restricted to the secret-free Desktop DTO fields
// (no prompts, bodies, responses, headers, URL paths, API keys, or model
// names). Append failures are soft: the capture keeps running and the failure
// is classified into safeErr for the GUI.
type trafficLogWriter struct {
	dir  string
	path string

	mu        sync.Mutex
	file      *os.File
	after     uint64 // last sequence flushed to disk
	written   uint64 // observations appended since creation
	gaps      uint64 // sequence gaps observed while flushing
	dropped   uint64 // latest dropped count reported by the observer
	finalized bool
	safeErr   string

	sessionID string
	startedAt string

	obs       func(uint64) ([]trafficanalysis.Observation, uint64)
	stop      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

// newTrafficLogWriter creates the log file (with header) and starts the poll
// goroutine that flushes new observations on a short ticker.
func newTrafficLogWriter(dir string, obs func(uint64) ([]trafficanalysis.Observation, uint64)) (*trafficLogWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path, file, err := createTrafficLogFile(dir)
	if err != nil {
		return nil, err
	}
	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	w := &trafficLogWriter{
		dir:       dir,
		path:      path,
		file:      file,
		obs:       obs,
		sessionID: uuid.NewString(),
		startedAt: startedAt,
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	if _, err := file.WriteString(renderLogHeader(w.sessionID, startedAt)); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	go w.poll()
	retainTrafficLogs(dir)
	return w, nil
}

// createTrafficLogFile opens a new traffic-analysis-YYYYMMDD-HHMMSS[.NNN].log
// in dir, disambiguating same-second collisions with an incrementing suffix.
func createTrafficLogFile(dir string) (string, *os.File, error) {
	return createTrafficLogFileAt(dir, time.Now())
}

// createTrafficLogFileAt is createTrafficLogFile with an injectable clock for
// deterministic collision tests.
func createTrafficLogFileAt(dir string, now time.Time) (string, *os.File, error) {
	stamp := now.Format("20060102-150405")
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("traffic-analysis-%s.log", stamp)
		if i > 0 {
			name = fmt.Sprintf("traffic-analysis-%s-%03d.log", stamp, i)
		}
		path := filepath.Join(dir, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return path, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("traffic log filename collision limit reached")
}

func (w *trafficLogWriter) poll() {
	defer close(w.stopped)
	ticker := time.NewTicker(trafficLogPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.flushNew()
		}
	}
}

// flushNew drains pending observations into the file. Write/sync failures are
// classified into safeErr and stop further appends, but never touch the
// capture itself.
func (w *trafficLogWriter) flushNew() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finalized || w.file == nil || w.safeErr != "" {
		return
	}
	w.appendLocked()
}

// appendLocked drains the observer's pending observations into the file. The
// caller must hold w.mu and guarantee w.file is open.
func (w *trafficLogWriter) appendLocked() {
	if w.obs == nil {
		return
	}
	items, dropped := w.obs(w.after)
	if len(items) == 0 {
		return
	}
	var out strings.Builder
	next := w.after
	var appended uint64
	for _, dto := range desktopObservations(items) {
		if dto.Sequence <= w.after {
			continue
		}
		if dto.Sequence > next+1 {
			w.gaps += dto.Sequence - next - 1
		}
		out.WriteString(renderObservationLine(dto))
		next = dto.Sequence
		appended++
	}
	if out.Len() == 0 {
		return
	}
	if _, err := w.file.WriteString(out.String()); err != nil {
		w.safeErr = "autosave_write_failed"
		return
	}
	if err := w.file.Sync(); err != nil {
		w.safeErr = "autosave_write_failed"
		return
	}
	w.after = next
	w.written += appended
	w.dropped = dropped
}

// safeStatus returns the secret-free autosave classification: active while
// appending, failed after a write error, finalized once closed.
func (w *trafficLogWriter) safeStatus() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finalized {
		return "finalized"
	}
	if w.safeErr != "" {
		return "failed"
	}
	return "active"
}

// Close stops appending and closes the log file. It is idempotent: only the
// first caller runs the teardown, and its finalize flag decides whether a
// "Status: completed" footer is written. Stop/Finish/shutdown pass true; an
// abnormal EndRun passes false so the file stays honest as "Status: active".
func (w *trafficLogWriter) Close(finalize bool) {
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.stopped

		w.mu.Lock()
		defer w.mu.Unlock()
		if w.file == nil {
			w.finalized = true
			return
		}
		// Final drain: any observations that landed after the last tick.
		w.appendLocked()
		if w.safeErr == "" && finalize {
			_, _ = w.file.WriteString(renderLogFooter(w.sessionID, w.startedAt,
				time.Now().UTC().Format(time.RFC3339Nano), w.written, w.gaps, w.dropped))
		}
		_ = w.file.Sync()
		_ = w.file.Close()
		w.file = nil
		w.finalized = true
		retainTrafficLogs(w.dir)
	})
}

// copyTo writes a flush-consistent copy of the log to dst. When the writer is
// still active it drains any pending observations and syncs first, then copies
// via temp+rename so dst never holds a partially written file.
func (w *trafficLogWriter) copyTo(dst string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil && w.safeErr == "" {
		w.appendLocked()
	}
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			return err
		}
	}
	return copyFileAtomic(w.path, dst)
}

// observationCount reports the number of observations appended to the file.
func (w *trafficLogWriter) observationCount() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// copyFileAtomic copies src to dst via writeFileAtomic.
func copyFileAtomic(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data)
}

// writeFileAtomic writes data to dst via a temp file in the destination
// directory plus rename, so a crash mid-write never leaves a truncated dst.
func writeFileAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".moon-bridge-export-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows os.Rename fails when dst exists; replace it first. The temp file
	// already isolates readers from a torn write.
	_ = os.Remove(dst)
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	return nil
}

// collectTrafficLogs returns the traffic log file paths in dir in name order
// (the timestamp names sort chronologically).
func collectTrafficLogs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var logs []string
	for _, e := range entries {
		if e.IsDir() || !isTrafficLogName(e.Name()) {
			continue
		}
		logs = append(logs, filepath.Join(dir, e.Name()))
	}
	sort.Strings(logs)
	return logs
}

// latestTrafficLogPath returns the most recently created traffic log file in
// the log folder, or "" when none exists.
func (a *App) latestTrafficLogPath() string {
	logs := collectTrafficLogs(a.trafficLogDirPath())
	if len(logs) == 0 {
		return ""
	}
	return logs[len(logs)-1]
}

// retainTrafficLogs keeps the 30 most recent traffic logs in dir, removing
// older files by name order.
func retainTrafficLogs(dir string) {
	logs := collectTrafficLogs(dir)
	if len(logs) <= trafficLogRetention {
		return
	}
	for _, p := range logs[:len(logs)-trafficLogRetention] {
		_ = os.Remove(p)
	}
}

// isTrafficLogName matches traffic-analysis-YYYYMMDD-HHMMSS[.NNN].log, with
// every part ASCII digits, mirroring the Tauri naming rules so logs from
// either runtime may coexist in the same folder.
func isTrafficLogName(name string) bool {
	stem, ok := strings.CutSuffix(name, ".log")
	if !ok {
		return false
	}
	rest, ok := strings.CutPrefix(stem, "traffic-analysis-")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "-")
	digits := func(s string, n int) bool {
		if len(s) != n {
			return false
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	switch len(parts) {
	case 2:
		return digits(parts[0], 8) && digits(parts[1], 6)
	case 3:
		return digits(parts[0], 8) && digits(parts[1], 6) && digits(parts[2], 3)
	}
	return false
}

// ---- render helpers (shared by autosave and export) ----

func renderLogHeader(sessionID, startedAt string) string {
	return fmt.Sprintf("Moon Bridge Codex Traffic Analysis\nSession-ID: %s\nStarted-At: %s\nStatus: active\n\n", sessionID, startedAt)
}

func renderLogFooter(sessionID, startedAt, endedAt string, observations, gaps, dropped uint64) string {
	return fmt.Sprintf("Status: completed\nSession-ID: %s\nStarted-At: %s\nEnded-At: %s\nObservations: %d\nSequence-Gaps: %d\nDropped: %d\n", sessionID, startedAt, endedAt, observations, gaps, dropped)
}

// renderObservationLine renders one secret-free observation row. Only fields
// on the Desktop DTO are emitted; prompts, bodies, responses, headers, URL
// paths, and model/provider names never exist here.
func renderObservationLine(o TrafficObservation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s #%d %s %s\n", o.Timestamp, o.Sequence, o.Direction, o.Transport)
	appendLogField(&b, "  kind", o.Kind)
	if o.GatewayEvent != nil {
		e := o.GatewayEvent
		appendLogField(&b, "  request_alias", e.RequestAlias)
		appendLogField(&b, "  requested_model", e.RequestedModel)
		appendLogField(&b, "  routing_slot", e.RoutingSlot)
		appendLogField(&b, "  active_profile", e.ActiveProfile)
		appendLogField(&b, "  provider", e.Provider)
		appendLogField(&b, "  upstream_model", e.UpstreamModel)
		appendLogField(&b, "  mode", e.Mode)
		appendLogField(&b, "  configured_effort", e.ConfiguredEffort)
		appendLogField(&b, "  protocol", e.Protocol)
		appendLogField(&b, "  model", e.Model)
		appendLogField(&b, "  thinking", e.Thinking)
		appendLogField(&b, "  effective_effort", e.EffectiveEffort)
		if e.Resolver != nil {
			r := e.Resolver
			appendLogField(&b, "  resolver_requested_model", r.RequestedModel)
			appendLogField(&b, "  server_instance", r.ServerInstance)
			fmt.Fprintf(&b, "  resolver_generation: %d\n", r.ResolverGeneration)
			fmt.Fprintf(&b, "  resolver_present: %t\n", r.ResolverPresent)
			appendLogField(&b, "  install_source", r.InstallSource)
			appendLogField(&b, "  config_source", r.ConfigSource)
			appendLogField(&b, "  extension_state", r.ExtensionState)
			appendLogField(&b, "  active_profile_state", r.ActiveProfileState)
			fmt.Fprintf(&b, "  slot_count: %d\n", r.SlotCount)
			appendLogField(&b, "  sol_state", r.SolState)
			appendLogField(&b, "  terra_state", r.TerraState)
			appendLogField(&b, "  luna_state", r.LunaState)
			appendLogField(&b, "  normal_result", r.NormalResult)
			appendLogField(&b, "  resolved_slot", r.ResolvedSlot)
			appendLogField(&b, "  fallback_result", r.FallbackResult)
			appendLogField(&b, "  final_stage", r.FinalStage)
		}
		return b.String()
	}
	appendLogField(&b, "  request_alias", o.RequestAlias)
	appendLogField(&b, "  method", o.Method)
	if o.StatusCode != 0 {
		fmt.Fprintf(&b, "  status_code: %d\n", o.StatusCode)
	}
	appendLogField(&b, "  content_encoding", o.ContentEncoding)
	appendLogField(&b, "  sse_event_type", o.SSEEventType)
	appendLogField(&b, "  payload_kind", o.PayloadKind)
	if o.PayloadShape != nil {
		appendLogField(&b, "  request_model", o.PayloadShape.RequestModel)
		appendLogField(&b, "  top_level_fields", strings.Join(o.PayloadShape.TopLevelFields, ","))
		appendLogField(&b, "  event_type", o.PayloadShape.EventType)
		appendLogField(&b, "  object_type", o.PayloadShape.ObjectType)
		appendLogField(&b, "  status", o.PayloadShape.Status)
		if o.PayloadShape.ToolCount != 0 {
			fmt.Fprintf(&b, "  tool_count: %d\n", o.PayloadShape.ToolCount)
		}
		if o.PayloadShape.InputItemCount != 0 {
			fmt.Fprintf(&b, "  input_item_count: %d\n", o.PayloadShape.InputItemCount)
		}
		appendLogField(&b, "  input_item_type_counts", formatCountMap(o.PayloadShape.InputItemTypeCounts))
		appendLogField(&b, "  input_role_counts", formatCountMap(o.PayloadShape.InputRoleCounts))
		for _, item := range o.PayloadShape.InputItemFingerprints {
			fmt.Fprintf(&b, "  input_item_fingerprint: index=%d fields=%s type=%s role=%s content_count=%d object_count=%d array_count=%d\n", item.Index, strings.Join(item.Fields, ","), item.Type, item.Role, item.ContentCount, item.ObjectCount, item.ArrayCount)
			appendFingerprintAliases(&b, &item.Identifiers)
		}
	}
	appendLogField(&b, "  response_id_aliases", strings.Join(o.Identifiers.ResponseIDAliases, ","))
	appendLogField(&b, "  previous_response_id_aliases", strings.Join(o.Identifiers.PreviousResponseIDAliases, ","))
	appendLogField(&b, "  item_id_aliases", strings.Join(o.Identifiers.ItemIDAliases, ","))
	appendLogField(&b, "  call_id_aliases", strings.Join(o.Identifiers.CallIDAliases, ","))
	appendLogField(&b, "  conversation_id_aliases", strings.Join(o.Identifiers.ConversationIDAliases, ","))
	appendLogField(&b, "  other_id_aliases", strings.Join(o.Identifiers.OtherIDAliases, ","))
	if o.RawPayloadSize != 0 {
		fmt.Fprintf(&b, "  raw_payload_size: %d\n", o.RawPayloadSize)
	}
	if o.DecodedObservationSize != 0 {
		fmt.Fprintf(&b, "  decoded_size: %d\n", o.DecodedObservationSize)
	}
	appendLogField(&b, "  decoding_status", o.DecodingStatus)
	if o.Partial {
		b.WriteString("  partial: true\n")
	}
	if o.Truncated {
		b.WriteString("  truncated: true\n")
	}
	appendLogField(&b, "  disposition", o.Disposition)
	appendLogField(&b, "  error_class", o.ErrorClass)
	appendUsageBlock(&b, o.Usage)
	b.WriteByte('\n')
	return b.String()
}

func appendFingerprintAliases(b *strings.Builder, ids *TrafficIdentifierSummary) {
	appendLogField(b, "    response_id_aliases", strings.Join(ids.ResponseIDAliases, ","))
	appendLogField(b, "    previous_response_id_aliases", strings.Join(ids.PreviousResponseIDAliases, ","))
	appendLogField(b, "    item_id_aliases", strings.Join(ids.ItemIDAliases, ","))
	appendLogField(b, "    call_id_aliases", strings.Join(ids.CallIDAliases, ","))
	appendLogField(b, "    conversation_id_aliases", strings.Join(ids.ConversationIDAliases, ","))
	appendLogField(b, "    other_id_aliases", strings.Join(ids.OtherIDAliases, ","))
}

func appendUsageBlock(b *strings.Builder, usage *TrafficUsageSummary) {
	if usage == nil {
		return
	}
	appendUsageField(b, "  input_tokens", usage.InputTokens)
	appendUsageField(b, "  output_tokens", usage.OutputTokens)
	appendUsageField(b, "  total_tokens", usage.TotalTokens)
	appendUsageField(b, "  cached_input_tokens", usage.CachedInputTokens)
	appendUsageField(b, "  reasoning_tokens", usage.ReasoningTokens)
}

func appendUsageField(b *strings.Builder, label string, value *int) {
	if value == nil {
		return
	}
	b.WriteString(label)
	b.WriteString(": ")
	b.WriteString(strconv.Itoa(*value))
	b.WriteByte('\n')
}

func formatCountMap(values map[string]int) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ",")
}

func appendLogField(b *strings.Builder, label, value string) {
	if value != "" {
		fmt.Fprintf(b, "%s: %s\n", label, value)
	}
}

// ---- App-level wiring ----

// trafficLogDirPath returns the traffic-analysis log directory, preferring the
// path cached during ensureRecoveryStore.
func (a *App) trafficLogDirPath() string {
	if a.trafficLogDir != "" {
		return a.trafficLogDir
	}
	base, err := recovery.DefaultDir(os.Getenv)
	if err != nil {
		return ""
	}
	return filepath.Join(base, "logs", "traffic-analysis")
}

// startTrafficAutosaveLocked creates a fresh autosave writer for a new capture
// session. A writer-generation failure is a soft-fail: the analysis keeps
// running and the failure is surfaced through autoSaveStatus instead of
// failing the already-successful Start command.
func (a *App) startTrafficAutosaveLocked() {
	if prev := a.trafficLog.Load(); prev != nil {
		prev.Close(false)
	}
	w, err := newTrafficLogWriter(a.trafficLogDirPath(), a.traffic.Observations)
	if err != nil {
		code := "autosave_init_failed"
		a.trafficLogInitErr.Store(&code)
		log.Printf("traffic autosave init failed (analysis continues): %s", code)
		return
	}
	a.trafficLog.Store(w)
	a.trafficLogInitErr.Store(nil)
}

func (a *App) closeTrafficAutosaveLocked(finalize bool) {
	if w := a.trafficLog.Load(); w != nil {
		w.Close(finalize)
	}
}

// autoSaveStatus reports the current autosave classification for snapshots.
// Safe without trafficMu: the writer pointer is atomic and the writer reports
// under its own mutex.
func (a *App) autoSaveStatus() string {
	if w := a.trafficLog.Load(); w != nil {
		return w.safeStatus()
	}
	if code := a.trafficLogInitErr.Load(); code != nil && *code != "" {
		return "failed"
	}
	return ""
}

// renderObservationsLog renders the in-memory observations to a log document
// using the same secret-free format as the autosave writer. It is the export
// fallback when no autosave log exists.
func (a *App) renderObservationsLog() (string, uint64) {
	items, dropped := a.traffic.Observations(0)
	var b strings.Builder
	now := time.Now().UTC().Format(time.RFC3339Nano)
	b.WriteString(renderLogHeader(uuid.NewString(), now))
	fmt.Fprintf(&b, "Observations: %d\nDropped: %d\n\n", len(items), dropped)
	for _, dto := range desktopObservations(items) {
		b.WriteString(renderObservationLine(dto))
	}
	return b.String(), uint64(len(items))
}

// defaultExplorerFunc reveals or opens a path in the platform file explorer.
// On Windows it shells out to explorer.exe fire-and-forget (explorer returns a
// non-zero exit even on success). Non-Windows reports reveal_unsupported.
func defaultExplorerFunc(args ...string) error {
	if runtime.GOOS != "windows" {
		return errors.New("reveal_unsupported")
	}
	cmd := exec.Command("explorer.exe", args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}
