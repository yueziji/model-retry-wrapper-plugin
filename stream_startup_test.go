package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const startupFailedJSON = `{"type":"response.failed","response":{"error":{"code":502,"message":"test failure"}}}`
const startupCreatedJSON = `{"type":"response.created","response":{"id":"test-response"}}`

func TestRetryableStreamStartupErrors(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.MaxAttempts = 2
	for _, tc := range []struct {
		name, payload string
		status        int
	}{
		{"codexcomp failure", "data: " + startupFailedJSON + "\n\n", 502},
		{"raw websocket event", startupFailedJSON, 502},
		{"SSE event field", "event: response.failed\r\ndata:" + startupFailedJSON + "\r\n\r\n", 502},
		{"overload", `data: {"type":"error","error":{"code":"server_is_overloaded"}}`, 503},
		{"rate limit", `data: {"type":"error","error":{"type":"rate_limit_error"}}`, 429},
		{"numeric string code", `data: {"type":"response.failed","response":{"error":{"code":"503"}}}`, 503},
		{"keyword fallback", `data: {"type":"error","error":{"message":"rate_limited private-detail"}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, format := range []string{"codex", "openai-response"} {
				err := retryableStreamStartupError(cfg, 1, format, []byte(tc.payload))
				if err == nil || statusFromError(err) != tc.status {
					t.Fatalf("format=%s error=%v, want status=%d", format, err, tc.status)
				}
				if strings.Contains(err.Error(), "private-detail") || strings.Contains(err.Error(), "test failure") {
					t.Fatal("startup diagnostics must not expose upstream error details")
				}
				if retryableStreamStartupError(cfg, 2, format, []byte(tc.payload)) != nil {
					t.Fatal("an exhausted retry budget must preserve the last failure payload")
				}
			}
		})
	}
}

func TestStreamStartupPreservesNonRetryablePayloads(t *testing.T) {
	cfg := defaultPluginConfig()
	for _, payload := range []string{
		"data: " + startupCreatedJSON + "\n\ndata: " + startupFailedJSON,
		"data: " + startupCreatedJSON + "data: " + startupFailedJSON,
		`data: {"type":"response.output_text.delta","delta":"rate_limited"}`,
		`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`data: {"type":"response.failed","response":{"output":[{"type":"reasoning"}],"error":{"code":502}}}`,
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"rate_limited"}}`,
		`data: {"type":"error","error":{"type":"authentication_error"}}`,
		`data: {"type":"error","error":{"message":"model-2026 failed"}}`,
		`data: {"type":"error","error":{"code":200}}`,
		`data: {"type":"response.failed"}`,
		`data: {malformed}`,
		"data: [DONE]\n\n", ": keepalive\n\n", "\n",
		"data:\n\n", "data:\r\n\r\n", "event: keepalive\n\n", "data: {\n\n",
	} {
		pending, failure := inspectStreamStartup("codex", []byte(payload))
		if pending || retryableStreamStartupError(cfg, 1, "codex", []byte(payload)) != nil {
			t.Fatalf("ordinary/non-retryable payload was held or retried; failure=%v", failure)
		}
	}
	cfg.StatusCodes = nil
	cfg.RetryKeywords = nil
	if retryableStreamStartupError(cfg, 1, "codex", []byte(startupFailedJSON)) != nil {
		t.Fatal("disabled retry rules must apply to startup events too")
	}
	if pending, failure := inspectStreamStartup("claude", []byte(startupFailedJSON)); pending || failure != nil {
		t.Fatal("other protocols must retain their existing first-payload behavior")
	}
}

func TestStreamStartupReassemblesFragmentedFirstFailure(t *testing.T) {
	wire := []byte("event: response.failed\ndata: " + startupFailedJSON + "\n\n")
	endJSON := bytes.Index(wire, []byte("\n\n"))
	for split := 1; split < endJSON; split++ {
		reads := 0
		chunks := [][]byte{wire[:split], wire[split:]}
		got, err := readStreamStartupPayload("codex", func() (pluginapi.HostModelStreamReadResponse, error) {
			if reads >= len(chunks) {
				t.Fatal("startup reader consumed past the first event")
			}
			chunk := chunks[reads]
			reads++
			return pluginapi.HostModelStreamReadResponse{Payload: chunk}, nil
		})
		if err != nil || !bytes.Equal(got, wire) || reads != 2 {
			t.Fatalf("split=%d reads=%d error=%v: first failure was not preserved", split, reads, err)
		}
		if retryableStreamStartupError(defaultPluginConfig(), 1, "codex", got) == nil {
			t.Fatalf("split=%d: fragmented first failure was not recognized", split)
		}
	}
}

func TestStreamStartupStopsAtFirstNormalEventAndProbeLimit(t *testing.T) {
	for _, wire := range [][]byte{
		[]byte("data: " + startupCreatedJSON + "\n\n"),
		[]byte(": keepalive\n\n"),
		[]byte("data:\n\n"),
		[]byte("data: {\"type\":\"" + strings.Repeat("x", maxStreamStartupBytes)),
	} {
		reads := 0
		got, err := readStreamStartupPayload("codex", func() (pluginapi.HostModelStreamReadResponse, error) {
			reads++
			if reads != 1 {
				t.Fatal("startup reader delayed a normal event or read beyond the probe limit")
			}
			return pluginapi.HostModelStreamReadResponse{Payload: wire}, nil
		})
		if err != nil || !bytes.Equal(got, wire) {
			t.Fatalf("startup payload changed: %v", err)
		}
	}
}

func TestStreamStartupEOFAndTransportError(t *testing.T) {
	prefix := []byte(`data: {"type":"response.fai`)
	reads := 0
	got, err := readStreamStartupPayload("codex", func() (pluginapi.HostModelStreamReadResponse, error) {
		reads++
		if reads == 1 {
			return pluginapi.HostModelStreamReadResponse{Payload: prefix}, nil
		}
		return pluginapi.HostModelStreamReadResponse{Done: true}, nil
	})
	if err != nil || !bytes.Equal(got, prefix) {
		t.Fatal("an incomplete event at EOF must retain its original bytes")
	}
	transportErr := errors.New("test transport failure")
	_, err = readStreamStartupPayload("codex", func() (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{}, transportErr
	})
	if !errors.Is(err, transportErr) {
		t.Fatal("transport errors must retain their existing retry path")
	}
}
