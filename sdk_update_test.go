package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRegistrationGatesOmitBodyOnNegotiatedSchema(t *testing.T) {
	previousSchema := hostSchemaVersion.Load()
	previousConfig := loadedConfig()
	t.Cleanup(func() {
		hostSchemaVersion.Store(previousSchema)
		currentConfig.Store(previousConfig)
	})
	for _, host := range []uint32{0, 1, 2, 6, 7, 8, 9, 10, 99} {
		t.Run(fmt.Sprint(host), func(t *testing.T) {
			raw, err := handleMethod(pluginabi.MethodPluginRegister, []byte(fmt.Sprintf(`{"schema_version":%d}`, host)))
			if err != nil {
				t.Fatal(err)
			}
			var env pluginabi.Envelope
			if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
				t.Fatalf("registration envelope: %s, error: %v", raw, err)
			}
			var wire struct {
				SchemaVersion uint32                     `json:"schema_version"`
				Capabilities  map[string]json.RawMessage `json:"capabilities"`
			}
			if err := json.Unmarshal(env.Result, &wire); err != nil {
				t.Fatal(err)
			}
			wantSchema := negotiatedSchemaVersion(host)
			if wire.SchemaVersion != wantSchema {
				t.Fatalf("schema = %d, want %d", wire.SchemaVersion, wantSchema)
			}
			for _, key := range []string{"request_interceptor_omit_body_before", "request_interceptor_omit_body_after"} {
				value, present := wire.Capabilities[key]
				wantOmit := wantSchema >= 7
				if present != wantOmit {
					t.Fatalf("%s present = %t for schema %d", key, present, wantSchema)
				}
				var omit bool
				if present && (json.Unmarshal(value, &omit) != nil || !omit) {
					t.Fatalf("%s = %s, want true", key, value)
				}
			}
		})
	}
}

func TestRegistrationPublishesConfigDefaultsIndependentlyOfCurrentValues(t *testing.T) {
	previousConfig := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	cfg := defaultPluginConfig()
	cfg.Models = []string{"configured-model"}
	cfg.MaxAttempts = 17
	cfg.SameAuthRetries = 3
	cfg.InitialDelayMS = 900
	currentConfig.Store(cfg)
	raw, err := json.Marshal(pluginRegistration())
	if err != nil {
		t.Fatal(err)
	}
	var reg registration
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"models":              "[]",
		"source_formats":      "[]",
		"status_codes":        "[408,429,500,502,503,504]",
		"retry_keywords":      `["rate_limited"]`,
		"max_attempts":        "0",
		"same_auth_retries":   "0",
		"initial_delay_ms":    "500",
		"max_delay_ms":        "10000",
		"max_elapsed_time_ms": "0",
		"model_overrides":     "{}",
	}
	for _, field := range reg.Metadata.ConfigFields {
		expected, exists := want[field.Name]
		if !exists {
			t.Fatalf("unexpected config field %q", field.Name)
		}
		value, err := json.Marshal(field.Default)
		if err != nil || string(value) != expected {
			t.Fatalf("%s default = %s, error = %v, want %s", field.Name, value, err, expected)
		}
		delete(want, field.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing config defaults: %v", want)
	}
}

func TestHostModelRetriesPreserveCallerScope(t *testing.T) {
	previousConfig, previousCallHost, previousLifecycle := loadedConfig(), callHost, pluginLifecycle
	pluginLifecycle = newPluginLifecycleState()
	t.Cleanup(func() {
		pluginLifecycle.shutdown(nil)
		currentConfig.Store(previousConfig)
		callHost = previousCallHost
		pluginLifecycle = previousLifecycle
	})
	cfg := defaultPluginConfig()
	cfg.MaxAttempts = 3
	cfg.InitialDelayMS = 1
	cfg.MaxDelayMS = 1
	currentConfig.Store(cfg)
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{"caller A", map[string]any{"caller_scope": "opaque-caller-a"}, "opaque-caller-a"},
		{"caller B", map[string]any{"caller_scope": "opaque-caller-b"}, "opaque-caller-b"},
		{"padded handle", map[string]any{"caller_scope": " opaque-caller-c "}, "opaque-caller-c"},
		{"legacy host", nil, ""},
		{"missing handle", map[string]any{"other": "value"}, ""},
		{"invalid handle", map[string]any{"caller_scope": 42}, ""},
		{"blank handle", map[string]any{"caller_scope": "  "}, ""},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				attempts := 0
				callHost = func(method string, payload any) (json.RawMessage, error) {
					switch method {
					case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
						attempts++
						wire, err := json.Marshal(payload)
						if err != nil {
							return nil, err
						}
						var nested hostModelExecutionRequest
						if err := json.Unmarshal(wire, &nested); err != nil {
							return nil, err
						}
						if nested.CallerScope != tc.want || nested.Stream != stream {
							t.Fatalf("attempt %d: caller scope = %q, stream = %t", attempts, nested.CallerScope, nested.Stream)
						}
						if attempts < 3 {
							return nil, retryStatusError{status: http.StatusBadGateway}
						}
						if stream {
							return json.Marshal(pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "sdk-test-stream"})
						}
						return json.Marshal(pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK})
					case pluginabi.MethodHostModelStreamRead:
						return json.Marshal(pluginapi.HostModelStreamReadResponse{Done: true})
					case pluginabi.MethodHostLog, pluginabi.MethodHostModelStreamClose:
						return json.RawMessage(`{}`), nil
					default:
						t.Fatalf("unexpected host callback %q", method)
						return nil, nil
					}
				}
				req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
					Model: "test-model", SourceFormat: "claude", Format: "claude", Metadata: tc.metadata,
				}}
				original, _ := json.Marshal(req)
				var err error
				if stream {
					err = runModelStreamWithRetry(context.Background(), req)
				} else {
					_, err = runModelExecuteWithRetry(context.Background(), req)
				}
				if err != nil || attempts != 3 {
					t.Fatalf("attempts = %d, error = %v, want success on third attempt", attempts, err)
				}
				after, _ := json.Marshal(req)
				if string(after) != string(original) {
					t.Fatal("nested execution modified the original request")
				}
			})
		}
	}
}
