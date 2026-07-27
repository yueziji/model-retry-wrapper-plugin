package main

/*
#include "bridge.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginIdentifier = "model-retry-wrapper"

var pluginVersion = "dev"

var errPluginStreamClosed = errors.New("plugin stream closed")

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type rpcHostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	if uint32(host.abi_version) != pluginabi.ABIVersion {
		return 2
	}
	pluginLifecycle.reopen()
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	methodName := ""
	if method != nil {
		methodName = C.GoString(method)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			message := fmt.Sprintf("panic handling %s: %v", methodName, recovered)
			pluginLog("", "error", "model-retry-wrapper: plugin call panic", map[string]any{
				"method": methodName,
				"panic":  fmt.Sprint(recovered),
				"stack":  string(debug.Stack()),
			})
			writeResponse(response, errorEnvelope("plugin_panic", message))
			rc = 1
		}
	}()
	if methodName == "" {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(methodName, requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelopeForError("plugin_error", errHandle))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	pluginLifecycle.shutdown(closeHostModelStream)
	C.store_host_api(nil)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case pluginabi.MethodExecutorHTTPRequest:
		return errorEnvelopeWithStatus("not_supported", "executor.http_request is not supported by this retry wrapper", http.StatusNotImplemented), nil
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginIdentifier,
			Version:          pluginVersion,
			Author:           "yueziji",
			GitHubRepository: "https://github.com/yueziji/model-retry-wrapper-plugin",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Client-requested model names or aliases wrapped by this retry executor."},
				{Name: "source_formats", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional inbound protocol filter such as openai, openai-response, claude, or gemini."},
				{Name: "status_codes", Type: pluginapi.ConfigFieldTypeArray, Description: "HTTP status codes retried inside the plugin before downstream delivery."},
				{Name: "retry_keywords", Type: pluginapi.ConfigFieldTypeArray, Description: "Fallback error substrings; an explicit empty list disables keyword retries."},
				{Name: "max_attempts", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum attempts, including the first try. 0 removes only the attempt-count cap."},
				{Name: "initial_delay_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Positive initial delay before a retry."},
				{Name: "max_delay_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Positive maximum exponential backoff delay."},
				{Name: "max_elapsed_time_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Wall-clock limit for one retry sequence. 0 removes the time limit."},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  supportedExecutorFormats(),
			ExecutorOutputFormats: supportedExecutorFormats(),
		},
	}
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	routeFields := map[string]any{
		"requested_model":          req.RequestedModel,
		"source_format":            req.SourceFormat,
		"normalized_source_format": normalizeSourceFormat(req.SourceFormat),
		"enabled":                  cfg.Enabled,
		"models":                   cfg.Models,
		"source_formats":           cfg.SourceFormats,
	}
	if !shouldRoute(cfg, req.SourceFormat, req.RequestedModel) {
		routeFields["reason"] = routeSkipReason(cfg, req.SourceFormat, req.RequestedModel)
		pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: route skipped", routeFields)
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "model_not_configured"})
	}
	pluginLog(req.HostCallbackID, "info", "model-retry-wrapper: route matched", routeFields)
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "model_retry_wrapper",
	})
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: execute received", map[string]any{
		"model":          req.Model,
		"source_format":  req.SourceFormat,
		"format":         req.Format,
		"entry_protocol": entryProtocol(req.ExecutorRequest),
		"exit_protocol":  exitProtocol(req.ExecutorRequest),
		"stream":         false,
	})
	ctx, errContext := pluginLifecycle.context()
	if errContext != nil {
		return nil, retryStatusError{status: http.StatusServiceUnavailable, err: errContext}
	}
	resp, errRun := runModelExecuteWithRetry(ctx, req)
	if errRun != nil {
		pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: execute failed", map[string]any{
			"model": req.Model,
			"error": shortError(errRun),
		})
		return nil, errRun
	}
	pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: execute completed", map[string]any{"model": req.Model})
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: stream execute received", map[string]any{
		"model":          req.Model,
		"source_format":  req.SourceFormat,
		"format":         req.Format,
		"entry_protocol": entryProtocol(req.ExecutorRequest),
		"exit_protocol":  exitProtocol(req.ExecutorRequest),
		"stream":         true,
	})
	ctx, errContext := pluginLifecycle.beginStreamTask()
	if errContext != nil {
		return errorEnvelopeForError("plugin_shutting_down", retryStatusError{status: http.StatusServiceUnavailable, err: errContext}), nil
	}
	go func() {
		defer pluginLifecycle.endStreamTask()
		defer func() {
			if recovered := recover(); recovered != nil {
				message := fmt.Sprintf("plugin stream panic: %v", recovered)
				pluginLog(req.HostCallbackID, "error", "model-retry-wrapper: stream panic", map[string]any{
					"model": req.Model,
					"panic": fmt.Sprint(recovered),
					"stack": string(debug.Stack()),
				})
				closePluginStream(req.StreamID, message)
			}
		}()
		errRun := runModelStreamWithRetry(ctx, req)
		if errRun != nil {
			if errors.Is(errRun, errPluginStreamClosed) {
				pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: stream canceled", map[string]any{
					"model": req.Model,
					"error": shortError(errRun),
				})
				return
			}
			pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: stream failed", map[string]any{
				"model": req.Model,
				"error": shortError(errRun),
			})
			closePluginStream(req.StreamID, errRun.Error())
			return
		}
		pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: stream completed", map[string]any{"model": req.Model})
		closePluginStream(req.StreamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{streamContentType(req.SourceFormat)}},
	})
}

func retryLogFields(req rpcExecutorRequest, cfg pluginConfig, attempt int, status int, stream bool) map[string]any {
	return map[string]any{
		"model":               req.Model,
		"entry_protocol":      entryProtocol(req.ExecutorRequest),
		"exit_protocol":       exitProtocol(req.ExecutorRequest),
		"source_format":       req.SourceFormat,
		"format":              req.Format,
		"stream":              stream,
		"attempt":             attempt,
		"status":              status,
		"retryable_status":    shouldRetryStatus(cfg, status),
		"max_attempts":        cfg.MaxAttempts,
		"max_elapsed_time_ms": cfg.MaxElapsedMS,
		"status_codes":        []int(cfg.StatusCodes),
		"retry_keywords":      cfg.RetryKeywords,
	}
}

func runModelExecuteWithRetry(ctx context.Context, req rpcExecutorRequest) (pluginapi.HostModelExecutionResponse, error) {
	cfg := loadedConfig()
	ctx, cancel := retryContext(ctx, cfg)
	defer cancel()
	pluginLog(req.HostCallbackID, "info", "model-retry-wrapper: retry executor start", retryLogFields(req, cfg, 0, 0, false))
	var lastErr error
	for attempt := 1; ; attempt++ {
		if errContext := ctx.Err(); errContext != nil {
			return pluginapi.HostModelExecutionResponse{}, retryTerminationError(lastErr, errContext)
		}
		resp, status, errCall := callHostModelExecute(req)
		if errCall == nil && !shouldRetryStatus(cfg, status) {
			pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: attempt completed without retry", retryLogFields(req, cfg, attempt, status, false))
			return resp, nil
		}
		if errCall != nil {
			status = statusFromError(errCall)
			lastErr = errorWithStatus(errCall, status)
		} else {
			lastErr = retryStatusError{status: status}
		}
		fields := retryLogFields(req, cfg, attempt, status, false)
		if errCall != nil {
			fields["error"] = shortError(errCall)
		}
		shouldRetry, retryKeyword := shouldRetryFailure(cfg, attempt, status, errCall)
		if retryKeyword != "" {
			fields["retry_keyword"] = retryKeyword
		}
		if !shouldRetry {
			pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: retry stopped", fields)
			return pluginapi.HostModelExecutionResponse{}, lastErr
		}
		delay := retryDelay(cfg, attempt)
		fields["delay_ms"] = durationMillis(delay)
		pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: retrying request", fields)
		if errWait := waitRetryDelay(ctx, delay); errWait != nil {
			pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: retry wait canceled", logFieldsWith(fields, "error", shortError(errWait)))
			return pluginapi.HostModelExecutionResponse{}, retryTerminationError(lastErr, errWait)
		}
	}
}

func runModelStreamWithRetry(ctx context.Context, req rpcExecutorRequest) error {
	cfg := loadedConfig()
	ctx, cancel := retryContext(ctx, cfg)
	defer cancel()
	pluginLog(req.HostCallbackID, "info", "model-retry-wrapper: stream retry executor start", retryLogFields(req, cfg, 0, 0, true))
	var lastErr error
	for attempt := 1; ; attempt++ {
		if errContext := ctx.Err(); errContext != nil {
			return retryTerminationError(lastErr, errContext)
		}
		if errProbe := probePluginStreamOpen(req.StreamID); errProbe != nil {
			return errProbe
		}
		status, firstPayload, streamID, errStart := startHostModelStream(req)
		if errStart == nil && !shouldRetryStatus(cfg, status) {
			fields := retryLogFields(req, cfg, attempt, status, true)
			fields["first_payload"] = len(firstPayload) > 0
			pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: stream startup completed without retry", fields)
			return forwardHostStream(ctx, streamID, firstPayload, req)
		}
		if streamID != "" {
			_ = closeTrackedHostModelStream(streamID)
		}
		if errStart != nil {
			status = statusFromError(errStart)
			lastErr = errorWithStatus(errStart, status)
		} else {
			lastErr = retryStatusError{status: status}
		}
		fields := retryLogFields(req, cfg, attempt, status, true)
		if errStart != nil {
			fields["error"] = shortError(errStart)
		}
		shouldRetry, retryKeyword := shouldRetryFailure(cfg, attempt, status, errStart)
		if retryKeyword != "" {
			fields["retry_keyword"] = retryKeyword
		}
		if !shouldRetry {
			pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: stream retry stopped", fields)
			return lastErr
		}
		delay := retryDelay(cfg, attempt)
		fields["delay_ms"] = durationMillis(delay)
		pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: retrying stream startup", fields)
		if errWait := waitRetryDelayWithProbe(ctx, delay, func() error {
			return probePluginStreamOpen(req.StreamID)
		}); errWait != nil {
			pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: stream retry wait canceled", logFieldsWith(fields, "error", shortError(errWait)))
			return retryTerminationError(lastErr, errWait)
		}
	}
}

func callHostModelExecute(req rpcExecutorRequest) (pluginapi.HostModelExecutionResponse, int, error) {
	raw, errCall := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: entryProtocol(req.ExecutorRequest),
			ExitProtocol:  exitProtocol(req.ExecutorRequest),
			Model:         req.Model,
			Stream:        false,
			Body:          requestBody(req.ExecutorRequest),
			Headers:       cloneHeader(req.Headers),
			Query:         cloneValues(req.Query),
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if errCall != nil {
		return pluginapi.HostModelExecutionResponse{}, statusFromError(errCall), errCall
	}
	var resp pluginapi.HostModelExecutionResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HostModelExecutionResponse{}, 0, errDecode
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return resp, resp.StatusCode, retryStatusError{status: resp.StatusCode}
	}
	return resp, resp.StatusCode, nil
}

func startHostModelStream(req rpcExecutorRequest) (int, []byte, string, error) {
	raw, errCall := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: entryProtocol(req.ExecutorRequest),
			ExitProtocol:  exitProtocol(req.ExecutorRequest),
			Model:         req.Model,
			Stream:        true,
			Body:          requestBody(req.ExecutorRequest),
			Headers:       cloneHeader(req.Headers),
			Query:         cloneValues(req.Query),
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if errCall != nil {
		return statusFromError(errCall), nil, "", errCall
	}
	var resp pluginapi.HostModelStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return 0, nil, "", errDecode
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return resp.StatusCode, nil, resp.StreamID, retryStatusError{status: resp.StatusCode}
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return 0, nil, "", fmt.Errorf("host model stream returned empty stream_id")
	}
	if errTrack := pluginLifecycle.trackHostStream(resp.StreamID); errTrack != nil {
		_ = closeHostModelStream(resp.StreamID)
		return 0, nil, "", errTrack
	}
	for {
		chunk, errRead := readHostModelStream(resp.StreamID)
		if errRead != nil {
			return statusFromError(errRead), nil, resp.StreamID, errRead
		}
		if chunk.Error != "" {
			errChunk := fmt.Errorf("%s", chunk.Error)
			return statusFromError(errChunk), nil, resp.StreamID, errChunk
		}
		if len(chunk.Payload) > 0 {
			return http.StatusOK, chunk.Payload, resp.StreamID, nil
		}
		if chunk.Done {
			return http.StatusOK, nil, resp.StreamID, nil
		}
	}
}

func forwardHostStream(ctx context.Context, hostStreamID string, firstPayload []byte, req rpcExecutorRequest) error {
	defer func() { _ = closeTrackedHostModelStream(hostStreamID) }()
	if len(firstPayload) > 0 {
		if errEmit := emitPluginStreamChunk(req.StreamID, firstPayload); errEmit != nil {
			logStreamForwardError(req, "model-retry-wrapper: plugin stream emit failed", "first_payload", hostStreamID, errEmit)
			return errEmit
		}
	}
	for {
		chunk, errRead := readHostModelStream(hostStreamID)
		if errRead != nil {
			logStreamForwardError(req, "model-retry-wrapper: host stream read failed", "read", hostStreamID, errRead)
			return errRead
		}
		if chunk.Error != "" {
			errChunk := fmt.Errorf("%s", chunk.Error)
			logStreamForwardError(req, "model-retry-wrapper: host stream returned error", "error_chunk", hostStreamID, errChunk)
			return errChunk
		}
		if len(chunk.Payload) > 0 {
			if errEmit := emitPluginStreamChunk(req.StreamID, chunk.Payload); errEmit != nil {
				logStreamForwardError(req, "model-retry-wrapper: plugin stream emit failed", "payload", hostStreamID, errEmit)
				return errEmit
			}
		}
		if chunk.Done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func logStreamForwardError(req rpcExecutorRequest, message string, source string, hostStreamID string, err error) {
	level := "warn"
	if errors.Is(err, errPluginStreamClosed) {
		level = "debug"
	}
	pluginLog(req.HostCallbackID, level, message, map[string]any{
		"model":            req.Model,
		"source":           source,
		"error":            shortError(err),
		"host_stream_id":   hostStreamID,
		"plugin_stream_id": req.StreamID,
	})
}

func readHostModelStream(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: streamID})
	if errCall != nil {
		return pluginapi.HostModelStreamReadResponse{}, errCall
	}
	var resp pluginapi.HostModelStreamReadResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HostModelStreamReadResponse{}, errDecode
	}
	return resp, nil
}

func closeHostModelStream(streamID string) error {
	_, errCall := callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	return errCall
}

func closeTrackedHostModelStream(streamID string) error {
	errClose := closeHostModelStream(streamID)
	pluginLifecycle.untrackHostStream(streamID)
	return errClose
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: streamID, Payload: payload})
	if errCall != nil {
		return fmt.Errorf("%w: %v", errPluginStreamClosed, errCall)
	}
	return nil
}

func probePluginStreamOpen(streamID string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: streamID})
	if errCall != nil {
		return fmt.Errorf("%w: %v", errPluginStreamClosed, errCall)
	}
	return nil
}

func closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMsg)})
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			if env.Error.HTTPStatus > 0 {
				return nil, retryStatusError{status: env.Error.HTTPStatus, err: fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)}
			}
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	return errorEnvelopeWithStatus(code, message, 0)
}

func errorEnvelopeForError(code string, err error) []byte {
	if err == nil {
		return errorEnvelope(code, "plugin call failed")
	}
	return errorEnvelopeWithStatus(code, err.Error(), structuredStatusFromError(err))
}

func errorEnvelopeWithStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code:       code,
		Message:    message,
		HTTPStatus: status,
	}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	if response.ptr != nil {
		C.free(response.ptr)
		response.ptr = nil
		response.len = 0
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
