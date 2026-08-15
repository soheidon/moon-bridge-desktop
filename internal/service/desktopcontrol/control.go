package desktopcontrol

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"moonbridge/internal/config"
)

const APIVersion = 2

// Control contains the process identity and lifecycle hooks exposed to the
// Desktop shell. It is intentionally independent from Moon Bridge's normal
// management API so it works in every server mode.
type Control struct {
	InstanceID            string
	Token                 string
	ServerToken           string
	PID                   int
	StartedAt             time.Time
	trafficAnalysis       http.Handler
	trafficStatus         func() any
	routingResolverStatus func() any
	runtimeConfiguration  func() any

	routingProfileRefresh func(cfg config.Config)

	shutdownOnce sync.Once
	shutdown     func()
}

func (c *Control) WithServerToken(token string) *Control {
	c.ServerToken = token
	return c
}

// WithTrafficAnalysis attaches the authenticated Capture management handler.
// The Capture listener itself remains separate from the main management port.
func (c *Control) WithTrafficAnalysis(handler http.Handler) *Control {
	c.trafficAnalysis = handler
	return c
}

// WithTrafficAnalysisStatus exposes the capture identity and state through
// the authenticated system status endpoint without coupling desktopcontrol
// to the traffic-analysis package.
func (c *Control) WithTrafficAnalysisStatus(status func() any) *Control {
	c.trafficStatus = status
	return c
}

// WithRoutingResolverStatus exposes only the reduced, secret-safe resolver
// lifecycle state through the authenticated system status endpoint.
func (c *Control) WithRoutingResolverStatus(status func() any) *Control {
	c.routingResolverStatus = status
	return c
}

// WithRuntimeConfiguration exposes the effective, secret-safe runtime
// configuration through the authenticated system status endpoint.
func (c *Control) WithRuntimeConfiguration(status func() any) *Control {
	c.runtimeConfiguration = status
	return c
}

// WithRoutingProfileRefresh registers a callback that rebuilds the Gateway's
// runtime SlotResolver from a fresh config snapshot. Called after a routing
// profile mutation succeeds so the next request uses the updated profile.
func (c *Control) WithRoutingProfileRefresh(refresh func(cfg config.Config)) *Control {
	c.routingProfileRefresh = refresh
	return c
}

// RefreshRoutingProfile triggers the registered routing profile resolver
// rebuild callback. No-op when no callback is registered.
func (c *Control) RefreshRoutingProfile(cfg config.Config) {
	if c.routingProfileRefresh != nil {
		c.routingProfileRefresh(cfg)
	}
}

func New(instanceID, token string, shutdown func()) *Control {
	return &Control{
		InstanceID: instanceID,
		Token:      token,
		PID:        os.Getpid(),
		StartedAt:  time.Now().UTC(),
		shutdown:   shutdown,
	}
}

// Wrap places the Desktop-only endpoints in front of the regular server
// handler. The endpoints are available only from loopback and require the
// per-process bearer token supplied by the Tauri shell.
func Wrap(next http.Handler, control *Control) http.Handler {
	if control == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/system/traffic-analysis/") {
			if !isLoopback(r.RemoteAddr) || !validBearer(r, control.Token) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if control.trafficAnalysis == nil {
				http.NotFound(w, r)
				return
			}
			control.trafficAnalysis.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/api/v1/system/status" && r.URL.Path != "/api/v1/system/shutdown" {
			if strings.HasPrefix(r.URL.Path, "/api/v1/") && isLoopback(r.RemoteAddr) && validBearer(r, control.Token) {
				forwarded := r.Clone(r.Context())
				forwarded.Header = r.Header.Clone()
				if control.ServerToken == "" {
					forwarded.Header.Del("Authorization")
				} else {
					forwarded.Header.Set("Authorization", "Bearer "+control.ServerToken)
				}
				next.ServeHTTP(w, forwarded)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !isLoopback(r.RemoteAddr) || !validBearer(r, control.Token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		switch {
		case r.URL.Path == "/api/v1/system/status" && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{
				"status":                "ok",
				"desktop_mode":          true,
				"instance_id":           control.InstanceID,
				"pid":                   control.PID,
				"started_at":            control.StartedAt,
				"api_version":           APIVersion,
				"capabilities":          append([]string{"config_init", "instance_identity", "graceful_shutdown"}, trafficCapability(control)...),
				"capture":               trafficStatus(control),
				"routing_resolver":      routingResolverStatus(control),
				"runtime_configuration": runtimeConfiguration(control),
			})
		case r.URL.Path == "/api/v1/system/shutdown" && r.Method == http.MethodPost:
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "shutting_down"})
			control.shutdownOnce.Do(func() {
				if control.shutdown != nil {
					control.shutdown()
				}
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func trafficCapability(control *Control) []string {
	if control.trafficAnalysis == nil {
		return nil
	}
	return []string{"traffic_analysis", "traffic-analysis", "traffic-analysis-pause", "traffic-analysis-passthrough", "traffic-analysis-final-stop"}
}

func trafficStatus(control *Control) any {
	if control.trafficStatus == nil {
		return nil
	}
	return control.trafficStatus()
}

func routingResolverStatus(control *Control) any {
	if control.routingResolverStatus == nil {
		return nil
	}
	return control.routingResolverStatus()
}

func runtimeConfiguration(control *Control) any {
	if control.runtimeConfiguration == nil {
		return nil
	}
	return control.runtimeConfiguration()
}

func validBearer(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "desktop control response error:", err)
	}
}
