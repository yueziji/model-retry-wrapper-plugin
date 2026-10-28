//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
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
		{"all credentials start another round", 2, 12, 503, 503, []string{"A", "A", "A", "B", "B", "B", "C", "C", "C", "A", "A", "A"}, true},
		{"authentication failover excludes across rounds", 2, 12, 401, 503, []string{"A", "A", "A", "B", "C", "C", "C", "A", "A", "A", "C", "C"}, true},
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
	state.failed(true)
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
		state.failed(true)
	}
	state.prepare(&req)
	if got := pickCredentialForTest(t, credentialRequestHeaders(req), pluginapi.SchedulerAuthCandidate{ID: "A", Priority: 10}); got.AuthID != "A" || state.round != 2 || state.failures != 0 {
		t.Fatal("exhausted credential did not receive a new round's budget")
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

// Reuse the existing transport stub, with configurable candidates and failures.
// Native failover asks for a second pick; the wrapper must defer that pick.
func installCredentialRoundHost(t *testing.T, cfg pluginConfig, stream bool, candidates []pluginapi.SchedulerAuthCandidate, execute func(string, int) error) *[]string {
	t.Helper()
	calls, _ := installCredentialHost(t, cfg, stream, 200, false)
	host := callHost
	callHost = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostModelExecute && method != pluginabi.MethodHostModelExecuteStream {
			return host(method, payload)
		}
		req := payload.(hostModelExecutionRequest)
		available := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if req.AuthID == "" || req.AuthID == candidate.ID {
				available = append(available, candidate)
			}
		}
		pick := pickCredentialForTest(t, req.Headers, available...)
		if !pick.Handled || pick.Reject {
			return nil, errors.New("auth_unavailable: no available credential")
		}
		*calls = append(*calls, pick.AuthID)
		if err := execute(pick.AuthID, len(*calls)); err != nil {
			var remaining []pluginapi.SchedulerAuthCandidate
			for _, candidate := range available {
				if candidate.ID != pick.AuthID {
					remaining = append(remaining, candidate)
				}
			}
			if len(remaining) > 0 && !pickCredentialForTest(t, req.Headers, remaining...).Reject {
				t.Fatal("native failover bypassed the wrapper attempt budget")
			}
			return nil, err
		}
		if stream {
			return json.Marshal(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "stream"})
		}
		return json.Marshal(pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte("success")})
	}
	return calls
}

func runCredentialRoundTest(ctx context.Context, stream bool) error {
	if stream {
		return runModelStreamWithRetry(ctx, credentialTestRequest())
	}
	_, err := runModelExecuteWithRetry(ctx, credentialTestRequest())
	return err
}

func TestCredentialRetryRounds(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			two          bool
			max, success int
			want         string
		}{
			{"single second round", false, 0, 4, "AAAA"},
			{"single third round", false, 0, 7, "AAAAAAA"},
			{"two second round", true, 0, 8, "AAABBBAA"},
			{"eight attempt cap", true, 8, 0, "AAABBBAA"},
			{"success at cap", true, 8, 8, "AAABBBAA"},
			{"cap before round ends", false, 2, 0, "AA"},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				cfg := defaultPluginConfig()
				cfg.Models, cfg.SameAuthRetries, cfg.MaxAttempts = []string{"test"}, 2, tc.max
				cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
				candidates := []pluginapi.SchedulerAuthCandidate{{ID: "A", Priority: 10}}
				if tc.two {
					candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: "B", Priority: 1})
				}
				calls := installCredentialRoundHost(t, cfg, stream, candidates, func(_ string, attempt int) error {
					if attempt == tc.success {
						return nil
					}
					return retryStatusError{status: 503, err: errors.New("upstream failed")}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				err := runCredentialRoundTest(ctx, stream)
				if strings.Join(*calls, "") != tc.want || (err != nil) != (tc.success == 0) {
					t.Fatalf("calls=%v error=%v", *calls, err)
				}
				if err != nil && (statusFromError(err) != 503 || !strings.Contains(err.Error(), "max_attempts") || !strings.Contains(err.Error(), "upstream failed")) {
					t.Fatalf("attempt cap lost status, cause or upstream error: %v", err)
				}
			})
		}
	}
}

func TestCredentialRoundsCooldownWithoutSchedulerCallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, runtime := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/runtime=%t", stream, runtime), func(t *testing.T) {
				cfg := defaultPluginConfig()
				cfg.Models, cfg.SameAuthRetries, cfg.MaxAttempts = []string{"test"}, 1, 3
				cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
				calls := installCredentialRoundHost(t, cfg, stream, []pluginapi.SchedulerAuthCandidate{{ID: "A"}}, func(_ string, attempt int) error {
					if attempt == 3 {
						return nil
					}
					return retryStatusError{status: 503}
				})
				host := callHost
				checks := 0
				callHost = func(method string, payload any) (json.RawMessage, error) {
					if method == pluginabi.MethodHostAuthList {
						return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{{ID: "A", NextRetryAfter: time.Now().Add(time.Minute)}}})
					}
					if (method == pluginabi.MethodHostModelExecute || method == pluginabi.MethodHostModelExecuteStream) && len(*calls) == 2 && checks < 3 {
						if payload.(hostModelExecutionRequest).AuthID != "" {
							t.Fatal("round transition retained its pin")
						}
						checks++
						if runtime {
							return nil, errors.New("auth_unavailable: no available credential")
						}
						return nil, errors.New(`{"error":{"code":"model_cooldown","reset_seconds":1}}`)
					}
					return host(method, payload)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				started := time.Now()
				err := runCredentialRoundTest(ctx, stream)
				if err != nil || checks != 3 || strings.Join(*calls, "") != "AAA" || time.Since(started) < 5*time.Millisecond {
					t.Fatalf("calls=%v cooldown checks=%d error=%v", *calls, checks, err)
				}
			})
		}
	}
}

func TestCredentialRoundTerminationPreservesUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, reason := range []string{"canceled", "deadline", "no candidates", "disabled", "unrelated cooldown"} {
			t.Run(fmt.Sprintf("%s/stream=%t", reason, stream), func(t *testing.T) {
				cfg := defaultPluginConfig()
				cfg.Models, cfg.SameAuthRetries = []string{"test"}, 1
				cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
				if reason == "deadline" {
					cfg.MaxElapsedMS = 40
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				upstreamErr := retryStatusError{status: 502, err: errors.New("last upstream failure")}
				calls := installCredentialRoundHost(t, cfg, stream, []pluginapi.SchedulerAuthCandidate{{ID: "A"}}, func(_ string, attempt int) error {
					if reason == "canceled" && attempt == 4 {
						cancel()
					}
					return upstreamErr
				})
				host := callHost
				callHost = func(method string, payload any) (json.RawMessage, error) {
					if reason != "canceled" && len(*calls) >= 3 {
						if method == pluginabi.MethodHostAuthList {
							entry := pluginapi.HostAuthFileEntry{ID: "A", Disabled: reason == "disabled", NextRetryAfter: time.Now().Add(time.Minute)}
							if reason == "unrelated cooldown" {
								entry.ID = "other-model"
							}
							if reason == "no candidates" {
								return json.RawMessage(`{"files":[]}`), nil
							}
							return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{entry}})
						}
						if method == pluginabi.MethodHostModelExecute || method == pluginabi.MethodHostModelExecuteStream {
							return nil, errors.New("auth_unavailable: no available credential")
						}
					}
					return host(method, payload)
				}
				err := runCredentialRoundTest(ctx, stream)
				if err == nil || statusFromError(err) != 502 || !strings.Contains(err.Error(), upstreamErr.Error()) {
					t.Fatalf("calls=%v error=%v", *calls, err)
				}
				switch reason {
				case "canceled":
					if !errors.Is(err, context.Canceled) || len(*calls) != 4 {
						t.Fatalf("calls=%v error=%v", *calls, err)
					}
				case "deadline":
					if !errors.Is(err, context.DeadlineExceeded) || len(*calls) != 3 {
						t.Fatalf("calls=%v error=%v", *calls, err)
					}
				default:
					if !strings.Contains(err.Error(), "auth_unavailable") || errors.Is(err, context.DeadlineExceeded) || len(*calls) != 3 {
						t.Fatalf("calls=%v error=%v", *calls, err)
					}
				}
			})
		}
	}
}

func TestCredentialRoundsStopWhenAllExcluded(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg := defaultPluginConfig()
		cfg.Models, cfg.SameAuthRetries = []string{"test"}, 2
		cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
		calls := installCredentialRoundHost(t, cfg, stream, []pluginapi.SchedulerAuthCandidate{{ID: "A", Priority: 10}, {ID: "B"}}, func(id string, _ int) error {
			return retryStatusError{status: 401, err: fmt.Errorf("upstream rejected %s", id)}
		})
		host := callHost
		callHost = func(method string, payload any) (json.RawMessage, error) {
			if len(*calls) == 2 && (method == pluginabi.MethodHostModelExecute || method == pluginabi.MethodHostModelExecuteStream) {
				return nil, errors.New(`{"error":{"code":"model_cooldown","reset_seconds":60}}`)
			}
			return host(method, payload)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := runCredentialRoundTest(ctx, stream)
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) || statusFromError(err) != 401 || strings.Join(*calls, "") != "AB" || !strings.Contains(err.Error(), "upstream rejected B") {
			t.Fatalf("stream=%t calls=%v error=%v", stream, *calls, err)
		}
	}
}

func TestConcurrentCredentialRoundsAndFreshRequest(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.SameAuthRetries = 1
	candidates := []pluginapi.SchedulerAuthCandidate{{ID: "B", Priority: 1}, {ID: "A", Priority: 1}}
	var requests sync.WaitGroup
	for i := 0; i < 8; i++ {
		requests.Go(func() {
			req := rpcExecutorRequest{}
			state, cleanup := beginCredentialRetry(&req, cfg)
			defer cleanup()
			var order string
			for j := 0; j < 8; j++ {
				state.prepare(&req)
				order += pickCredentialForTest(t, credentialRequestHeaders(req), candidates...).AuthID
				state.failed(true)
			}
			if order != "AABBAABB" {
				t.Errorf("concurrent request inherited credential state: %s", order)
			}
		})
	}
	requests.Wait()
	req := rpcExecutorRequest{}
	state, cleanup := beginCredentialRetry(&req, cfg)
	defer cleanup()
	state.prepare(&req)
	if got := pickCredentialForTest(t, credentialRequestHeaders(req), candidates...); got.AuthID != "A" {
		t.Fatalf("fresh request did not start with highest priority and ID: %q", got.AuthID)
	}
}

func TestCredentialEmptySelectionsDoNotConsumeAttemptCap(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, pinnedUnavailable := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/pinned_unavailable=%t", stream, pinnedUnavailable), func(t *testing.T) {
				cfg := defaultPluginConfig()
				cfg.Models, cfg.SameAuthRetries, cfg.MaxAttempts = []string{"test"}, 2, 1
				cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
				if pinnedUnavailable {
					cfg.MaxAttempts = 2
				}
				calls := installCredentialRoundHost(t, cfg, stream, []pluginapi.SchedulerAuthCandidate{{ID: "A", Priority: 10}, {ID: "B"}}, func(id string, _ int) error {
					if pinnedUnavailable && id == "A" {
						return retryStatusError{status: 503}
					}
					return nil
				})
				host := callHost
				checks := 0
				callHost = func(method string, payload any) (json.RawMessage, error) {
					if method == pluginabi.MethodHostModelExecute || method == pluginabi.MethodHostModelExecuteStream {
						if !pinnedUnavailable && checks < 2 {
							checks++
							return nil, errors.New(`{"error":{"code":"model_cooldown","reset_seconds":1}}`)
						}
						if pinnedUnavailable && payload.(hostModelExecutionRequest).AuthID == "A" {
							checks++
							return nil, errors.New("auth_unavailable: pinned credential removed")
						}
					}
					return host(method, payload)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				err := runCredentialRoundTest(ctx, stream)
				want, wantChecks := "A", 2
				if pinnedUnavailable {
					want, wantChecks = "AB", 1
				}
				if err != nil || strings.Join(*calls, "") != want || checks != wantChecks {
					t.Fatalf("calls=%v empty checks=%d error=%v", *calls, checks, err)
				}
			})
		}
	}
}

func TestCredentialRoundBackoffContinues(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg := defaultPluginConfig()
		cfg.Models, cfg.SameAuthRetries, cfg.MaxAttempts = []string{"test"}, 1, 5
		cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 8
		installCredentialRoundHost(t, cfg, stream, []pluginapi.SchedulerAuthCandidate{{ID: "A"}}, func(_ string, attempt int) error {
			if attempt == 5 {
				return nil
			}
			return retryStatusError{status: 503}
		})
		host := callHost
		var delays []int64
		callHost = func(method string, payload any) (json.RawMessage, error) {
			if method == pluginabi.MethodHostLog {
				if delay, ok := payload.(rpcHostLogRequest).Fields["delay_ms"].(int64); ok {
					delays = append(delays, delay)
				}
			}
			return host(method, payload)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := runCredentialRoundTest(ctx, stream)
		cancel()
		if err != nil || !reflect.DeepEqual(delays, []int64{1, 2, 4, 8}) {
			t.Fatalf("stream=%t delays=%v error=%v", stream, delays, err)
		}
	}
}

func TestCredentialRoundsWithStreamStartupEvents(t *testing.T) {
	for _, afterPayloadError := range []bool{false, true} {
		cfg := defaultPluginConfig()
		cfg.Models, cfg.SameAuthRetries, cfg.MaxAttempts = []string{"test"}, 2, 8
		cfg.InitialDelayMS, cfg.MaxDelayMS = 1, 1
		calls := installCredentialRoundHost(t, cfg, true, []pluginapi.SchedulerAuthCandidate{{ID: "A", Priority: 10}, {ID: "B"}}, func(string, int) error { return nil })
		host := callHost
		forwardedReads := 0
		callHost = func(method string, payload any) (json.RawMessage, error) {
			if method == pluginabi.MethodHostModelStreamRead {
				if len(*calls) <= 6 {
					return json.Marshal(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: " + startupFailedJSON + "\n\n")})
				}
				forwardedReads++
				if forwardedReads > 1 && afterPayloadError {
					return json.Marshal(pluginapi.HostModelStreamReadResponse{Error: "HTTP 503 after payload"})
				}
			}
			return host(method, payload)
		}
		req := credentialTestRequest()
		req.SourceFormat, req.Format = "codex", "codex"
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := runModelStreamWithRetry(ctx, req)
		cancel()
		if (err != nil) != afterPayloadError || strings.Join(*calls, "") != "AAABBBA" {
			t.Fatalf("calls=%v error=%v", *calls, err)
		}
	}
}

func TestCredentialRoundsUseModelOverride(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg, err := decodeConfig([]byte("models: [test]\nsame_auth_retries: 2\nmax_attempts: 5\ninitial_delay_ms: 1\nmax_delay_ms: 1\nmodel_overrides: {test: {same_auth_retries: 1}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		calls := installCredentialRoundHost(t, cfg, stream, []pluginapi.SchedulerAuthCandidate{{ID: "A", Priority: 10}, {ID: "B"}}, func(_ string, attempt int) error {
			if attempt == 5 {
				return nil
			}
			return retryStatusError{status: 503}
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = runCredentialRoundTest(ctx, stream)
		cancel()
		if err != nil || strings.Join(*calls, "") != "AABBA" {
			t.Fatalf("stream=%t calls=%v error=%v", stream, *calls, err)
		}
	}
}

func TestCredentialRoundCanRestartWhileUnusedCredentialCools(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.SameAuthRetries = 1
	req := rpcExecutorRequest{}
	state, cleanup := beginCredentialRetry(&req, cfg)
	defer cleanup()
	available := []pluginapi.SchedulerAuthCandidate{{ID: "A", Priority: 10}, {ID: "B"}}
	for i := 0; i < 2; i++ {
		state.prepare(&req)
		if got := pickCredentialForTest(t, credentialRequestHeaders(req), available...); got.AuthID != "A" {
			t.Fatalf("first round selected %q", got.AuthID)
		}
		state.failed(true)
	}
	// B was observed earlier but is now cooling, so CPA offers only A.
	state.prepare(&req)
	if got := pickCredentialForTest(t, credentialRequestHeaders(req), available[0]); got.AuthID != "A" || state.round != 2 {
		t.Fatal("a cooling unused credential blocked the next round")
	}
	if got := pickCredentialForTest(t, credentialRequestHeaders(req), available[1]); !got.Reject {
		t.Fatal("native retry dispatched twice inside the new round's host call")
	}
}

func TestZeroCredentialRetriesRetainNativeSelection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg, err := decodeConfig([]byte("models: [test]\nsame_auth_retries: 2\nmax_attempts: 2\ninitial_delay_ms: 1\nmax_delay_ms: 1\nmodel_overrides: {test: {same_auth_retries: 0}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		installCredentialHost(t, cfg, stream, 200, false)
		host, attempts := callHost, 0
		callHost = func(method string, payload any) (json.RawMessage, error) {
			if method == pluginabi.MethodHostModelExecute || method == pluginabi.MethodHostModelExecuteStream {
				req := payload.(hostModelExecutionRequest)
				if req.AuthID != "" || req.Headers.Get(credentialSequenceHeader) != "" {
					t.Fatal("zero override enabled credential scheduling")
				}
				attempts++
				if attempts == 1 {
					return nil, retryStatusError{status: 503}
				}
				if stream {
					return json.Marshal(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "stream"})
				}
				return json.Marshal(pluginapi.HostModelExecutionResponse{StatusCode: 200})
			}
			return host(method, payload)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = runCredentialRoundTest(ctx, stream)
		cancel()
		if err != nil || attempts != 2 {
			t.Fatalf("stream=%t attempts=%d error=%v", stream, attempts, err)
		}
	}
}
