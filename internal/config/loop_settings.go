package config

import (
	"strconv"
	"time"
)

// LoopSettingsWithDefaults fills zero-valued fields with the same defaults
// the loop detector applies internally. Boot config / runtime_config only
// need to carry an override for what an operator actually changed — this
// keeps the dashboard's displayed values in sync with what's really enforced
// instead of showing misleading zeros for anything left at its default.
func LoopSettingsWithDefaults(s LoopSettingsConfig) LoopSettingsConfig {
	if s.RateThreshold <= 0 {
		s.RateThreshold = 30
	}
	if s.FingerprintWindow <= 0 {
		s.FingerprintWindow = 20
	}
	if s.FingerprintDuplicates <= 0 {
		s.FingerprintDuplicates = 5
	}
	if s.CostWindow <= 0 {
		s.CostWindow = 5 * time.Minute
	}
	if s.CostThreshold <= 0 {
		s.CostThreshold = 5.0
	}
	if s.SessionMaxRequests <= 0 {
		s.SessionMaxRequests = 100
	}
	if s.OutputLoopMode == "" {
		s.OutputLoopMode = "observe"
	}
	if s.OutputLoopThreshold <= 0 {
		s.OutputLoopThreshold = 6
	}
	if s.OutputMinSentence <= 0 {
		s.OutputMinSentence = 20
	}
	return s
}

// overrideInt sets *dst from overrides[key] if present and parses as an
// int of at least minVal; invalid or too-small values are ignored.
func overrideInt(dst *int, overrides map[string]string, key string, minVal int) {
	v, ok := overrides[key]
	if !ok {
		return
	}
	if n, err := strconv.Atoi(v); err == nil && n >= minVal {
		*dst = n
	}
}

// overrideDuration sets *dst from overrides[key] if present and parses as
// a positive time.Duration; invalid or non-positive values are ignored.
func overrideDuration(dst *time.Duration, overrides map[string]string, key string) {
	v, ok := overrides[key]
	if !ok {
		return
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		*dst = d
	}
}

// overrideFloat sets *dst from overrides[key] if present and parses as a
// float64 of at least minVal; invalid or too-small values are ignored.
func overrideFloat(dst *float64, overrides map[string]string, key string, minVal float64) {
	v, ok := overrides[key]
	if !ok {
		return
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= minVal {
		*dst = f
	}
}

// overrideString sets *dst from overrides[key] if present and non-empty.
func overrideString(dst *string, overrides map[string]string, key string) {
	if v, ok := overrides[key]; ok && v != "" {
		*dst = v
	}
}

// ApplyLoopSettingsOverrides merges persisted runtime_config values (section
// "loop_settings") on top of base. Unknown/invalid/zero values are ignored so
// a partial or corrupt override map can't zero out a field.
func ApplyLoopSettingsOverrides(base LoopSettingsConfig, overrides map[string]string) LoopSettingsConfig {
	out := base
	overrideInt(&out.RateThreshold, overrides, "rate_threshold", 1)
	overrideInt(&out.FingerprintWindow, overrides, "fingerprint_window", 1)
	overrideInt(&out.FingerprintDuplicates, overrides, "fingerprint_duplicates", 1)
	overrideDuration(&out.CostWindow, overrides, "cost_window")
	overrideFloat(&out.CostThreshold, overrides, "cost_threshold", 0)
	overrideInt(&out.SessionMaxRequests, overrides, "session_max_requests", 1)
	overrideString(&out.OutputLoopMode, overrides, "output_loop_mode")
	overrideInt(&out.OutputLoopThreshold, overrides, "output_loop_threshold", 2)
	overrideInt(&out.OutputMinSentence, overrides, "output_min_sentence_len", 1)
	return out
}

// LoopSettingsToRuntimeConfigValues flattens a LoopSettingsConfig into the
// section="loop_settings" key/value rows persisted to runtime_config.
func LoopSettingsToRuntimeConfigValues(s LoopSettingsConfig) map[string]string {
	return map[string]string{
		"rate_threshold":          strconv.Itoa(s.RateThreshold),
		"fingerprint_window":      strconv.Itoa(s.FingerprintWindow),
		"fingerprint_duplicates":  strconv.Itoa(s.FingerprintDuplicates),
		"cost_window":             s.CostWindow.String(),
		"cost_threshold":          strconv.FormatFloat(s.CostThreshold, 'f', -1, 64),
		"session_max_requests":    strconv.Itoa(s.SessionMaxRequests),
		"output_loop_mode":        s.OutputLoopMode,
		"output_loop_threshold":   strconv.Itoa(s.OutputLoopThreshold),
		"output_min_sentence_len": strconv.Itoa(s.OutputMinSentence),
	}
}
