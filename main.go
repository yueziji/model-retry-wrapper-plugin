package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginIdentifier = "model-retry-wrapper"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
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

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
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
func cliproxyPluginShutdown() {}

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
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginIdentifier,
			Version:          "0.1.0",
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Client-requested model names or aliases wrapped by this retry executor."},
				{Name: "source_formats", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional inbound protocol filter such as openai, claude, or gemini."},
				{Name: "status_codes", Type: pluginapi.ConfigFieldTypeArray, Description: "HTTP status codes retried inside the plugin before downstream delivery."},
				{Name: "max_attempts", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum attempts, including the first try. 0 means retry until the client cancels."},
				{Name: "initial_delay_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Initial delay before a retry."},
				{Name: "max_delay_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum exponential backoff delay."},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai", "claude", "gemini", "chat-completions"},
			ExecutorOutputFormats: []string{"openai", "claude", "gemini", "chat-completions"},
		},
	}
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !shouldRoute(cfg, req.SourceFormat, req.RequestedModel) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "model_not_configured"})
	}
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
	resp, errRun := runModelExecuteWithRetry(context.Background(), req)
	if errRun != nil {
		return nil, errRun
	}
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
	go func() {
		errRun := runModelStreamWithRetry(context.Background(), req)
		if errRun != nil {
			closePluginStream(req.StreamID, errRun.Error())
			return
		}
		closePluginStream(req.StreamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{streamContentType(req.SourceFormat)}},
	})
}

func runModelExecuteWithRetry(ctx context.Context, req rpcExecutorRequest) (pluginapi.HostModelExecutionResponse, error) {
	cfg := loadedConfig()
	var lastErr error
	for attempt := 1; ; attempt++ {
		resp, status, errCall := callHostModelExecute(req)
		if errCall == nil && !shouldRetryStatus(cfg, status) {
			return resp, nil
		}
		if errCall != nil {
			status = statusFromError(errCall)
			lastErr = errCall
		} else {
			lastErr = retryStatusError{status: status}
		}
		if !shouldRetryAttempt(cfg, attempt, status) {
			return pluginapi.HostModelExecutionResponse{}, lastErr
		}
		if errWait := waitRetryDelay(ctx, retryDelay(cfg, attempt)); errWait != nil {
			return pluginapi.HostModelExecutionResponse{}, errWait
		}
	}
}

func runModelStreamWithRetry(ctx context.Context, req rpcExecutorRequest) error {
	cfg := loadedConfig()
	var lastErr error
	for attempt := 1; ; attempt++ {
		status, firstPayload, streamID, errStart := startHostModelStream(req)
		if errStart == nil && !shouldRetryStatus(cfg, status) {
			return forwardHostStream(ctx, streamID, firstPayload, req.StreamID)
		}
		if streamID != "" {
			_ = closeHostModelStream(streamID)
		}
		if errStart != nil {
			status = statusFromError(errStart)
			lastErr = errStart
		} else {
			lastErr = retryStatusError{status: status}
		}
		if !shouldRetryAttempt(cfg, attempt, status) {
			return lastErr
		}
		if errWait := waitRetryDelay(ctx, retryDelay(cfg, attempt)); errWait != nil {
			return errWait
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

func forwardHostStream(ctx context.Context, hostStreamID string, firstPayload []byte, pluginStreamID string) error {
	defer func() { _ = closeHostModelStream(hostStreamID) }()
	if len(firstPayload) > 0 {
		if errEmit := emitPluginStreamChunk(pluginStreamID, firstPayload); errEmit != nil {
			return errEmit
		}
	}
	for {
		chunk, errRead := readHostModelStream(hostStreamID)
		if errRead != nil {
			return errRead
		}
		if chunk.Error != "" {
			return fmt.Errorf("%s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if errEmit := emitPluginStreamChunk(pluginStreamID, chunk.Payload); errEmit != nil {
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

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: streamID, Payload: payload})
	return errCall
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
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
