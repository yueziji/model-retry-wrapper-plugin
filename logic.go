package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var currentConfig atomic.Value

// lifecycleSchemaVersion is the minimum negotiated schema that delivers request.complete events.
const lifecycleSchemaVersion = 2

var hostSchemaVersion atomic.Uint32

// retryRequestIDHeader is an internal correlation header added by the request
// interceptor and removed before the nested host model call reaches upstream.
const retryRequestIDHeader = "X-Model-Retry-Wrapper-Request-Id"

// The marker survives other plugins' host callbacks so this wrapper runs once per chain.
const retryAppliedHeader = "X-Model-Retry-Wrapper-Applied"

const maxRequestIDLength = 128

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type pluginConfig struct {
	Enabled        bool           `yaml:"enabled"`
	Models         []string       `yaml:"models"`
	SourceFormats  []string       `yaml:"source_formats"`
	StatusCodes    statusCodeList `yaml:"status_codes"`
	RetryKeywords  []string       `yaml:"retry_keywords"`
	MaxAttempts    int            `yaml:"max_attempts"`
	InitialDelayMS int            `yaml:"initial_delay_ms"`
	MaxDelayMS     int            `yaml:"max_delay_ms"`
	MaxElapsedMS   int            `yaml:"max_elapsed_time_ms"`
}

type statusCodeList []int

func supportedExecutorFormats() []string {
	return []string{"openai", "openai-response", "claude", "gemini", "chat-completions", "codex"}
}

type retryStatusError struct {
	status int
	err    error
}

const maxDurationMillis = int64(math.MaxInt64) / int64(time.Millisecond)

var explicitHTTPStatusPattern = regexp.MustCompile(`(?i)\b(?:http(?:\s+(?:response\s+)?status(?:\s+code)?)?|status(?:\s+code)?)\s*[:=]?\s*([45][0-9]{2})\b`)

func (e retryStatusError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	if e.status > 0 {
		return fmt.Sprintf("host model status %d", e.status)
	}
	return "host model execution failed"
}

func (e retryStatusError) StatusCode() int {
	return e.status
}

func (e retryStatusError) Unwrap() error {
	return e.err
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	hostSchemaVersion.Store(negotiatedSchemaVersion(req.SchemaVersion))
	currentConfig.Store(cfg)
	return nil
}

// negotiatedSchemaVersion caps the advertised schema at what both sides support.
// Hosts predating schema negotiation omit schema_version and are treated as schema 1.
func negotiatedSchemaVersion(hostVersion uint32) uint32 {
	if hostVersion == 0 {
		return 1
	}
	if hostVersion > pluginabi.SchemaVersion {
		return pluginabi.SchemaVersion
	}
	return hostVersion
}

func lifecycleEventsSupported(schemaVersion uint32) bool {
	return schemaVersion >= lifecycleSchemaVersion
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:        true,
		StatusCodes:    []int{408, 429, 500, 502, 503, 504},
		RetryKeywords:  []string{"rate_limited"},
		MaxAttempts:    0,
		InitialDelayMS: 500,
		MaxDelayMS:     10000,
		MaxElapsedMS:   0,
	}
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	cfg.Models = normalizeStringList(cfg.Models)
	cfg.SourceFormats = normalizeSourceFormatList(cfg.SourceFormats)
	cfg.StatusCodes = normalizeStatusCodes(cfg.StatusCodes)
	cfg.RetryKeywords = normalizeStringList(cfg.RetryKeywords)
	if cfg.MaxAttempts < 0 {
		return pluginConfig{}, fmt.Errorf("max_attempts must be 0 or greater")
	}
	if errValidate := validatePositiveDurationMillis("initial_delay_ms", cfg.InitialDelayMS); errValidate != nil {
		return pluginConfig{}, errValidate
	}
	if errValidate := validatePositiveDurationMillis("max_delay_ms", cfg.MaxDelayMS); errValidate != nil {
		return pluginConfig{}, errValidate
	}
	if errValidate := validateOptionalDurationMillis("max_elapsed_time_ms", cfg.MaxElapsedMS); errValidate != nil {
		return pluginConfig{}, errValidate
	}
	if cfg.InitialDelayMS > cfg.MaxDelayMS {
		return pluginConfig{}, fmt.Errorf("initial_delay_ms must not exceed max_delay_ms")
	}
	return cfg, nil
}

func validatePositiveDurationMillis(name string, value int) error {
	if value <= 0 {
		return fmt.Errorf("%s must be greater than 0", name)
	}
	if int64(value) > maxDurationMillis {
		return fmt.Errorf("%s exceeds the maximum supported duration", name)
	}
	return nil
}

func validateOptionalDurationMillis(name string, value int) error {
	if value < 0 {
		return fmt.Errorf("%s must be 0 or greater", name)
	}
	if int64(value) > maxDurationMillis {
		return fmt.Errorf("%s exceeds the maximum supported duration", name)
	}
	return nil
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

func shouldRoute(cfg pluginConfig, sourceFormat string, model string) bool {
	if !cfg.Enabled || len(cfg.Models) == 0 {
		return false
	}
	if len(cfg.SourceFormats) > 0 && !stringListContains(cfg.SourceFormats, normalizeSourceFormat(sourceFormat)) {
		return false
	}
	return stringListContains(cfg.Models, normalizeKey(model))
}

func routeSkipReason(cfg pluginConfig, sourceFormat string, model string) string {
	switch {
	case !cfg.Enabled:
		return "disabled"
	case len(cfg.Models) == 0:
		return "no_models_configured"
	case len(cfg.SourceFormats) > 0 && !stringListContains(cfg.SourceFormats, normalizeSourceFormat(sourceFormat)):
		return "source_format_not_configured"
	case !stringListContains(cfg.Models, normalizeKey(model)):
		return "model_not_configured"
	default:
		return "not_handled"
	}
}

func shouldRetryAttempt(cfg pluginConfig, attempt int, status int) bool {
	if !shouldRetryStatus(cfg, status) {
		return false
	}
	if cfg.MaxAttempts == 0 {
		return true
	}
	return attempt < cfg.MaxAttempts
}

func shouldRetryStatus(cfg pluginConfig, status int) bool {
	if status <= 0 {
		return false
	}
	for _, code := range cfg.StatusCodes {
		if code == status {
			return true
		}
	}
	return false
}

func shouldRetryFailure(cfg pluginConfig, attempt int, status int, err error) (bool, string) {
	if shouldRetryStatus(cfg, status) {
		return shouldRetryAttempt(cfg, attempt, status), ""
	}
	if status > 0 {
		return false, ""
	}
	keyword := retryKeywordFromError(cfg, err)
	if keyword == "" {
		return false, ""
	}
	if cfg.MaxAttempts == 0 {
		return true, keyword
	}
	return attempt < cfg.MaxAttempts, keyword
}

func retryKeywordFromError(cfg pluginConfig, err error) string {
	if err == nil || len(cfg.RetryKeywords) == 0 {
		return ""
	}
	text := normalizeKey(err.Error())
	var startupErr *streamStartupError
	if errors.As(err, &startupErr) {
		// Match the upstream details in memory; Error() deliberately omits them from logs.
		text = normalizeKey(startupErr.details)
	}
	if text == "" {
		return ""
	}
	for _, keyword := range cfg.RetryKeywords {
		keyword = normalizeKey(keyword)
		if keyword != "" && strings.Contains(text, keyword) {
			return keyword
		}
	}
	return ""
}

func retryDelay(cfg pluginConfig, attempt int) time.Duration {
	base := durationFromMillis(cfg.InitialDelayMS)
	if attempt <= 1 || base == 0 {
		return base
	}
	maxDelay := durationFromMillis(cfg.MaxDelayMS)
	delay := base
	for i := 1; i < attempt; i++ {
		if maxDelay > 0 && delay >= maxDelay {
			return maxDelay
		}
		if delay > time.Duration(math.MaxInt64)/2 {
			if maxDelay > 0 {
				return maxDelay
			}
			return time.Duration(math.MaxInt64)
		}
		delay *= 2
		if maxDelay > 0 && delay > maxDelay {
			return maxDelay
		}
	}
	return delay
}

func durationFromMillis(value int) time.Duration {
	if value <= 0 {
		return 0
	}
	if int64(value) > maxDurationMillis {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(value) * time.Millisecond
}

func waitRetryDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func logFieldsWith(fields map[string]any, key string, value any) map[string]any {
	next := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		next[k] = v
	}
	next[key] = value
	return next
}

func shortError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if len(text) <= 240 {
		return text
	}
	return text[:240] + "..."
}

func durationMillis(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	if status := structuredStatusFromError(err); status > 0 {
		return status
	}
	match := explicitHTTPStatusPattern.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return 0
	}
	code, errAtoi := strconv.Atoi(match[1])
	if errAtoi != nil || code < 400 || code > 599 {
		return 0
	}
	return code
}

func structuredStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) && statusErr != nil {
		if status := statusErr.StatusCode(); status >= 100 && status <= 599 {
			return status
		}
	}
	return 0
}

func errorWithStatus(err error, status int) error {
	if err == nil || status < 100 || status > 599 {
		return err
	}
	if structuredStatusFromError(err) == status {
		return err
	}
	return retryStatusError{status: status, err: err}
}

func retryTerminationError(lastErr error, terminationErr error) error {
	if lastErr == nil {
		return terminationErr
	}
	wrapped := fmt.Errorf("retry stopped: %w; last error: %w", terminationErr, lastErr)
	return errorWithStatus(wrapped, statusFromError(lastErr))
}

func requestBody(req pluginapi.ExecutorRequest) []byte {
	if len(req.OriginalRequest) > 0 {
		return append([]byte(nil), req.OriginalRequest...)
	}
	return append([]byte(nil), req.Payload...)
}

func entryProtocol(req pluginapi.ExecutorRequest) string {
	if strings.TrimSpace(req.SourceFormat) != "" {
		return strings.TrimSpace(req.SourceFormat)
	}
	if strings.TrimSpace(req.Format) != "" {
		return strings.TrimSpace(req.Format)
	}
	return "openai"
}

func exitProtocol(req pluginapi.ExecutorRequest) string {
	if strings.TrimSpace(req.Format) != "" {
		return strings.TrimSpace(req.Format)
	}
	return entryProtocol(req)
}

func streamContentType(sourceFormat string) string {
	switch normalizeKey(sourceFormat) {
	case "claude", "anthropic":
		return "text/event-stream"
	case "gemini":
		return "application/json"
	default:
		return "text/event-stream"
	}
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := normalizeKey(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func normalizeSourceFormatList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := normalizeSourceFormat(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func normalizeSourceFormat(value string) string {
	key := normalizeKey(value)
	switch key {
	case "response", "responses", "openai-responses":
		return "openai-response"
	default:
		return key
	}
}

func normalizeStatusCodes(values []int) []int {
	out := make([]int, 0, len(values))
	seen := make(map[int]struct{}, len(values))
	for _, code := range values {
		if code < 100 || code > 599 {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

func (codes *statusCodeList) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == 0 || value.Tag == "!!null" {
		*codes = nil
		return nil
	}
	switch value.Kind {
	case yaml.SequenceNode:
		out := make([]int, 0, len(value.Content))
		for _, item := range value.Content {
			code, ok, errParse := parseStatusCodeYAMLNode(item)
			if errParse != nil {
				return errParse
			}
			if ok {
				out = append(out, code)
			}
		}
		*codes = out
		return nil
	case yaml.ScalarNode:
		code, ok, errParse := parseStatusCodeYAMLNode(value)
		if errParse != nil {
			return errParse
		}
		if !ok {
			*codes = nil
			return nil
		}
		*codes = []int{code}
		return nil
	default:
		return fmt.Errorf("status_codes must be a status code or a list of status codes")
	}
}

func parseStatusCodeYAMLNode(value *yaml.Node) (int, bool, error) {
	if value == nil || value.Kind == 0 || value.Tag == "!!null" {
		return 0, false, nil
	}
	if value.Kind != yaml.ScalarNode {
		return 0, false, fmt.Errorf("status_codes entries must be scalar values")
	}
	text := strings.TrimSpace(value.Value)
	if text == "" {
		return 0, false, nil
	}
	code, errAtoi := strconv.Atoi(text)
	if errAtoi != nil {
		return 0, false, fmt.Errorf("status_codes contains non-numeric value %q", value.Value)
	}
	return code, true, nil
}

func normalizeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxRequestIDLength {
		return ""
	}
	return value
}

func requestIDFromHeaders(headers http.Header) string {
	for key, values := range headers {
		if !strings.EqualFold(key, retryRequestIDHeader) {
			continue
		}
		for _, value := range values {
			if requestID := normalizeRequestID(value); requestID != "" {
				return requestID
			}
		}
	}
	return ""
}

func stripRequestIDHeader(headers http.Header) http.Header {
	cloned := cloneHeader(headers)
	for key := range cloned {
		if strings.EqualFold(key, retryRequestIDHeader) {
			delete(cloned, key)
		}
	}
	return cloned
}

func hasRetryMarker(headers http.Header) bool {
	for key, values := range headers {
		if strings.EqualFold(key, retryAppliedHeader) {
			for _, value := range values {
				if strings.TrimSpace(value) == "1" {
					return true
				}
			}
		}
	}
	return false
}

func nestedRequestHeaders(headers http.Header) http.Header {
	cloned := stripRequestIDHeader(headers)
	if cloned == nil {
		cloned = make(http.Header)
	}
	for key := range cloned {
		if strings.EqualFold(key, retryAppliedHeader) {
			delete(cloned, key)
		}
	}
	cloned.Set(retryAppliedHeader, "1")
	return cloned
}

func stringListContains(values []string, needle string) bool {
	needle = normalizeKey(needle)
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func cloneHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	cloned := make(http.Header, len(headers))
	for key, values := range headers {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func cloneValues(values map[string][]string) map[string][]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string][]string, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}
