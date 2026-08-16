package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"moonbridge/internal/service/trafficanalysis"
)

func TestSaveFileDialogBinding(t *testing.T) {
	app := NewApp(AppOptions{})
	app.ctx = context.Background()

	t.Run("success", func(t *testing.T) {
		app.saveDialogFunc = func(ctx context.Context, opts runtime.SaveDialogOptions) (string, error) {
			if opts.DefaultFilename != "moon-bridge-traffic-analysis-test.log" {
				t.Fatalf("DefaultFilename = %q", opts.DefaultFilename)
			}
			if len(opts.Filters) != 1 || opts.Filters[0].DisplayName != "Log" || opts.Filters[0].Pattern != "*.log" {
				t.Fatalf("Filters = %#v", opts.Filters)
			}
			return filepath.Join(t.TempDir(), "export.log"), nil
		}
		result := app.SaveFileDialog(SaveFileDialogOptions{
			DefaultFilename: "moon-bridge-traffic-analysis-test.log",
			Filters:         []FileFilter{{DisplayName: "Log", Pattern: "*.log"}},
		})
		if !result.OK || result.Value == nil || result.Value.SaveDialog == nil {
			t.Fatalf("SaveFileDialog() = %#v, want success", result)
		}
		if result.Value.SaveDialog.Canceled || result.Value.SaveDialog.Path == "" {
			t.Fatalf("SaveFileDialog() snapshot = %#v, want a path", result.Value.SaveDialog)
		}
	})

	t.Run("cancel returns ok with canceled flag", func(t *testing.T) {
		app.saveDialogFunc = func(ctx context.Context, opts runtime.SaveDialogOptions) (string, error) { return "", nil }
		result := app.SaveFileDialog(SaveFileDialogOptions{})
		if !result.OK || result.Value == nil || result.Value.SaveDialog == nil {
			t.Fatalf("cancel SaveFileDialog() = %#v, want ok", result)
		}
		if !result.Value.SaveDialog.Canceled || result.Value.SaveDialog.Path != "" {
			t.Fatalf("cancel snapshot = %#v, want canceled", result.Value.SaveDialog)
		}
	})

	t.Run("dialog error is surfaced", func(t *testing.T) {
		app.saveDialogFunc = func(ctx context.Context, opts runtime.SaveDialogOptions) (string, error) {
			return "", errors.New("dialog boom")
		}
		result := app.SaveFileDialog(SaveFileDialogOptions{})
		if result.OK || result.Error == nil || result.Error.Code != "save_dialog_failed" {
			t.Fatalf("error SaveFileDialog() = %#v, want save_dialog_failed", result)
		}
	})

	t.Run("missing runtime context is rejected", func(t *testing.T) {
		app.ctx = nil
		result := app.SaveFileDialog(SaveFileDialogOptions{})
		if result.OK || result.Error == nil || result.Error.Code != "desktop_context_unavailable" {
			t.Fatalf("SaveFileDialog() without ctx = %#v, want desktop_context_unavailable", result)
		}
	})
}

func TestTrafficAnalysisExportCopiesAutosave(t *testing.T) {
	app := NewApp(AppOptions{})
	source := &fakeObs{}
	source.add(1)
	source.add(2)
	source.add(3)

	w, err := newTrafficLogWriter(t.TempDir(), source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	defer w.Close(false)
	app.trafficLog.Store(w)

	dst := filepath.Join(t.TempDir(), "export.log")
	result := app.TrafficAnalysisExport(TrafficExportRequest{Destination: dst})
	if !result.OK || result.Value == nil || result.Value.Export == nil {
		t.Fatalf("TrafficAnalysisExport() = %#v, want success", result)
	}
	if result.Value.Export.Destination != dst {
		t.Fatalf("export destination = %q, want %q", result.Value.Export.Destination, dst)
	}
	if result.Value.Export.ObservationCount != 3 {
		t.Fatalf("export observationCount = %d, want 3", result.Value.Export.ObservationCount)
	}
	content := readLogFile(t, dst)
	// The source writer is still active, so the copy carries the live header and
	// rows without a completed footer.
	if !strings.Contains(content, "#3") || !strings.Contains(content, "Status: active") {
		t.Fatalf("exported log missing live observations: %q", content)
	}
	if strings.Contains(content, "Status: completed") {
		t.Fatalf("export of a live writer must not carry a completed footer: %q", content)
	}
	// The reveal ownership guard must accept the canonical exported path.
	revealed := app.TrafficAnalysisRevealExport(TrafficRevealRequest{Destination: dst})
	if !revealed.OK {
		t.Fatalf("TrafficAnalysisRevealExport() after export = %#v, want success", revealed)
	}
}

func TestTrafficAnalysisExportValidatesDestination(t *testing.T) {
	app := NewApp(AppOptions{})
	for name, dst := range map[string]string{
		"empty":    "",
		"relative": "export.log",
		"no-log":   filepath.Join(t.TempDir(), "export.txt"),
	} {
		result := app.TrafficAnalysisExport(TrafficExportRequest{Destination: dst})
		if result.OK || result.Error == nil || result.Error.Code != "invalid_export_destination" {
			t.Fatalf("%s: TrafficAnalysisExport(%q) = %#v, want invalid_export_destination", name, dst, result)
		}
	}
}

func TestTrafficAnalysisExportNoAutosaveNoObservations(t *testing.T) {
	app := NewApp(AppOptions{})
	dst := filepath.Join(t.TempDir(), "export.log")
	result := app.TrafficAnalysisExport(TrafficExportRequest{Destination: dst})
	if result.OK || result.Error == nil || result.Error.Code != "no_autosave_log" {
		t.Fatalf("TrafficAnalysisExport() without data = %#v, want no_autosave_log", result)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("failed export must not leave a file behind")
	}
}

func TestTrafficAnalysisExportRenderFallback(t *testing.T) {
	if listener, err := net.Listen("tcp", "127.0.0.1:38441"); err != nil {
		t.Skipf("capture listener 127.0.0.1:38441 is unavailable: %v", err)
	} else {
		_ = listener.Close()
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	app := NewApp(AppOptions{})
	if _, err := app.traffic.BindGatewayRun("export-test-gateway", "127.0.0.1:38440"); err != nil {
		t.Fatalf("BindGatewayRun() error = %v", err)
	}
	started, err := app.traffic.StartCapture(trafficanalysis.StartOptions{
		UpstreamBase: upstream.URL,
		ListenAddr:   "127.0.0.1:38441",
	})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	if started.CaptureState != "capturing" {
		t.Fatalf("StartCapture() state = %q, want capturing", started.CaptureState)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = app.traffic.StopCapture(ctx)
	}()

	body := `{"prompt":"SENTINEL_PROMPT","model":"SENTINEL_MODEL","api_key":"SENTINEL_KEY"}`
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:38441/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer SENTINEL_AUTH")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("capture request error = %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if st := app.traffic.Status(); st.ObservationCount >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture did not record the request: status = %#v", app.traffic.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// No autosave writer exists, so export must render from memory.
	dst := filepath.Join(t.TempDir(), "export.log")
	result := app.TrafficAnalysisExport(TrafficExportRequest{Destination: dst})
	if !result.OK || result.Value == nil || result.Value.Export == nil {
		t.Fatalf("TrafficAnalysisExport() fallback = %#v, want success", result)
	}
	if result.Value.Export.ObservationCount < 1 {
		t.Fatalf("fallback observationCount = %d, want >= 1", result.Value.Export.ObservationCount)
	}
	content := readLogFile(t, dst)
	for _, sentinel := range []string{"SENTINEL_PROMPT", "SENTINEL_MODEL", "SENTINEL_KEY", "SENTINEL_AUTH", "Bearer", "/responses"} {
		if strings.Contains(content, sentinel) {
			t.Fatalf("fallback export leaked sentinel %q: %q", sentinel, content)
		}
	}
}

func TestTrafficAnalysisRevealExportOwnershipGuard(t *testing.T) {
	app := NewApp(AppOptions{})
	dest := filepath.Join(t.TempDir(), "export.log")

	t.Run("rejects a destination this session did not export", func(t *testing.T) {
		result := app.TrafficAnalysisRevealExport(TrafficRevealRequest{Destination: dest})
		if result.OK || result.Error == nil || result.Error.Code != "reveal_ownership_mismatch" {
			t.Fatalf("TrafficAnalysisRevealExport() = %#v, want reveal_ownership_mismatch", result)
		}
	})

	t.Run("reveals an owned destination via the explorer seam", func(t *testing.T) {
		app.recordExport(dest)
		if err := os.WriteFile(dest, []byte("export"), 0o644); err != nil {
			t.Fatalf("os.WriteFile() error = %v", err)
		}
		var got []string
		app.explorerFunc = func(args ...string) error {
			got = append(got, args...)
			return nil
		}
		result := app.TrafficAnalysisRevealExport(TrafficRevealRequest{Destination: dest})
		if !result.OK || result.Value == nil || result.Value.RevealExport == nil {
			t.Fatalf("TrafficAnalysisRevealExport() = %#v, want success", result)
		}
		if len(got) != 1 || got[0] != "/select,"+dest {
			t.Fatalf("explorer args = %#v, want /select,%q", got, dest)
		}
	})

	t.Run("surfaces an explorer failure", func(t *testing.T) {
		app.explorerFunc = func(args ...string) error { return errors.New("no explorer") }
		result := app.TrafficAnalysisRevealExport(TrafficRevealRequest{Destination: dest})
		if result.OK || result.Error == nil || result.Error.Code != "reveal_unsupported" {
			t.Fatalf("TrafficAnalysisRevealExport() = %#v, want reveal_unsupported", result)
		}
	})

	t.Run("rejects an owned destination that no longer exists", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.log")
		app.recordExport(missing)
		result := app.TrafficAnalysisRevealExport(TrafficRevealRequest{Destination: missing})
		if result.OK || result.Error == nil || result.Error.Code != "export_destination_missing" {
			t.Fatalf("TrafficAnalysisRevealExport() = %#v, want export_destination_missing", result)
		}
	})
}

func TestTrafficAnalysisOpenLogFolder(t *testing.T) {
	app := NewApp(AppOptions{})
	logDir := filepath.Join(t.TempDir(), "logs", "traffic-analysis")
	app.trafficLogDir = logDir

	var got []string
	app.explorerFunc = func(args ...string) error {
		got = append(got, args...)
		return nil
	}
	result := app.TrafficAnalysisOpenLogFolder()
	if !result.OK {
		t.Fatalf("TrafficAnalysisOpenLogFolder() = %#v, want success", result)
	}
	if info, err := os.Stat(logDir); err != nil || !info.IsDir() {
		t.Fatalf("log folder not created: %v", err)
	}
	if len(got) != 1 || got[0] != logDir {
		t.Fatalf("explorer args = %#v, want %q", got, logDir)
	}

	t.Run("surfaces an explorer failure", func(t *testing.T) {
		app.explorerFunc = func(args ...string) error { return errors.New("no explorer") }
		result := app.TrafficAnalysisOpenLogFolder()
		if result.OK || result.Error == nil || result.Error.Code != "open_folder_unsupported" {
			t.Fatalf("TrafficAnalysisOpenLogFolder() = %#v, want open_folder_unsupported", result)
		}
	})
}

func TestTrafficAnalysisOpenLogFile(t *testing.T) {
	app := NewApp(AppOptions{})
	logDir := filepath.Join(t.TempDir(), "logs", "traffic-analysis")
	app.trafficLogDir = logDir

	t.Run("reports log_file_unavailable when no log exists", func(t *testing.T) {
		result := app.TrafficAnalysisOpenLogFile()
		if result.OK || result.Error == nil || result.Error.Code != "log_file_unavailable" {
			t.Fatalf("TrafficAnalysisOpenLogFile() = %#v, want log_file_unavailable", result)
		}
	})

	t.Run("opens the active writer's file", func(t *testing.T) {
		source := &fakeObs{}
		source.add(1)
		w, err := newTrafficLogWriter(logDir, source.obs)
		if err != nil {
			t.Fatalf("newTrafficLogWriter() error = %v", err)
		}
		defer w.Close(false)
		app.trafficLog.Store(w)

		var got []string
		app.explorerFunc = func(args ...string) error {
			got = append(got, args...)
			return nil
		}
		result := app.TrafficAnalysisOpenLogFile()
		if !result.OK {
			t.Fatalf("TrafficAnalysisOpenLogFile() = %#v, want success", result)
		}
		if len(got) != 1 || got[0] != w.path {
			t.Fatalf("explorer args = %#v, want %q", got, w.path)
		}
	})

	t.Run("falls back to the latest retained log without a writer", func(t *testing.T) {
		app.trafficLog.Store(nil)
		var got []string
		app.explorerFunc = func(args ...string) error {
			got = append(got, args...)
			return nil
		}
		result := app.TrafficAnalysisOpenLogFile()
		if !result.OK {
			t.Fatalf("TrafficAnalysisOpenLogFile() = %#v, want success", result)
		}
		if len(got) != 1 || filepath.Dir(got[0]) != logDir || !isTrafficLogName(filepath.Base(got[0])) {
			t.Fatalf("explorer args = %#v, want a traffic log path in %q", got, logDir)
		}
	})

	t.Run("surfaces an explorer failure", func(t *testing.T) {
		app.explorerFunc = func(args ...string) error { return errors.New("no explorer") }
		result := app.TrafficAnalysisOpenLogFile()
		if result.OK || result.Error == nil || result.Error.Code != "open_log_unsupported" {
			t.Fatalf("TrafficAnalysisOpenLogFile() = %#v, want open_log_unsupported", result)
		}
	})
}

func TestAutoSaveStatusReflectsWriterState(t *testing.T) {
	app := NewApp(AppOptions{})
	if got := app.autoSaveStatus(); got != "" {
		t.Fatalf("autoSaveStatus() with no writer = %q, want empty", got)
	}

	source := &fakeObs{}
	source.add(1)
	w, err := newTrafficLogWriter(t.TempDir(), source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	defer w.Close(false)
	app.trafficLog.Store(w)
	if got := app.autoSaveStatus(); got != "active" {
		t.Fatalf("autoSaveStatus() with active writer = %q, want active", got)
	}

	w.Close(true)
	if got := app.autoSaveStatus(); got != "finalized" {
		t.Fatalf("autoSaveStatus() after close = %q, want finalized", got)
	}

	// Soft-fail: writer creation failure must surface without a writer.
	app.trafficLog.Store(nil)
	code := "autosave_init_failed"
	app.trafficLogInitErr.Store(&code)
	if got := app.autoSaveStatus(); got != "failed" {
		t.Fatalf("autoSaveStatus() with init error = %q, want failed", got)
	}
}

func TestTrafficAnalysisSnapshotCarriesAutoSaveStatus(t *testing.T) {
	app := NewApp(AppOptions{})
	snap := app.trafficSnapshot()
	if snap == nil || snap.AutoSaveStatus != "" {
		t.Fatalf("trafficSnapshot().AutoSaveStatus = %#v, want empty", snap)
	}
	source := &fakeObs{}
	source.add(1)
	w, err := newTrafficLogWriter(t.TempDir(), source.obs)
	if err != nil {
		t.Fatalf("newTrafficLogWriter() error = %v", err)
	}
	defer w.Close(false)
	app.trafficLog.Store(w)
	snap = app.trafficSnapshot()
	if snap.AutoSaveStatus != "active" {
		t.Fatalf("trafficSnapshot().AutoSaveStatus = %q, want active", snap.AutoSaveStatus)
	}
}
