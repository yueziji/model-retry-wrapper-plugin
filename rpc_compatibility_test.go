package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestDecodeHostResponsePreservesErrorMetadata(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wire       string
		status     int
		structured int
		retryable  bool
	}{
		{"structured HTTP status", `{"ok":false,"error":{"code":"host_call_failed","message":"rate_limited HTTP 503","http_status":400,"retryable":true}}`, 400, 400, true},
		{"legacy text status", `{"ok":false,"error":{"code":"host_call_failed","message":"rate_limited HTTP 400"}}`, 400, 0, false},
		{"legacy statusless error", `{"ok":false,"error":{"code":"host_call_failed","message":"rate_limited"}}`, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeHostResponse(pluginabi.MethodHostModelExecuteStream, 1, []byte(tc.wire))
			if err == nil || !strings.HasPrefix(err.Error(), "host_call_failed: rate_limited") {
				t.Fatalf("host error prefix or message lost: %v", err)
			}
			var rpcErr *pluginabi.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != "host_call_failed" || rpcErr.HTTPStatus != tc.structured || rpcErr.Retryable != tc.retryable {
				t.Fatalf("SDK error metadata lost: %#v", rpcErr)
			}
			if status := statusFromError(err); status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if retry, keyword := shouldRetryFailure(defaultPluginConfig(), 1, tc.status, err); !retry || keyword != "rate_limited" {
				t.Fatalf("keyword retry through RPC error = %t, %q", retry, keyword)
			}
			var forwarded pluginabi.Envelope
			if errDecode := json.Unmarshal(errorEnvelopeForError("plugin_error", errorWithStatus(err, tc.status)), &forwarded); errDecode != nil {
				t.Fatal(errDecode)
			}
			if forwarded.OK || forwarded.Error == nil || forwarded.Error.HTTPStatus != tc.status || forwarded.Error.Message != err.Error() {
				t.Fatalf("forwarded error changed: %#v", forwarded)
			}
		})
	}
}

func TestDecodeHostResponseHandlesSuccessAndProtocolFailures(t *testing.T) {
	result, err := decodeHostResponse("test", 0, []byte(`{"ok":true,"result":{"body":"dGVzdA=="}}`))
	if err != nil || string(result) != `{"body":"dGVzdA=="}` {
		t.Fatalf("result = %s, error = %v", result, err)
	}
	for _, tc := range []struct {
		wire string
		code int
		want string
	}{
		{"", 1, "returned no response"},
		{"{", 0, "decode host callback envelope"},
		{`{"ok":false}`, 0, "host callback test failed"},
		{`{"ok":true,"result":{}}`, 1, "returned code=1"},
	} {
		if _, err := decodeHostResponse("test", tc.code, []byte(tc.wire)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error = %v, want %q", err, tc.want)
		}
	}
}

func TestOfficialErrorEnvelopeKeepsLegacyWireShape(t *testing.T) {
	raw := errorEnvelope("test_error", "test message")
	if string(raw) != `{"ok":false,"error":{"code":"test_error","message":"test message"}}` {
		t.Fatalf("legacy envelope changed: %s", raw)
	}
}
