package trafficanalysis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// managementPathPrefix matches the prefix desktopcontrol.Wrap routes to the
// authenticated traffic-analysis endpoints.
const captureManagementPathPrefix = "/api/v1/system/traffic-analysis/"

// managementHandler returns the HTTP handler that the desktopcontrol surface
// and the external management API mount. Every request is routed to this
// Service's operations, so the external surface and any in-process Desktop
// path reach the same proxy, state, and observations. Mode guards (desktop
// managed, recovery) are enforced by the Service operations.
func (s *Service) managementHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == captureManagementPathPrefix+"status":
			writeServiceJSON(w, http.StatusOK, s.Status())
		case r.Method == http.MethodGet && r.URL.Path == captureManagementPathPrefix+"observations":
			after := parseAfter(r.URL.Query().Get("after"))
			items, dropped := s.Observations(after)
			writeServiceJSON(w, http.StatusOK, map[string]any{"observations": items, "dropped": dropped})
		case r.Method == http.MethodPost && r.URL.Path == captureManagementPathPrefix+"start":
			st, err := s.StartCapture(startOptionsFromRequest(r))
			if err != nil {
				writeServiceError(w, err, http.StatusConflict)
				return
			}
			writeServiceJSON(w, http.StatusAccepted, st)
		case r.Method == http.MethodPost && r.URL.Path == captureManagementPathPrefix+"pause":
			ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
			st, err := s.PauseCapture(ctx)
			cancel()
			if err != nil {
				writeServiceError(w, err, http.StatusConflict)
				return
			}
			writeServiceJSON(w, http.StatusAccepted, st)
		case r.Method == http.MethodPost && r.URL.Path == captureManagementPathPrefix+"stop":
			ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
			st, err := s.StopCapture(ctx)
			cancel()
			if err != nil {
				writeServiceError(w, err, http.StatusGatewayTimeout)
				return
			}
			writeServiceJSON(w, http.StatusAccepted, st)
		case r.Method == http.MethodPost && r.URL.Path == captureManagementPathPrefix+"clear":
			if err := s.Clear(); err != nil {
				writeServiceError(w, err, http.StatusConflict)
				return
			}
			writeServiceJSON(w, http.StatusOK, s.Status())
		default:
			http.NotFound(w, r)
		}
	})
}

// startOptionsFromRequest builds a StartOptions from a management start
// request. The external start command does not carry a gateway token (that is
// an internal/Desktop path concern); it binds to the default loopback capture
// address with the default upstream. This preserves the existing management
// start contract while ownership is resolved by the Service (which in 4C is
// idle or capture_only).
func startOptionsFromRequest(r *http.Request) StartOptions {
	opts := StartOptions{
		ListenAddr: DefaultCaptureAddress,
	}
	// If the request carries a JSON body with a listen_addr, use it instead
	// of the default. This allows tests (and future callers) to bind to a
	// unique port rather than the fixed 38441.
	if r.Body != nil && r.Header.Get("Content-Type") == "application/json" {
		var body struct {
			ListenAddr string `json:"listen_addr"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) == nil && body.ListenAddr != "" {
			opts.ListenAddr = body.ListenAddr
		}
	}
	return opts
}

// writeServiceError maps a Service Error to an HTTP status and a sanitized
// JSON body carrying only the kind and message.
func writeServiceError(w http.ResponseWriter, err error, defaultStatus int) {
	kind := "capture_failed"
	if se, ok := err.(*Error); ok {
		kind = string(se.Kind)
	}
	writeServiceJSON(w, defaultStatus, map[string]string{"code": kind, "message": err.Error()})
}

func writeServiceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
