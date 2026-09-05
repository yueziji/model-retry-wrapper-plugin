package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestStreamPayloadDiagnosticsRecognizeEventsWithoutContent(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		events  []string
	}{
		{
			name:    "responses startup",
			payload: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"private-id\",\"instructions\":\"private-content\"}}\n\n",
			events:  []string{"response.created"},
		},
		{
			name:    "responses text delta is a string",
			payload: `{"type":"response.output_text.delta","delta":"private-content"}`,
			events:  []string{"response.output_text.delta"},
		},
		{
			name:    "multiple frames in one payload",
			payload: "data: {\"type\":\"response.in_progress\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"private-content\"}\n\n",
			events:  []string{"response.in_progress", "response.output_text.delta"},
		},
		{
			name:    "multiline SSE data",
			payload: "data: {\"type\":\ndata: \"response.created\"}\n\n",
			events:  []string{"response.created"},
		},
		{
			name:    "tool item",
			payload: `{"type":"response.output_item.added","item":{"type":"function_call","name":"private-tool","arguments":"private-content"}}`,
			events:  []string{"response.output_item.added", "item.function_call"},
		},
		{
			name:    "claude tool delta with CRLF",
			payload: "event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"private-content\"}}\r\n\r\n",
			events:  []string{"content_block_delta", "delta.input_json_delta"},
		},
		{
			name:    "chat role only",
			payload: `{"object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			events:  []string{"chat.completion.chunk", "chat.role"},
		},
		{
			name:    "chat content reasoning and tool arguments",
			payload: `{"choices":[{"delta":{"content":"private-content","reasoning_content":"private-reasoning","tool_calls":[{"function":{"arguments":"private-arguments"}}]},"finish_reason":"tool_calls"}]}`,
			events:  []string{"chat.completion.chunk", "chat.content", "chat.tool_calls", "chat.reasoning_content", "chat.finish"},
		},
		{
			name:    "gemini",
			payload: `{"candidates":[{"content":{"parts":[{"text":"private-content"}]}}]}`,
			events:  []string{"gemini.candidates"},
		},
		{
			name:    "heartbeat and metadata",
			payload: ": private-comment\nid: private-id\nretry: 1000\n\n",
			events:  []string{"sse.comment", "sse.metadata"},
		},
		{
			name:    "done",
			payload: "data: [DONE]\n\n",
			events:  []string{"sse.done"},
		},
		{
			name:    "unknown event names are never logged",
			payload: "event: private-event\ndata: {\"type\":\"private-type\",\"authorization\":\"private-credential\"}\n\n",
			events:  []string{"unknown_event"},
		},
		{
			name:    "split frame",
			payload: "event: response.created\ndata: {\"type\":\"response.cre",
			events:  []string{"response.created", "incomplete_or_unclassified_json", "sse.partial_frame"},
		},
		{
			name:    "one newline is not an SSE frame terminator",
			payload: "data: {\"type\":\"response.created\"}\n",
			events:  []string{"response.created", "sse.partial_frame"},
		},
		{
			name:    "opaque bytes",
			payload: "private-opaque-body",
			events:  []string{"unclassified"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(tt.payload)
			original := bytes.Clone(payload)
			d := &streamDiagnostics{attempt: 1}
			d.observe(payload)
			if !bytes.Equal(payload, original) {
				t.Fatal("inspection modified the forwarded bytes")
			}
			got := d.payloads[0]
			if !reflect.DeepEqual(got.events, tt.events) {
				t.Fatalf("events = %v, want %v", got.events, tt.events)
			}
			if got.bytes != len(payload) || got.truncated {
				t.Fatalf("bytes = %d, truncated = %t", got.bytes, got.truncated)
			}
			summary := d.summary()
			if strings.Contains(summary, "private-") {
				t.Fatal("diagnostic summary exposed content or an unknown event name")
			}
			for i := range payload {
				payload[i] = 'x'
			}
			if d.summary() != summary {
				t.Fatal("diagnostic summary retained the original payload buffer")
			}
		})
	}
}

func TestStreamDiagnosticsBoundInspectionAndKeepAccurateCounters(t *testing.T) {
	d := &streamDiagnostics{attempt: 2}
	d.observe(nil)
	payload := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("private-content", streamDiagnosticByteLimit) + `"}`)
	for i := 0; i < streamDiagnosticPayloadLimit+3; i++ {
		d.observe(payload)
		d.emitStarted = true
		if i < 2 {
			d.recordEmitted(payload)
		}
	}
	if len(d.payloads) != streamDiagnosticPayloadLimit {
		t.Fatalf("sampled payload count = %d", len(d.payloads))
	}
	if d.receivedChunks != streamDiagnosticPayloadLimit+3 || d.emittedChunks != 2 {
		t.Fatalf("received = %d, emitted = %d", d.receivedChunks, d.emittedChunks)
	}
	if d.receivedBytes != int64(len(payload))*d.receivedChunks || d.emittedBytes != int64(len(payload))*2 {
		t.Fatal("byte counters must count whole payloads, independently of the inspection cap")
	}
	for _, sample := range d.payloads {
		if !sample.truncated || sample.bytes != len(payload) {
			t.Fatal("oversized payload must report its actual size and truncated inspection")
		}
	}
	summary := d.summary()
	if !strings.Contains(summary, "samples_truncated=true") || len(summary) > 4096 || strings.Contains(summary, "private-") {
		t.Fatal("summary must be bounded, sanitized, and report the sample limit")
	}
}

func TestStreamDiagnosticsLimitEventLabels(t *testing.T) {
	var payload strings.Builder
	for _, event := range []string{
		"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed",
	} {
		fmt.Fprintf(&payload, "data: {\"type\":%q}\n\n", event)
	}
	d := inspectStreamPayload([]byte(payload.String()))
	if len(d.events) != streamDiagnosticEventLimit || !d.truncated {
		t.Fatalf("events = %d, truncated = %t", len(d.events), d.truncated)
	}
}

func TestStreamFailureDiagnosticExplainsSkippedRetry(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.RetryKeywords = []string{"rate_limit_exceeded"}
	errRateLimit := errors.New(`{"error":{"code":"rate_limit_exceeded","message":"private-error-content"}}`)
	tests := []struct {
		name        string
		source      string
		err         error
		emitStarted bool
		emitted     int64
		maxAttempts int
		want        []string
	}{
		{"keyword after startup", "error_chunk", errRateLimit, true, 1, 0, []string{"retry_skipped=downstream_started", "status=0", "keyword_match=true", "startup_retry_eligible=true", "emitted_chunks=1"}},
		{"unmatched keyword", "error_chunk", errors.New("private-other-error"), true, 1, 0, []string{"keyword_match=false", "startup_retry_eligible=false"}},
		{"HTTP status takes precedence", "error_chunk", retryStatusError{status: http.StatusBadRequest, err: errRateLimit}, true, 1, 0, []string{"status=400", "keyword_match=true", "startup_retry_eligible=false"}},
		{"attempt limit", "error_chunk", errRateLimit, true, 1, 1, []string{"keyword_match=true", "startup_retry_eligible=false"}},
		{"write failure with uncertain delivery", "first_payload", errRateLimit, true, 0, 0, []string{"retry_skipped=downstream_emit_failed", "emit_started=true", "emitted_chunks=0"}},
		{"forwarding without a first payload", "read", errRateLimit, false, 0, 0, []string{"retry_skipped=stream_forwarding", "emit_started=false", "emitted_chunks=0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.MaxAttempts = tt.maxAttempts
			d := &streamDiagnostics{attempt: 1, emitStarted: tt.emitStarted, emittedChunks: tt.emitted}
			message := streamFailureDiagnostic("host stream returned error", tt.source, cfg, d, tt.err)
			for _, want := range tt.want {
				if !strings.Contains(message, want) {
					t.Fatalf("message missing %q: %s", want, message)
				}
			}
			if strings.Contains(message, "private-") || strings.Contains(message, "rate_limit_exceeded") {
				t.Fatal("diagnostic message must not copy error bodies or configured keyword values")
			}
		})
	}
}

func TestDiagnosticProtocolDoesNotExposeUnknownValues(t *testing.T) {
	if got := diagnosticProtocol("responses"); got != "openai-response" {
		t.Fatalf("protocol = %q", got)
	}
	if got := diagnosticProtocol("private-credential"); got != "unknown" {
		t.Fatal("unknown protocol value reached diagnostic output")
	}
}
