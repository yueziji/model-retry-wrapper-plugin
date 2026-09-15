package main

import "fmt"

// Pointers distinguish inheritance from explicit zero limits and empty lists.
type modelRetryOverride struct {
	StatusCodes    *statusCodeList `yaml:"status_codes"`
	RetryKeywords  *[]string       `yaml:"retry_keywords"`
	MaxAttempts    *int            `yaml:"max_attempts"`
	InitialDelayMS *int            `yaml:"initial_delay_ms"`
	MaxDelayMS     *int            `yaml:"max_delay_ms"`
	MaxElapsedMS   *int            `yaml:"max_elapsed_time_ms"`
}

func validateRetryConfig(cfg pluginConfig) error {
	if cfg.MaxAttempts < 0 {
		return fmt.Errorf("max_attempts must be 0 or greater")
	}
	if err := validatePositiveDurationMillis("initial_delay_ms", cfg.InitialDelayMS); err != nil {
		return err
	}
	if err := validatePositiveDurationMillis("max_delay_ms", cfg.MaxDelayMS); err != nil {
		return err
	}
	if err := validateOptionalDurationMillis("max_elapsed_time_ms", cfg.MaxElapsedMS); err != nil {
		return err
	}
	if cfg.InitialDelayMS > cfg.MaxDelayMS {
		return fmt.Errorf("initial_delay_ms must not exceed max_delay_ms")
	}
	return nil
}

func normalizeModelOverrides(cfg *pluginConfig) error {
	if cfg.ModelOverrides == nil {
		return nil
	}
	normalized := make(map[string]modelRetryOverride, len(cfg.ModelOverrides))
	for name, override := range cfg.ModelOverrides {
		model := normalizeKey(name)
		if model == "" {
			return fmt.Errorf("model_overrides model names must not be blank")
		}
		if _, exists := normalized[model]; exists {
			return fmt.Errorf("model_overrides contains duplicate normalized model %q", model)
		}
		if override.StatusCodes != nil {
			codes := statusCodeList(normalizeStatusCodes(*override.StatusCodes))
			override.StatusCodes = &codes
		}
		if override.RetryKeywords != nil {
			keywords := normalizeStringList(*override.RetryKeywords)
			override.RetryKeywords = &keywords
		}
		if err := validateRetryConfig(applyModelRetryOverride(*cfg, override)); err != nil {
			return fmt.Errorf("model_overrides[%q]: %w", name, err)
		}
		normalized[model] = override
	}
	cfg.ModelOverrides = normalized
	return nil
}

func retryConfigForModel(cfg pluginConfig, model string) pluginConfig {
	return applyModelRetryOverride(cfg, cfg.ModelOverrides[normalizeKey(model)])
}

// Each request resolves one immutable configuration snapshot before its retry loop.
func applyModelRetryOverride(cfg pluginConfig, override modelRetryOverride) pluginConfig {
	if override.StatusCodes != nil {
		cfg.StatusCodes = *override.StatusCodes
	}
	if override.RetryKeywords != nil {
		cfg.RetryKeywords = *override.RetryKeywords
	}
	if override.MaxAttempts != nil {
		cfg.MaxAttempts = *override.MaxAttempts
	}
	if override.InitialDelayMS != nil {
		cfg.InitialDelayMS = *override.InitialDelayMS
	}
	if override.MaxDelayMS != nil {
		cfg.MaxDelayMS = *override.MaxDelayMS
	}
	if override.MaxElapsedMS != nil {
		cfg.MaxElapsedMS = *override.MaxElapsedMS
	}
	return cfg
}
