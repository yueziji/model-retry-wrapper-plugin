package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var currentConfig atomic.Value

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
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
}

type statusCodeList []int

func supportedExecutorFormats() []string {
	return []string{"openai", "openai-response", "claude", "gemini", "chat-completions"}
}

type retryStatusError struct {
	status int
	err    error
}

func (e retryStatusError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	if e.status > 0 {
		return fmt.Sprintf("host model status %d", e.status)
	}
	return "host model execution failed"
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
	currentConfig.Store(cfg)
	pluginLog("", "info", "model-retry-wrapper: configured", map[string]any{
		"enabled":          cfg.Enabled,
		"models":           cfg.Models,
		"source_formats":   cfg.SourceFormats,
		"status_codes":     []int(cfg.StatusCodes),
		"retry_keywords":   cfg.RetryKeywords,
		"max_attempts":     cfg.MaxAttempts,
		"initial_delay_ms": cfg.InitialDelayMS,
		"max_delay_ms":     cfg.MaxDelayMS,
		"executor_formats": supportedExecutorFormats(),
	})
	return nil
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:        true,
		StatusCodes:    []int{408, 429, 500, 502, 503, 504},
		RetryKeywords:  []string{"rate_limited"},
		MaxAttempts:    0,
		InitialDelayMS: 500,
		MaxDelayMS:     10000,
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
	if len(cfg.RetryKeywords) == 0 {
		cfg.RetryKeywords = defaultPluginConfig().RetryKeywords
	}
	if cfg.InitialDelayMS < 0 {
		cfg.InitialDelayMS = 0
	}
	if cfg.MaxDelayMS < 0 {
		cfg.MaxDelayMS = 0
	}
	if cfg.MaxDelayMS > 0 && cfg.InitialDelayMS > cfg.MaxDelayMS {
		cfg.InitialDelayMS = cfg.MaxDelayMS
	}
	return cfg, nil
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
	base := time.Duration(cfg.InitialDelayMS) * time.Millisecond
	if base < 0 {
		base = 0
	}
	if attempt <= 1 || base == 0 {
		return base
	}
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
		maxDelay := time.Duration(cfg.MaxDelayMS) * time.Millisecond
		if maxDelay > 0 && delay > maxDelay {
			return maxDelay
		}
	}
	return delay
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

func waitRetryDelayWithProbe(ctx context.Context, delay time.Duration, probe func() error) error {
	if probe != nil {
		if err := probe(); err != nil {
			return err
		}
	}
	if delay <= 0 {
		return nil
	}
	remaining := delay
	for remaining > 0 {
		step := remaining
		if step > time.Second {
			step = time.Second
		}
		if err := waitRetryDelay(ctx, step); err != nil {
			return err
		}
		remaining -= step
		if probe != nil {
			if err := probe(); err != nil {
				return err
			}
		}
	}
	return nil
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
	var retryErr retryStatusError
	if asRetryStatusError(err, &retryErr) && retryErr.status > 0 {
		return retryErr.status
	}
	text := err.Error()
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r < '0' || r > '9'
	})
	for _, field := range fields {
		if len(field) != 3 {
			continue
		}
		code, errAtoi := strconv.Atoi(field)
		if errAtoi == nil && code >= 100 && code <= 599 {
			return code
		}
	}
	return 0
}

func asRetryStatusError(err error, target *retryStatusError) bool {
	if err == nil || target == nil {
		return false
	}
	if value, ok := err.(retryStatusError); ok {
		*target = value
		return true
	}
	return false
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
