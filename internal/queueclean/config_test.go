package queueclean

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewDefaultConfig_SafeOptIn(t *testing.T) {
	// The contract: a fresh deploy is a pure no-op until the operator
	// flips Enabled=true. Even then, EnablePurge must default false so
	// the first cycles produce dry-run telemetry only. Mirrors the
	// retention worker's safety pattern.
	cfg := NewDefaultConfig()

	assert.False(t, cfg.Enabled, "Enabled must default false (operator opt-in)")
	assert.False(t, cfg.EnablePurge, "EnablePurge must default false (dry-run first)")
	assert.Greater(t, cfg.Interval, time.Duration(0), "Interval must be positive")
	assert.Greater(t, cfg.RetentionAge, time.Duration(0), "RetentionAge must be positive")
	assert.Greater(t, cfg.BatchSize, 0, "BatchSize must be positive")
	assert.NotEmpty(t, cfg.PurgeStatuses, "PurgeStatuses must default to non-empty")
	assert.Contains(t, cfg.PurgeStatuses, "processed",
		"processed jobs are the primary purge target")
}

func TestNewDefaultConfig_NeverPurgesPending(t *testing.T) {
	// Pending jobs are never eligible for purge — the worker would
	// otherwise nuke jobs that the queue server hasn't processed yet.
	// Pin this contract.
	cfg := NewDefaultConfig()
	for _, s := range cfg.PurgeStatuses {
		assert.NotEqual(t, "pending", s,
			"pending status must NEVER be in default PurgeStatuses")
		assert.NotEqual(t, "running", s,
			"running status must NEVER be in default PurgeStatuses (would race the queue server)")
	}
}
