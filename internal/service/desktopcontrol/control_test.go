package desktopcontrol

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWrapStatusRequiresLoopbackAndToken(t *testing.T) {
	control := New("instance-1", "secret", nil).WithTrafficAnalysis(http.NotFoundHandler())
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler := Wrap(next, control)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/system/status", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", response.Code, http.StatusOK)
	}
	var status struct {
		APIVersion   int      `json:"api_version"`
		InstanceID   string   `json:"instance_id"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.APIVersion != 2 || status.InstanceID != "instance-1" {
		t.Fatalf("status identity = %+v", status)
	}
	for _, required := range []string{"traffic-analysis", "traffic-analysis-pause", "traffic-analysis-passthrough", "traffic-analysis-final-stop"} {
		found := false
		for _, capability := range status.Capabilities {
			if capability == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing capability %q: %v", required, status.Capabilities)
		}
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/system/status", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status code = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestWrapShutdownIsIdempotent(t *testing.T) {
	var calls atomic.Int32
	control := New("instance-1", "secret", func() { calls.Add(1) })
	handler := Wrap(http.NotFoundHandler(), control)
	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/system/shutdown", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("shutdown status code = %d, want %d", response.Code, http.StatusAccepted)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("shutdown calls = %d, want 1", calls.Load())
	}
}

func TestWrapForwardsDesktopTokenAsServerToken(t *testing.T) {
	control := New("instance-1", "desktop-secret", nil).WithServerToken("server-secret")
	handler := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer server-secret" {
			t.Fatalf("forwarded authorization = %q, want server token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}), control)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/config/graph", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer desktop-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestWrapRoutesTrafficAnalysisOnlyWithDesktopAuthentication(t *testing.T) {
	control := New("instance-1", "desktop-secret", nil).WithTrafficAnalysis(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler := Wrap(http.NotFoundHandler(), control)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/system/traffic-analysis/status", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer desktop-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("authenticated traffic-analysis status = %d, want %d", response.Code, http.StatusNoContent)
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/system/traffic-analysis/status", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated traffic-analysis status = %d, want %d", response.Code, http.StatusForbidden)
	}
}
