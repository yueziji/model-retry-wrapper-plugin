package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxStreamStartupBytes = 16 * 1024

// Only the first Responses/Codex event is inspected, before any downstream emission.
// Normal handshake events immediately end inspection; this is not a content buffer.
func readStreamStartupPayload(format string, read func() (pluginapi.HostModelStreamReadResponse, error)) ([]byte, error) {
	var payload []byte
	for {
		chunk, err := read()
		if err != nil {
			return nil, err
		}
		if chunk.Error != "" {
			return nil, errors.New(chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			payload = append(payload, chunk.Payload...)
			pending, _ := inspectStreamStartup(format, payload)
			if !pending {
				return payload, nil
			}
		}
		if chunk.Done {
			return payload, nil
		}
	}
}

type streamStartupError struct {
	status  int
	details string
}

func (e *streamStartupError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("upstream stream startup failed (HTTP %d)", e.status)
	}
	return "upstream stream startup failed"
}

func (e *streamStartupError) StatusCode() int { return e.status }

type streamErrorDetails struct {
	Code    json.RawMessage `json:"code"`
	Type    string          `json:"type"`
	Message string          `json:"message"`
}

// Non-retryable errors, exhausted attempts and unrecognized payloads retain their wire form.
func retryableStreamStartupError(cfg pluginConfig, attempt int, format string, payload []byte) error {
	_, failure := inspectStreamStartup(format, payload)
	if failure != nil {
		if retry, _ := shouldRetryFailure(cfg, attempt, failure.status, failure); retry {
			return failure
		}
	}
	return nil
}

func inspectStreamStartup(format string, payload []byte) (pending bool, failure *streamStartupError) {
	format = normalizeSourceFormat(format)
	if (format != "openai-response" && format != "codex") || len(payload) >= maxStreamStartupBytes {
		return false, nil
	}
	data := bytes.TrimSpace(payload)
	frameEnded := bytes.HasSuffix(payload, []byte("\n\n")) || bytes.HasSuffix(payload, []byte("\r\n\r\n"))
	for len(data) > 0 && data[0] != '{' {
		if bytes.HasPrefix(data, []byte("data:")) {
			data = bytes.TrimSpace(data[len("data:"):])
			break
		}
		if bytes.HasPrefix([]byte("data:"), data) || bytes.HasPrefix([]byte("event:"), data) {
			return true, nil
		}
		if !bytes.HasPrefix(data, []byte("event:")) {
			// Comments/keepalives and other protocols retain the first-payload behavior.
			return false, nil
		}
		_, rest, found := bytes.Cut(data, []byte("\n"))
		if !found || len(bytes.TrimSpace(rest)) == 0 {
			return !frameEnded, nil
		}
		data = bytes.TrimSpace(rest)
	}
	if len(data) == 0 {
		return len(bytes.TrimSpace(payload)) != 0 && !frameEnded, nil
	}
	if data[0] != '{' {
		return false, nil
	}
	var event struct {
		Type     string              `json:"type"`
		Error    *streamErrorDetails `json:"error"`
		Response struct {
			Error  *streamErrorDetails `json:"error"`
			Output []json.RawMessage   `json:"output"`
		} `json:"response"`
	}
	// Decode one JSON object even when CPA joins events or omits SSE newlines.
	err := json.NewDecoder(bytes.NewReader(data)).Decode(&event)
	if err != nil {
		return !frameEnded && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)), nil
	}
	var details *streamErrorDetails
	switch event.Type {
	case "response.failed":
		if len(event.Response.Output) != 0 {
			return false, nil
		}
		details = event.Response.Error
	case "error":
		details = event.Error
	default:
		return false, nil
	}
	if details == nil {
		return false, nil
	}
	code := string(details.Code)
	if len(details.Code) > 0 && details.Code[0] == '"' {
		_ = json.Unmarshal(details.Code, &code)
	}
	status, _ := strconv.Atoi(code)
	if status < 400 || status > 599 {
		switch {
		case normalizeKey(code) == "server_is_overloaded", normalizeKey(details.Type) == "service_unavailable_error":
			status = 503
		case normalizeKey(code) == "rate_limit_exceeded", normalizeKey(details.Type) == "rate_limit_error":
			status = 429
		case normalizeKey(details.Type) == "invalid_request_error":
			status = 400
		case normalizeKey(details.Type) == "authentication_error":
			status = 401
		default:
			status = statusFromError(errors.New(details.Message))
		}
	}
	return false, &streamStartupError{
		status:  status,
		details: strings.Join([]string{code, details.Type, details.Message}, " "),
	}
}
