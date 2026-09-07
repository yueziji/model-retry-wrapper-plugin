package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCodexExecutorRegistrationAndRequestPreservation(t *testing.T) {
	registration := pluginRegistration()
	for _, formats := range [][]string{registration.Capabilities.ExecutorInputFormats, registration.Capabilities.ExecutorOutputFormats} {
		if !stringListContains(formats, "codex") {
			t.Fatal("CodexComp requires native codex input and output capability")
		}
	}
	if got := diagnosticProtocol("codex"); got != "codex" {
		t.Fatalf("diagnostic protocol = %q", got)
	}
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"test-state"}],"prompt_cache_key":"test-cache","include":["reasoning.encrypted_content"]}`)
	for _, source := range []string{"codex", "openai-response"} {
		req := pluginapi.ExecutorRequest{Format: "codex", SourceFormat: source, OriginalRequest: body}
		if entryProtocol(req) != source || exitProtocol(req) != "codex" {
			t.Fatalf("protocol pair was changed for %s", source)
		}
		if !bytes.Equal(requestBody(req), body) {
			t.Fatal("reasoning replay and prompt cache input must pass through unchanged")
		}
	}
}

func TestRetryMarkerPreservesOriginalHeaders(t *testing.T) {
	headers := http.Header{
		"X-Cpa-Session-Id":                    []string{"test-session"},
		strings.ToLower(retryRequestIDHeader): []string{"test-request"},
		strings.ToLower(retryAppliedHeader):   []string{"0"},
	}
	original := cloneHeader(headers)
	nested := nestedRequestHeaders(headers)
	if !reflect.DeepEqual(headers, original) {
		t.Fatal("marking a callback modified the caller's headers")
	}
	if !hasRetryMarker(nested) || requestIDFromHeaders(nested) != "" || nested.Get("X-CPA-Session-Id") != "test-session" {
		t.Fatal("callback headers must carry the marker and session, without the lifecycle request ID")
	}
	nested.Set("X-CPA-Session-Id", "changed")
	if headers.Get("X-CPA-Session-Id") != "test-session" {
		t.Fatal("callback header values alias the original request")
	}
	if hasRetryMarker(headers) || hasRetryMarker(nil) || !hasRetryMarker(nestedRequestHeaders(nil)) {
		t.Fatal("the marker must be scoped to the nested request")
	}
	if !hasRetryMarker(http.Header{strings.ToLower(retryAppliedHeader): []string{"0", " 1 "}}) {
		t.Fatal("marker lookup must accept case-insensitive header names")
	}
}

func TestRetryRouterSkipsCodexCompCallbacks(t *testing.T) {
	previousConfig := loadedConfig()
	previousSchema := hostSchemaVersion.Load()
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
		hostSchemaVersion.Store(previousSchema)
	})
	cfg := defaultPluginConfig()
	cfg.Models = []string{"gpt-5.5"}
	currentConfig.Store(cfg)
	hostSchemaVersion.Store(lifecycleSchemaVersion)
	for _, source := range []string{"openai-response", "codex"} {
		for _, wrapped := range []bool{false, true} {
			headers := http.Header{"X-CPA-Session-Id": []string{"test-session"}}
			if wrapped {
				// CodexComp clones these headers for every continuation callback.
				headers = cloneHeader(nestedRequestHeaders(headers))
			}
			raw, err := json.Marshal(rpcModelRouteRequest{ModelRouteRequest: pluginapi.ModelRouteRequest{
				RequestedModel: "gpt-5.5", SourceFormat: source, Stream: true, Headers: headers,
			}})
			if err != nil {
				t.Fatal(err)
			}
			response, err := routeModel(raw)
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				OK     bool                         `json:"ok"`
				Result pluginapi.ModelRouteResponse `json:"result"`
			}
			if err := json.Unmarshal(response, &env); err != nil {
				t.Fatal(err)
			}
			if !env.OK || env.Result.Handled == wrapped {
				t.Fatalf("source=%s wrapped=%t: unexpected route decision", source, wrapped)
			}
			if wrapped && env.Result.Reason != "already_wrapped" {
				t.Fatal("a repeated wrapper must be explicitly bypassed")
			}
		}
	}
	request, err := json.Marshal(rpcRequestInterceptRequest{RequestInterceptRequest: pluginapi.RequestInterceptRequest{
		RequestedModel: "gpt-5.5", SourceFormat: "openai-response", RequestID: "nested-request", Headers: nestedRequestHeaders(nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := interceptRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result pluginapi.RequestInterceptResponse `json:"result"`
	}
	if err := json.Unmarshal(response, &env); err != nil {
		t.Fatal(err)
	}
	if requestIDFromHeaders(env.Result.Headers) != "" {
		t.Fatal("a bypassed callback must not acquire another wrapper lifecycle ID")
	}
}
