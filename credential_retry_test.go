//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func pickCredentialForTest(t *testing.T, headers http.Header, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	t.Helper()
	raw, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Options: pluginapi.SchedulerOptions{Headers: headers}, Candidates: candidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handleMethod(pluginabi.MethodSchedulerPick, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(result, &env); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestCredentialRetryConfiguration(t *testing.T) {
	for _, raw := range []string{"same_auth_retries: -1", "model_overrides: {test: {same_auth_retries: -1}}"} {
		if _, err := decodeConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	cfg, err := decodeConfig([]byte("models: [a, b]\nsame_auth_retries: 2\nmodel_overrides: {a: {same_auth_retries: 0}, unused: {same_auth_retries: 4}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if retryConfigForModel(cfg, "a").SameAuthRetries != 0 || retryConfigForModel(cfg, "b").SameAuthRetries != 2 {
		t.Fatal("override inheritance failed")
	}
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	currentConfig.Store(cfg)
	reg := pluginRegistration()
	if !reg.Capabilities.Scheduler || !reg.Capabilities.SchedulerAcrossPriorities {
		t.Fatal("enabled retry policy must receive lower-priority fallback candidates")
	}
	cfg.Models = []string{"a"}
	currentConfig.Store(cfg)
	if pluginRegistration().Capabilities.Scheduler {
		t.Fatal("disabled and unlisted overrides must not activate the scheduler")
	}
	if defaultPluginConfig().SameAuthRetries != 0 {
		t.Fatal("existing installations must retain normal CPA selection")
	}
}

func TestCredentialSelectionIsolationAndCleanup(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.SameAuthRetries = 2
	first, second := rpcExecutorRequest{}, rpcExecutorRequest{}
	a, releaseA := beginCredentialRetry(&first, cfg)
	b, releaseB := beginCredentialRetry(&second, cfg)
	t.Cleanup(releaseA)
	t.Cleanup(releaseB)
	a.prepare(&first)
	b.prepare(&second)
	low := pluginapi.SchedulerAuthCandidate{ID: "low", Priority: 1}
	high := pluginapi.SchedulerAuthCandidate{ID: "high", Priority: 10}
	if got := pickCredentialForTest(t, credentialRequestHeaders(first), low, high); got.AuthID != "high" {
		t.Fatalf("selected %q", got.AuthID)
	}
	if got := pickCredentialForTest(t, credentialRequestHeaders(first), low); !got.Reject {
		t.Fatal("CPA switched before the wrapper could retry")
	}
	if got := pickCredentialForTest(t, credentialRequestHeaders(second), low); got.AuthID != "low" {
		t.Fatal("another request inherited the first request's pin")
	}
	releaseA()
	if got := pickCredentialForTest(t, credentialRequestHeaders(first), low); got.Handled {
		t.Fatal("completed request remained registered")
	}
	if got := pickCredentialForTest(t, nil, low); got.Handled {
		t.Fatal("unwrapped request was intercepted")
	}
	if got := pickCredentialForTest(t, http.Header{credentialSequenceHeader: []string{"unknown"}}, low); got.Handled {
		t.Fatal("unregistered token was accepted")
	}
}

// The host stub offers only ready candidates and asks again after an upstream
// failure, as CPA does. It deliberately does not implement the wrapper's retry
// budget: the real executor loop and scheduler must produce the expected order.
func installCredentialHost(t *testing.T, cfg pluginConfig, stream bool, statusA int, failAfterPayload bool, otherStatuses ...map[string]int) (*[]string, *[]string) {
	t.Helper()
	previousCall, previousConfig := callHost, loadedConfig()
	t.Cleanup(func() { callHost = previousCall; currentConfig.Store(previousConfig) })
	currentConfig.Store(cfg)
	calls, pins := []string{}, []string{}
	reads := 0
	encode := func(v any) (json.RawMessage, error) { raw, err := json.Marshal(v); return raw, err }
	callHost = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
			req := payload.(hostModelExecutionRequest)
			pins = append(pins, req.AuthID)
			if req.Model != "test" || string(req.Body) != `{"model":"test"}` || req.Query.Get("q") != "v" || req.Headers.Get("X-Test") != "preserved" {
				t.Fatal("host request changed")
			}
			candidates := []pluginapi.SchedulerAuthCandidate{{ID: "B", Priority: 1}, {ID: "A", Priority: 10}}
			if len(otherStatuses) > 0 {
				candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: "C", Priority: 0})
			}
			if req.AuthID != "" {
				candidates = []pluginapi.SchedulerAuthCandidate{{ID: req.AuthID, Priority: 10}}
			}
			var upstreamErr error
			for len(candidates) > 0 {
				pick := pickCredentialForTest(t, req.Headers, candidates...)
				if !pick.Handled {
					t.Fatal("scheduler did not handle wrapped host call")
				}
				if pick.Reject {
					if upstreamErr != nil {
						return nil, upstreamErr
					}
					return nil, retryStatusError{status: 503}
				}
				calls = append(calls, pick.AuthID)
				status := 200
				if pick.AuthID == "A" {
					status = statusA
				} else if len(otherStatuses) > 0 {
					if configured, exists := otherStatuses[0][pick.AuthID]; exists {
						status = configured
					}
				}
				if status == 200 {
					if stream {
						return encode(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "stream"})
					}
					return encode(pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte("success")})
				}
				upstreamErr = retryStatusError{status: status}
				remaining := candidates[:0]
				for _, candidate := range candidates {
					if candidate.ID != pick.AuthID {
						remaining = append(remaining, candidate)
					}
				}
				candidates = remaining
			}
			return nil, upstreamErr
		case pluginabi.MethodHostModelStreamRead:
			reads++
			if reads == 1 {
				return encode(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: success\n\n")})
			}
			if failAfterPayload {
				return encode(pluginapi.HostModelStreamReadResponse{Error: "HTTP 503 stream failed"})
			}
			return encode(pluginapi.HostModelStreamReadResponse{Done: true})
		default:
			return encode(struct{}{})
		}
	}
	return &calls, &pins
}

func credentialTestRequest() rpcExecutorRequest {
	return rpcExecutorRequest{StreamID: "plugin-stream", ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "test", SourceFormat: "openai", Format: "openai",
		OriginalRequest: []byte(`{"model":"test"}`), Headers: http.Header{"X-Test": []string{"preserved"}}, Query: url.Values{"q": []string{"v"}},
	}}
}

func TestCredentialRetryExecutionOrder(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		retries, max, status int
		want                 []string
		wantErr              bool
	}{
		{"two extra attempts then fallback", 2, 4, 503, []string{"A", "A", "A", "B"}, false},
		{"one extra attempt then fallback", 1, 4, 503, []string{"A", "A", "B"}, false},
		{"total attempt cap", 2, 2, 503, []string{"A", "A"}, true},
		{"first success", 2, 4, 200, []string{"A"}, false},
		{"native authentication failover", 2, 4, 401, []string{"A", "B"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultPluginConfig()
			cfg.Models = []string{"test"}
			cfg.SameAuthRetries = tc.retries
			cfg.MaxAttempts = tc.max
			cfg.InitialDelayMS = 1
			cfg.MaxDelayMS = 1
			calls, pins := installCredentialHost(t, cfg, false, tc.status, false)
			_, err := runModelExecuteWithRetry(context.Background(), credentialTestRequest())
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(*calls, tc.want) {
				t.Fatalf("calls=%v error=%v", *calls, err)
			}
			if (*pins)[0] != "" {
				t.Fatal("first attempt should use CPA's candidate priorities")
			}
			if tc.status == 503 && len(*pins) > 1 && (*pins)[1] != "A" {
				t.Fatal("retry host callback was not pinned")
			}
			if !tc.wantErr && len(tc.want) > 1 && (*pins)[len(*pins)-1] != "" {
				t.Fatal("fallback remained pinned")
			}
		})
	}
}

func TestCredentialRetryStreamBoundary(t *testing.T) {
	for _, downstreamError := range []bool{false, true} {
		cfg := defaultPluginConfig()
		cfg.Models = []string{"test"}
		cfg.SameAuthRetries = 2
		cfg.MaxAttempts = 5
		cfg.InitialDelayMS = 1
		cfg.MaxDelayMS = 1
		calls, _ := installCredentialHost(t, cfg, true, 503, downstreamError)
		err := runModelStreamWithRetry(context.Background(), credentialTestRequest())
		if (err != nil) != downstreamError || !reflect.DeepEqual(*calls, []string{"A", "A", "A", "B"}) {
			t.Fatalf("calls=%v error=%v", *calls, err)
		}
	}
}

func TestEveryCredentialGetsItsOwnRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		retries, max, statusB, statusC int
		want                           []string
		wantErr                        bool
	}{
		{"two retries per credential", 2, 7, 503, 200, []string{"A", "A", "A", "B", "B", "B", "C"}, false},
		{"one retry per credential", 1, 5, 502, 200, []string{"A", "A", "B", "B", "C"}, false},
		{"total cap during second credential", 2, 4, 503, 200, []string{"A", "A", "A", "B"}, true},
		{"all credentials exhausted", 2, 12, 503, 503, []string{"A", "A", "A", "B", "B", "B", "C", "C", "C"}, true},
		{"authentication failover resets next budget", 2, 12, 401, 503, []string{"A", "A", "A", "B", "C", "C", "C"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultPluginConfig()
			cfg.Models = []string{"test"}
			cfg.SameAuthRetries, cfg.MaxAttempts = tc.retries, tc.max
			cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
			calls, pins := installCredentialHost(t, cfg, false, 503, false, map[string]int{"B": tc.statusB, "C": tc.statusC})
			_, err := runModelExecuteWithRetry(context.Background(), credentialTestRequest())
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(*calls, tc.want) {
				t.Fatalf("calls=%v pins=%v error=%v", *calls, *pins, err)
			}
			if tc.name == "two retries per credential" && !reflect.DeepEqual(*pins, []string{"", "A", "A", "", "B", "B", ""}) {
				t.Fatalf("second credential retries were not pinned: %v", *pins)
			}
		})
	}
}

func TestLaterCredentialStreamRetriesAndCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg := defaultPluginConfig()
		cfg.Models = []string{"test"}
		cfg.SameAuthRetries, cfg.MaxAttempts = 2, 7
		cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
		calls, _ := installCredentialHost(t, cfg, stream, 503, false, map[string]int{"B": 503, "C": 200})
		host := callHost
		cooled := false
		callHost = func(method string, payload any) (json.RawMessage, error) {
			if method == pluginabi.MethodHostModelExecute || method == pluginabi.MethodHostModelExecuteStream {
				if payload.(hostModelExecutionRequest).AuthID == "B" && !cooled {
					cooled = true
					return nil, errors.New(`{"error":{"code":"model_cooldown","reset_seconds":1}}`)
				}
			}
			return host(method, payload)
		}
		var err error
		if stream {
			err = runModelStreamWithRetry(context.Background(), credentialTestRequest())
		} else {
			_, err = runModelExecuteWithRetry(context.Background(), credentialTestRequest())
		}
		if err != nil || !cooled || !reflect.DeepEqual(*calls, []string{"A", "A", "A", "B", "B", "B", "C"}) {
			t.Fatalf("stream=%t calls=%v cooling=%t error=%v", stream, *calls, cooled, err)
		}
	}
}

func TestCredentialCooldownDoesNotConsumeRetries(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.SameAuthRetries = 2
	req := rpcExecutorRequest{}
	state, cleanup := beginCredentialRetry(&req, cfg)
	defer cleanup()
	state.prepare(&req)
	pickCredentialForTest(t, credentialRequestHeaders(req), pluginapi.SchedulerAuthCandidate{ID: "A"})
	state.failed()
	state.prepare(&req)
	err := errors.New(`host_call_failed: {"error":{"code":"model_cooldown","reset_seconds":30,"last_upstream_error":"private"}}`)
	for i := 0; i < 4; i++ {
		if delay, cooling := state.cooldownDelay(req, err, cfg, 2); !cooling || delay != 10*time.Second {
			t.Fatalf("delay=%v cooling=%v", delay, cooling)
		}
	}
	if state.failures != 1 || req.pinnedAuthID != "A" {
		t.Fatal("cooldown consumed an upstream retry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitCredentialCooldown(ctx, req, time.Hour, 2); !errors.Is(err, context.Canceled) {
		t.Fatal("cooldown wait ignored cancellation")
	}
	if _, cooling := state.cooldownDelay(req, errors.New("private body model_cooldown"), cfg, 2); cooling {
		t.Fatal("unstructured text was accepted as a cooldown deadline")
	}
	if _, cooling := state.cooldownDelay(req, errors.New(`{"error":{"code":"model_cooldown","reset_seconds":9223372036854775807}}`), cfg, 2); !cooling {
		t.Fatal("large deadline overflowed")
	}
}

func TestCredentialHeadersAndSingleCandidateExhaustion(t *testing.T) {
	input := http.Header{strings.ToLower(credentialSequenceHeader): []string{"untrusted"}, "X-Test": []string{"preserved"}}
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Headers: input}}
	if credentialRequestHeaders(req).Get(credentialSequenceHeader) != "" || input[strings.ToLower(credentialSequenceHeader)][0] != "untrusted" {
		t.Fatal("client token was trusted or original headers mutated")
	}
	cfg := defaultPluginConfig()
	cfg.SameAuthRetries = 1
	state, cleanup := beginCredentialRetry(&req, cfg)
	defer cleanup()
	for i := 0; i < 2; i++ {
		state.prepare(&req)
		pickCredentialForTest(t, credentialRequestHeaders(req), pluginapi.SchedulerAuthCandidate{ID: "A", Priority: 10})
		state.failed()
	}
	state.prepare(&req)
	if got := pickCredentialForTest(t, credentialRequestHeaders(req), pluginapi.SchedulerAuthCandidate{ID: "A", Priority: 10}); !got.Reject || !state.noAlternative() {
		t.Fatal("exhausted credential was selected again")
	}
}

func TestCredentialRuntimeCooldownAndElapsedLimit(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Models = []string{"test"}
	cfg.SameAuthRetries = 2
	cfg.InitialDelayMS, cfg.MaxDelayMS, cfg.MaxElapsedMS = 1, 1, 15
	calls, _ := installCredentialHost(t, cfg, false, 503, false)
	host := callHost
	coolingCalls := 0
	callHost = func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthList {
			return json.Marshal(struct {
				Files []pluginapi.HostAuthFileEntry `json:"files"`
			}{[]pluginapi.HostAuthFileEntry{{ID: "A", NextRetryAfter: time.Now().Add(time.Minute)}}})
		}
		if method == pluginabi.MethodHostModelExecute && payload.(hostModelExecutionRequest).AuthID == "A" {
			coolingCalls++
			return nil, errors.New("auth_unavailable: no auth available")
		}
		return host(method, payload)
	}
	_, err := runModelExecuteWithRetry(context.Background(), credentialTestRequest())
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(*calls, []string{"A"}) || coolingCalls == 0 {
		t.Fatalf("calls=%v cooling=%d error=%v", *calls, coolingCalls, err)
	}
	if statusFromError(err) != 503 {
		t.Fatal("cooldown timeout lost the original upstream status")
	}
}

func TestCredentialRetryRecognizesStreamFailureBeforePayload(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Models = []string{"test"}
	cfg.SameAuthRetries, cfg.MaxAttempts = 2, 4
	cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
	calls, _ := installCredentialHost(t, cfg, true, 200, false)
	host := callHost
	callHost = func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostModelStreamRead && len(*calls) > 0 && (*calls)[len(*calls)-1] == "A" {
			return json.Marshal(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: " + startupFailedJSON + "\n\n")})
		}
		return host(method, payload)
	}
	req := credentialTestRequest()
	req.SourceFormat, req.Format = "codex", "codex"
	if err := runModelStreamWithRetry(context.Background(), req); err != nil || !reflect.DeepEqual(*calls, []string{"A", "A", "A", "B"}) {
		t.Fatalf("calls=%v error=%v", *calls, err)
	}
}
