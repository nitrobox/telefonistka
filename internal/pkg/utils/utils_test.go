package utils

import (
	"testing"
)

//nolint:paralleltest // t.Setenv is used in subtests, which is not compatible with t.Parallel
func TestGetEnvInt(t *testing.T) {
	const envKey = "TEST_ENV_INT"

	t.Run("env var set to valid int", func(t *testing.T) {
		t.Setenv(envKey, "42")
		result := GetEnvInt(envKey, 10)
		if result != 42 {
			t.Errorf("expected 42, got %d", result)
		}
	})

	t.Run("env var not set, fallback used", func(t *testing.T) {
		result := GetEnvInt(envKey, 99)
		if result != 99 {
			t.Errorf("expected fallback 99, got %d", result)
		}
	})

	t.Run("env var set to invalid int, fallback used", func(t *testing.T) {
		t.Setenv(envKey, "notanint")
		result := GetEnvInt(envKey, 7)
		if result != 7 {
			t.Errorf("expected fallback 7, got %d", result)
		}
	})
}
