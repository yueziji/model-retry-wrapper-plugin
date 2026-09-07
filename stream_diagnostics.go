package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	streamDiagnosticPayloadLimit = 8
	streamDiagnosticByteLimit    = 16 * 1024
	streamDiagnosticEventLimit   = 8
)

// These summaries are observational only. They never decide whether to retry or
// hold back a payload, and retain no response bytes after inspection.
type streamPayloadDiagnostic struct {
	bytes     int
	events    []string
	truncated bool
}

type streamDiagnostics struct {
	attempt        int
	receivedChunks int64
	receivedBytes  int64
	emittedChunks  int64
	emittedBytes   int64
	emitStarted    bool
	payloads       []streamPayloadDiagnostic
}

func (d *streamDiagnostics) observe(payload []byte) {
	if len(payload) == 0 {
		return
	}
	d.receivedChunks++
	d.receivedBytes += int64(len(payload))
	if len(d.payloads) < streamDiagnosticPayloadLimit {
		d.payloads = append(d.payloads, inspectStreamPayload(payload))
	}
}

func (d *streamDiagnostics) recordEmitted(payload []byte) {
	d.emittedChunks++
	d.emittedBytes += int64(len(payload))
}

func (d *streamDiagnostics) summary() string {
	var text strings.Builder
	fmt.Fprintf(&text, "attempt=%d received_chunks=%d received_bytes=%d emitted_chunks=%d emitted_bytes=%d emit_started=%t sampled_payloads=%d samples_truncated=%t payloads=[",
		d.attempt, d.receivedChunks, d.receivedBytes, d.emittedChunks, d.emittedBytes,
		d.emitStarted, len(d.payloads), d.receivedChunks > int64(len(d.payloads)))
	for i, payload := range d.payloads {
		if i > 0 {
			text.WriteByte(',')
		}
		fmt.Fprintf(&text, "{n=%d bytes=%d events=%s truncated=%t}", i+1, payload.bytes, strings.Join(payload.events, "+"), payload.truncated)
	}
	text.WriteByte(']')
	return text.String()
}

func (d *streamPayloadDiagnostic) addEvent(event string) {
	for _, existing := range d.events {
		if existing == event {
			return
		}
	}
	if len(d.events) == streamDiagnosticEventLimit {
		d.truncated = true
		return
	}
	d.events = append(d.events, event)
}

func inspectStreamPayload(payload []byte) streamPayloadDiagnostic {
	d := streamPayloadDiagnostic{bytes: len(payload)}
	if len(payload) > streamDiagnosticByteLimit {
		payload = payload[:streamDiagnosticByteLimit]
		d.truncated = true
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		d.addEvent("whitespace")
		return d
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		d.inspectJSON(trimmed)
		return d
	}

	// Inspect SSE frames within this payload only. A frame split across host
	// chunks is explicitly marked partial; diagnostics do not buffer the stream.
	var data []byte
	frameStarted := false
	flush := func(partial bool) {
		if len(data) > 0 {
			d.inspectJSON(data)
		}
		if partial && frameStarted {
			d.addEvent("sse.partial_frame")
		}
		data = nil
		frameStarted = false
	}
	lines := bytes.Split(payload, []byte{'\n'})
	for i, line := range lines {
		if i == len(lines)-1 && len(line) == 0 {
			break // A trailing newline terminates a line, not an SSE frame.
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			flush(false)
			continue
		}
		if line[0] == ':' {
			d.addEvent("sse.comment")
			continue
		}
		field, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "event":
			frameStarted = true
			d.addEvent(diagnosticEventType(string(value)))
		case "data":
			frameStarted = true
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		case "id", "retry":
			d.addEvent("sse.metadata")
		default:
			d.addEvent("unclassified")
		}
	}
	flush(true)
	if len(d.events) == 0 {
		d.addEvent("unclassified")
	}
	return d
}

func (d *streamPayloadDiagnostic) inspectJSON(data []byte) {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		d.addEvent("sse.done")
		return
	}
	var event struct {
		Type         string          `json:"type"`
		Object       string          `json:"object"`
		Item         json.RawMessage `json:"item"`
		Delta        json.RawMessage `json:"delta"`
		ContentBlock json.RawMessage `json:"content_block"`
		Candidates   json.RawMessage `json:"candidates"`
		Choices      []struct {
			Delta        map[string]json.RawMessage `json:"delta"`
			FinishReason json.RawMessage            `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		d.addEvent("incomplete_or_unclassified_json")
		return
	}
	if event.Type != "" {
		d.addEvent(diagnosticEventType(event.Type))
		for _, nested := range []struct{ prefix, kind string }{
			{"item", diagnosticNestedType(event.Item)}, {"delta", diagnosticNestedType(event.Delta)}, {"block", diagnosticNestedType(event.ContentBlock)},
		} {
			switch nested.kind {
			case "message", "function_call", "custom_tool_call", "reasoning", "web_search_call", "file_search_call", "computer_call", "code_interpreter_call", "mcp_call",
				"text", "tool_use", "thinking", "redacted_thinking", "text_delta", "input_json_delta", "thinking_delta", "signature_delta", "citations_delta":
				d.addEvent(nested.prefix + "." + nested.kind)
			}
		}
		return
	}
	if event.Object == "chat.completion.chunk" || len(event.Choices) > 0 {
		d.addEvent("chat.completion.chunk")
		for _, choice := range event.Choices {
			for _, key := range []string{"role", "content", "tool_calls", "function_call", "reasoning", "reasoning_content", "audio", "refusal"} {
				if diagnosticValuePresent(choice.Delta[key]) {
					d.addEvent("chat." + key)
				}
			}
			if diagnosticValuePresent(choice.FinishReason) {
				d.addEvent("chat.finish")
			}
		}
		return
	}
	if len(event.Candidates) > 0 {
		d.addEvent("gemini.candidates")
		return
	}
	d.addEvent("unclassified_json")
}

func diagnosticNestedType(data json.RawMessage) string {
	var nested struct{ Type string }
	if err := json.Unmarshal(data, &nested); err != nil {
		return ""
	}
	return nested.Type
}

func diagnosticValuePresent(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte(`""`)) && !bytes.Equal(value, []byte("[]")) && !bytes.Equal(value, []byte("{}"))
}

// Only fixed labels may reach a log. Unknown event names can contain arbitrary
// provider or user data, so never copy them into diagnostic output.
func diagnosticEventType(value string) string {
	switch value {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.failed", "response.incomplete",
		"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done", "response.audio.delta", "response.audio.done",
		"response.audio_transcript.delta", "response.audio_transcript.done", "response.output_audio.delta", "response.output_audio.done",
		"message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "ping", "error":
		return value
	default:
		return "unknown_event"
	}
}

func diagnosticProtocol(value string) string {
	switch value := normalizeSourceFormat(value); value {
	case "openai", "openai-response", "claude", "gemini", "chat-completions", "codex":
		return value
	default:
		return "unknown"
	}
}

func streamFailureDiagnostic(message, source string, cfg pluginConfig, diagnostics *streamDiagnostics, err error) string {
	status := statusFromError(err)
	retryEligible, _ := shouldRetryFailure(cfg, diagnostics.attempt, status, err)
	reason := "stream_forwarding"
	if diagnostics.emitStarted {
		reason = "downstream_started"
	}
	if source == "first_payload" || source == "payload" {
		reason = "downstream_emit_failed"
	}
	return fmt.Sprintf("%s retry_skipped=%s status=%d keyword_match=%t startup_retry_eligible=%t %s",
		message, reason, status, retryKeywordFromError(cfg, err) != "", retryEligible, diagnostics.summary())
}
