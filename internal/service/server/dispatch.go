package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/logger"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/egressobservation"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/stats"

	mbtrace "moonbridge/internal/service/trace"
)

type routingObservationContextKey struct{}

var routingObservationSequence uint64

func withRoutingObservationContext(request *http.Request, correlationKey string) *http.Request {
	key := correlationKey
	if key == "" {
		key = fmt.Sprintf("gateway-request-%d", atomic.AddUint64(&routingObservationSequence, 1))
	}
	return request.WithContext(context.WithValue(request.Context(), routingObservationContextKey{}, key))
}

func routingObservationKey(ctx context.Context) string {
	value, _ := ctx.Value(routingObservationContextKey{}).(string)
	return value
}

func (server *Server) onRequestCompleted(model, actualModel, providerKey string, startTime time.Time, usage plugin.RequestUsage, cost float64, status, errMsg string) {
	if server.pluginRegistry == nil {
		return
	}
	inputTokens := usage.NormalizedInputTokens
	outputTokens := usage.NormalizedOutputTokens
	cacheCreation := usage.NormalizedCacheCreation
	cacheRead := usage.NormalizedCacheRead
	server.pluginRegistry.OnRequestCompleted(
		&plugin.RequestContext{ModelAlias: model},
		plugin.RequestResult{
			Model:         model,
			ActualModel:   actualModel,
			ProviderKey:   providerKey,
			InputTokens:   inputTokens,
			OutputTokens:  outputTokens,
			CacheCreation: cacheCreation,
			CacheRead:     cacheRead,
			Cost:          cost,
			Duration:      time.Since(startTime),
			Status:        status,
			ErrorMessage:  errMsg,
			Usage:         usage,
		},
	)
}
func (server *Server) handleResponses(writer http.ResponseWriter, request *http.Request) {
	log := slog.Default().With("path", request.URL.Path, "method", request.Method, "remote", request.RemoteAddr)
	log.Debug("收到请求")
	requestStart := time.Now()
	// Consume the relay marker before any tracing or upstream forwarding so it
	// never appears in traces or reaches upstream. Only the value extracted here
	// may qualify a source model for lazy binding.
	relayMarker := request.Header.Get(RelayMarkerHeader)
	request.Header.Del(RelayMarkerHeader)
	requestCorrelation := request.Header.Get(RequestCorrelationHeader)
	request.Header.Del(RequestCorrelationHeader)
	if request.Method != http.MethodPost {
		log.Warn("方法不允许", "method", request.Method)
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "方法不允许",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}

	server.sessionForRequest(request)

	encoding, encodingErr := normalizeContentEncoding(request.Header.Values("Content-Encoding"))
	if encodingErr != nil {
		log.Warn("不支持的 Content-Encoding", "error", encodingErr)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "unsupported content encoding",
			Type:    "invalid_request_error",
			Code:    "unsupported_content_encoding",
		}}
		record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request), OpenAIRequest: mbtrace.RawJSONOrString(nil)}
		record.Error = traceError("unsupported_content_encoding", encodingErr)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	// Bound the encoded side for zstd only; identity/plain keeps the existing
	// unbounded read so we never add a limit to today's behavior.
	bodyReader := io.Reader(request.Body)
	if encoding == "zstd" {
		bodyReader = io.LimitReader(request.Body, maxEncodedZstdRequestBody+1)
	}
	encoded, err := io.ReadAll(bodyReader)
	record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request)}
	// A bounded read that hit the encoded cap means the zstd body exceeded it.
	if err == nil && encoding == "zstd" && len(encoded) > maxEncodedZstdRequestBody {
		err = errRequestBodyTooLarge
	}
	if err != nil {
		log.Error("读取请求体失败", "error", err)
		code := "invalid_request_body"
		message := "读取请求体失败"
		stage := "read_openai_request"
		if errors.Is(err, errRequestBodyTooLarge) {
			code = "request_too_large"
			message = "request body too large"
			stage = "decode_request_too_large"
		}
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: message,
			Type:    "invalid_request_error",
			Code:    code,
		}}
		record.Error = traceError(stage, err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	decoded, decodeErr := decodeRequestBody(encoded, encoding)
	if decodeErr != nil {
		log.Warn("请求体解码失败", "error", decodeErr)
		code := "invalid_request_body"
		message := "读取请求体失败"
		stage := "decode_request_body"
		switch {
		case errors.Is(decodeErr, errRequestBodyTooLarge):
			code = "request_too_large"
			message = "request body too large"
			stage = "decode_request_too_large"
		case errors.Is(decodeErr, errUnsupportedContentEncoding):
			code = "unsupported_content_encoding"
			message = "unsupported content encoding"
			stage = "unsupported_content_encoding"
		}
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: message,
			Type:    "invalid_request_error",
			Code:    code,
		}}
		record.Error = traceError(stage, decodeErr)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	record.OpenAIRequest = mbtrace.RawJSONOrString(decoded)

	var responsesRequest openai.ResponsesRequest
	if err := json.Unmarshal(decoded, &responsesRequest); err != nil {
		log.Warn("无效的 JSON 请求体", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "无效的 JSON 请求体",
			Type:    "invalid_request_error",
			Code:    "invalid_json",
		}}
		record.Error = traceError("decode_openai_request", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	record.Model = responsesRequest.Model
	request = withRoutingObservationContext(request, requestCorrelation)
	resolvedRoute, routingAlias, resolveErr := server.resolveModelOrFallbackContext(request.Context(), responsesRequest.Model, relayMarker, true)
	if resolveErr == nil {
		var candidateInfo string
		for i, c := range resolvedRoute.Candidates {
			if i > 0 {
				candidateInfo += ", "
			}
			candidateInfo += c.ProviderKey + "=" + c.UpstreamModel + "(p" + fmt.Sprint(i) + ")"
		}
		log.Debug("路由解析结果", "model", responsesRequest.Model, "candidates", candidateInfo)
	}
	if resolveErr != nil {
		log.Warn("请求了未知模型", "model", responsesRequest.Model)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("unknown model: %q", responsesRequest.Model),
			Type:    "invalid_request_error",
			Code:    "model_not_found",
		}}
		record.Error = traceError("model_not_found", fmt.Errorf("model %q not found", responsesRequest.Model))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusNotFound, payload)
		return
	}

	// Filter candidates by request features (e.g., image input).
	filteredCandidates, filterReason := server.filterCandidatesByInput(resolvedRoute.Candidates, responsesRequest.Input)
	if len(filteredCandidates) == 0 {
		log.Warn("过滤后无可用提供商", "model", responsesRequest.Model, "reason", filterReason)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no available provider for model %q with the requested features", responsesRequest.Model),
			Type:    "invalid_request_error",
			Code:    "provider_error",
		}}
		record.Error = traceError("provider_filtered", fmt.Errorf("candidates filtered: %s", filterReason))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		return
	}
	resolvedRoute.Candidates = filteredCandidates
	if filterReason != "" {
		log.Info("候选过滤", "model", responsesRequest.Model, "reason", filterReason)
	}

	// Protocol branch: get preferred candidate.
	preferred, ok := resolvedRoute.Preferred()
	if ok {
		log.Debug("选中提供商", "model", responsesRequest.Model, "provider", preferred.ProviderKey, "upstream", preferred.UpstreamModel)
	}
	if !ok {
		log.Error("模型解析结果无可用提供商", "model", responsesRequest.Model)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no available provider for model %q", responsesRequest.Model),
			Type:    "server_error",
			Code:    "provider_error",
		}}
		record.Error = traceError("provider_error", fmt.Errorf("no available provider for %q", responsesRequest.Model))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		return
	}
	if preferred.RoutingSlot != "" {
		server.recordRoutingResolved(request.Context(), responsesRequest.Model, preferred)
	}

	if preferred.Protocol == config.ProtocolOpenAIResponse {
		server.handleOpenAIResponse(writer, request, responsesRequest, routingAlias, resolvedRoute.Candidates, record)
		return
	}

	// Adapter dispatch path for all non-OpenAI-Response protocols.
	if server.adapterRegistry != nil {
		if _, ok := server.adapterRegistry.GetProvider(preferred.Protocol); ok {
			server.handleWithAdapters(writer, request, responsesRequest, routingAlias, resolvedRoute)
			return
		}
	}

	// No adapter path available.
	log.Error("no adapter path configured", "model", responsesRequest.Model, "protocol", preferred.Protocol)
	payload := openai.ErrorResponse{Error: openai.ErrorObject{
		Message: fmt.Sprintf("no adapter path configured for protocol %q", preferred.Protocol),
		Type:    "server_error",
		Code:    "adapter_not_configured",
	}}
	record.Error = traceError("no_adapter_path", fmt.Errorf("no adapter path"))
	record.OpenAIResponse = payload
	server.writeTrace(record)
	writeOpenAIError(writer, http.StatusInternalServerError, payload)
	server.onRequestCompleted(
		responsesRequest.Model, "", "", requestStart,
		zeroUsage("anthropic", "none"), 0, "error", "no adapter path",
	)
}
func (server *Server) writeTrace(record mbtrace.Record) {
	if server.tracer == nil || !server.tracer.Enabled() {
		return
	}
	requestNumber := server.tracer.NextRequestNumber()

	// Chat 分类：openai-chat 协议的请求/响应
	if shouldWriteChatTrace(record) {
		server.writeTraceCategory("Chat", requestNumber, mbtrace.Record{
			HTTPRequest:      record.HTTPRequest,
			Model:            record.Model,
			ChatRequest:      record.ChatRequest,
			ChatResponse:     record.ChatResponse,
			ChatStreamEvents: record.ChatStreamEvents,
			Error:            record.Error,
		})
	}

	if shouldWriteResponseTrace(record) {
		server.writeTraceCategory("Response", requestNumber, mbtrace.Record{
			HTTPRequest:        record.HTTPRequest,
			OpenAIRequest:      record.OpenAIRequest,
			Model:              record.Model,
			OpenAIResponse:     record.OpenAIResponse,
			OpenAIStreamEvents: record.OpenAIStreamEvents,
			UpstreamRequest:    record.UpstreamRequest,
			UpstreamResponse:   record.UpstreamResponse,
			Error:              record.Error,
		})
	}
	if shouldWriteAnthropicTrace(record) {
		server.writeTraceCategory("Anthropic", requestNumber, mbtrace.Record{
			HTTPRequest:           record.HTTPRequest,
			AnthropicRequest:      record.AnthropicRequest,
			Model:                 record.Model,
			AnthropicResponse:     record.AnthropicResponse,
			AnthropicStreamEvents: record.AnthropicStreamEvents,
			Error:                 record.Error,
		})
	}
}
func (server *Server) writeTraceCategory(category string, requestNumber uint64, record mbtrace.Record) {
	if _, err := server.tracer.WriteNumbered(category, requestNumber, record); err != nil && server.traceErrors != nil {
		fmt.Fprintf(server.traceErrors, "跟踪 %s 写入失败: %v\n", category, err)
	}
}
func shouldWriteResponseTrace(record mbtrace.Record) bool {
	return record.OpenAIRequest != nil || record.OpenAIResponse != nil || record.OpenAIStreamEvents != nil || record.UpstreamRequest != nil
}
func shouldWriteAnthropicTrace(record mbtrace.Record) bool {
	return record.AnthropicRequest != nil || record.AnthropicResponse != nil || record.AnthropicStreamEvents != nil
}
func shouldWriteChatTrace(record mbtrace.Record) bool {
	return record.ChatRequest != nil || record.ChatResponse != nil || record.ChatStreamEvents != nil
}

func traceError(stage string, err error) map[string]string {
	return map[string]string{"stage": stage, "message": err.Error()}
}
func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
func writeOpenAIError(writer http.ResponseWriter, status int, payload openai.ErrorResponse) {
	writeJSON(writer, status, payload)
}
func writeSSE(writer http.ResponseWriter, event openai.StreamEvent) error {
	var payload []byte
	if event.Data == nil {
		payload = []byte("{}")
	} else {
		payload, _ = json.Marshal(event.Data)
	}
	if _, err := writer.Write([]byte("event: " + event.Event + "\n")); err != nil {
		return err
	}
	if _, err := writer.Write([]byte("data: " + string(payload) + "\n\n")); err != nil {
		return err
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (server *Server) handleOpenAIResponse(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, routingAlias string, candidates []provider.ProviderCandidate, record mbtrace.Record) {
	proxyStart := time.Now()
	var hookErr string
	var lastErr error
	var lastProviderKey string
	actualModel := "" // updated with the successfully used upstream model
	pm := server.activeProviderManager()
	defer func() {
		if hookErr != "" {
			server.onRequestCompleted(
				responsesRequest.Model, actualModel, lastProviderKey, proxyStart,
				zeroUsage(config.ProtocolOpenAIResponse, "none"), 0, "error", hookErr,
			)
		}
	}()
	log := slog.Default().With("path", request.URL.Path, "method", request.Method)
	if pm == nil {
		log.Error("未配置 OpenAI Responses 直通的提供商管理器")
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "提供商路由未配置",
			Type:    "server_error",
			Code:    "internal_error",
		}}
		record.Error = map[string]string{"stage": "openai_provider_config", "message": "provider manager not configured"}
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		hookErr = "provider manager not configured"
		return
	}

	// Filter to only OpenAI-response protocol candidates.
	openaiCandidates := make([]provider.ProviderCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Protocol == config.ProtocolOpenAIResponse {
			openaiCandidates = append(openaiCandidates, c)
		}
	}
	if len(openaiCandidates) == 0 {
		log.Error("没有 OpenAI Responses 协议的提供商候选")
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "没有可用的提供商",
			Type:    "server_error",
			Code:    "provider_error",
		}}
		record.Error = map[string]string{"stage": "openai_provider_config", "message": "no openai-response candidates"}
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		hookErr = "no openai-response candidates"
		return
	}

	for i, candidate := range openaiCandidates {
		providerKey := candidate.ProviderKey
		lastProviderKey = providerKey
		isLast := i == len(openaiCandidates)-1
		log := logger.L().With("provider", providerKey, "attempt", i+1)
		if candidate.Client == nil {
			if dynamicClient, err := pm.ClientForKey(providerKey); err == nil {
				candidate.Client = dynamicClient
			}
		}

		baseURL := pm.ProviderBaseURL(providerKey)
		apiKey := pm.ProviderAPIKey(providerKey)
		if baseURL == "" {
			if isLast {
				log.Error("OpenAI 提供商缺少 base_url")
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "提供商未配置",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = map[string]string{"stage": "openai_provider_config", "message": "missing base_url"}
				record.OpenAIResponse = payload
				server.writeTrace(record)
				hookErr = "missing base_url"
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI 提供商缺少 base_url，尝试下一个候选",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1)
			lastErr = fmt.Errorf("provider %q has empty base_url", providerKey)
			continue
		}

		// Build upstream URL: baseURL + /v1/responses
		upstreamURL := strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(upstreamURL, "/v1/responses") && !strings.HasSuffix(upstreamURL, "/responses") {
			upstreamURL += "/v1/responses"
		}

		upstreamRequest := responsesRequest
		upstreamRequest.Model = candidate.UpstreamModel
		actualModel = candidate.UpstreamModel

		// Inject web_search tool if enabled for this model.
		webSearchModel := responsesRequest.Model
		if routingAlias != "" {
			webSearchModel = routingAlias
		}
		if pm.ResolvedWebSearchForModel(webSearchModel) == "enabled" {
			upstreamRequest.Tools = InjectWebSearchTool(upstreamRequest.Tools)
		}

		body, err := json.Marshal(upstreamRequest)
		if err != nil {
			if isLast {
				log.Error("序列化请求失败", "error", err)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "内部错误",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = traceError("encode_openai_upstream_request", err)
				record.OpenAIResponse = payload
				hookErr = "encode upstream request"
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusInternalServerError, payload)
				return
			}
			logger.Warn("OpenAI 请求序列化失败，尝试下一个候选",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"error", err)
			lastErr = err
			continue
		}

		// Create upstream request
		upstreamReq, err := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			if isLast {
				log.Error("创建上游请求失败", "error", err)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "上游请求失败",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = traceError("create_openai_upstream_request", err)
				hookErr = "create upstream request"
				record.OpenAIResponse = payload
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI 上游请求创建失败，尝试下一个候选",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"error", err)
			lastErr = err
			continue
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
		upstreamReq = upstreamReq.WithContext(server.withProviderEgress(upstreamReq.Context(), responsesRequest.Model, candidate, responsesRequest.Stream, &upstreamRequest))

		client := server.openAIHTTP
		if client == nil {
			client = &http.Client{Timeout: 0}
		}
		upstreamResp, err := client.Do(upstreamReq)
		if err != nil {
			if isLast {
				log.Error("OpenAI 上游请求失败",
					"request_model", responsesRequest.Model,
					"actual_model", upstreamRequest.Model,
					"error", err.Error(),
					"stage", "openai_upstream",
				)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: err.Error(),
					Type:    "server_error",
					Code:    "provider_error",
				}}
				hookErr = err.Error()
				record.Error = traceError("openai_upstream", err)
				record.OpenAIResponse = payload
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI 上游连接失败，回退到下一个候选",
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"provider", providerKey,
				"error", err,
			)
			lastErr = err
			continue
		}
		defer upstreamResp.Body.Close()

		// Log successful fallback if not on the first candidate
		if i > 0 {
			logger.Info("OpenAI 回退成功",
				"request_model", responsesRequest.Model,
				"final_provider", providerKey,
				"final_model", candidate.UpstreamModel,
				"attempt", i+1,
			)
		}

		// Copy response headers and status
		for key, values := range upstreamResp.Header {
			for _, v := range values {
				writer.Header().Add(key, v)
			}
		}
		writer.WriteHeader(upstreamResp.StatusCode)

		traceEnabled := server.tracer != nil && server.tracer.Enabled()
		usageEnabled := upstreamResp.StatusCode >= 200 && upstreamResp.StatusCode <= 299 && (server.stats != nil || server.pluginRegistry != nil)
		shouldCapture := traceEnabled || usageEnabled

		var captured bytes.Buffer
		target := io.Writer(writer)
		if shouldCapture {
			target = io.MultiWriter(writer, &captured)
		}
		if _, err := io.Copy(target, upstreamResp.Body); err != nil {
			hookErr = "copy upstream response"
			log.Error("复制上游响应失败", "error", err)
			return
		}
		if upstreamResp.StatusCode >= 200 && upstreamResp.StatusCode < 300 && (!responsesRequest.Stream || bytes.Contains(captured.Bytes(), []byte("response.completed")) || bytes.Contains(captured.Bytes(), []byte("[DONE]"))) {
			egressobservation.MarkForwarded(upstreamReq.Context())
		}

		if traceEnabled {
			record.OpenAIResponse = mbtrace.RawJSONOrString(captured.Bytes())
			server.writeTrace(record)
		}

		// Capture usage for metrics recording.
		var billingUsage stats.BillingUsage
		var metricTelemetry plugin.RequestUsage
		if usageEnabled {
			if u, raw, source, ok := openAIUsageFromResponse(captured.Bytes(), responsesRequest.Stream); ok {
				billingUsage = u.BillingUsage()
				metricTelemetry = usageFromStats(config.ProtocolOpenAIResponse, source, u, raw)
				if server.stats != nil {
					server.stats.RecordBilling(responsesRequest.Model, actualModel, billingUsage)
					logBillingUsageLine(responsesRequest.Model, actualModel, billingUsage, server.stats)
				}
			}
		}
		if metricTelemetry.Protocol == "" {
			metricTelemetry = zeroUsage(config.ProtocolOpenAIResponse, "none")
		}

		// Record metrics via plugin hooks.
		status := "success"
		errMsg := ""
		if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
			status = "error"
			errMsg = fmt.Sprintf("HTTP %d", upstreamResp.StatusCode)
		}
		cost := float64(0)
		if server.stats != nil {
			cost = computeCostWithProviderPricing(pm, server.stats, responsesRequest.Model, actualModel, providerKey, billingUsage)
		}
		server.onRequestCompleted(
			responsesRequest.Model, actualModel, providerKey, proxyStart,
			metricTelemetry,
			cost, status, errMsg,
		)

		// Record trace including final provider info
		record.Model = fmt.Sprintf("%s (%s)", responsesRequest.Model, providerKey)

		return // success
	}

	// All candidates failed
	log.Error("所有 OpenAI Responses 提供商候选均失败",
		"request_model", responsesRequest.Model,
		"candidates_count", len(openaiCandidates),
		"last_error", lastErr,
	)
	if hookErr == "" {
		hookErr = fmt.Sprintf("all %d candidates failed: %v", len(openaiCandidates), lastErr)
	}
}
