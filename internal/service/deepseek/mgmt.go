package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/service/configgraph"
)

// ManagementAPI is the subset of the gateway config management API the
// DeepSeek service drives. Base revisions are required on every mutation and
// the response's graph carries the new revision forward.
type ManagementAPI interface {
	Graph(ctx context.Context) (configgraph.Graph, error)
	Patch(ctx context.Context, req configgraph.PatchRequest) (configgraph.PatchResponse, error)
	CreateResource(ctx context.Context, baseRevision string, kind configgraph.ResourceKind, id string, value map[string]any) (configgraph.PatchResponse, error)
	// Effective returns the gateway's current effective file config (secrets
	// masked by the server). It is the source of truth for codex config
	// derivation; the desktop re-injects the live server token.
	Effective(ctx context.Context) (config.FileConfig, error)
}

// HTTPClient talks to the in-process gateway's management API over loopback.
// Token is the per-run control token sent as a Bearer credential; the server
// swaps it for the run's server token on forwarding.
type HTTPClient struct {
	BaseURL string // management API origin, e.g. http://127.0.0.1:38440
	Token   string
	HTTP    *http.Client
}

func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type createResourceRequest struct {
	BaseRevision string         `json:"baseRevision"`
	ID           string         `json:"id"`
	Value        map[string]any `json:"value"`
}

func (c *HTTPClient) Graph(ctx context.Context) (configgraph.Graph, error) {
	data, status, err := c.do(ctx, http.MethodGet, "/api/v1/config/graph", nil)
	if err != nil {
		return configgraph.Graph{}, err
	}
	if status != http.StatusOK {
		return configgraph.Graph{}, fmt.Errorf("deepseek: get config graph failed with status %d", status)
	}
	var graph configgraph.Graph
	if err := json.Unmarshal(data, &graph); err != nil {
		return configgraph.Graph{}, fmt.Errorf("deepseek: decode config graph: %w", err)
	}
	return graph, nil
}

func (c *HTTPClient) Effective(ctx context.Context) (config.FileConfig, error) {
	data, status, err := c.do(ctx, http.MethodGet, "/api/v1/config/effective", nil)
	if err != nil {
		return config.FileConfig{}, err
	}
	if status != http.StatusOK {
		return config.FileConfig{}, fmt.Errorf("deepseek: get effective config failed with status %d", status)
	}
	var fc config.FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return config.FileConfig{}, fmt.Errorf("deepseek: decode effective config: %w", err)
	}
	return fc, nil
}

func (c *HTTPClient) Patch(ctx context.Context, req configgraph.PatchRequest) (configgraph.PatchResponse, error) {
	data, status, err := c.do(ctx, http.MethodPatch, "/api/v1/config/graph", req)
	if err != nil {
		return configgraph.PatchResponse{}, err
	}
	return decodePatchResponse(data, status)
}

func (c *HTTPClient) CreateResource(ctx context.Context, baseRevision string, kind configgraph.ResourceKind, id string, value map[string]any) (configgraph.PatchResponse, error) {
	data, status, err := c.do(ctx, http.MethodPost, "/api/v1/config/resources/"+string(kind), createResourceRequest{
		BaseRevision: baseRevision,
		ID:           id,
		Value:        value,
	})
	if err != nil {
		return configgraph.PatchResponse{}, err
	}
	return decodePatchResponse(data, status)
}

// decodePatchResponse maps the server status codes to the patch result: 200 for
// committed/restartRequired, 409 for revision conflict, 400 for rejected. All
// three carry a PatchResponse body.
func decodePatchResponse(data []byte, status int) (configgraph.PatchResponse, error) {
	if status == http.StatusOK || status == http.StatusConflict || status == http.StatusBadRequest {
		var resp configgraph.PatchResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return resp, fmt.Errorf("deepseek: decode patch response: %w (status %d)", err, status)
		}
		return resp, nil
	}
	return configgraph.PatchResponse{}, fmt.Errorf("deepseek: management API failed with status %d", status)
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	if !isLoopbackBaseURL(c.BaseURL) {
		return nil, 0, fmt.Errorf("deepseek: refusing to send control token to non-loopback management API %q", c.BaseURL)
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return data, resp.StatusCode, nil
}

func isLoopbackBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
