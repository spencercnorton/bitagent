// Package llmwork owns bounded, durable optional model work. A task is source
// and policy bound; its lifecycle never substitutes for a provider receipt.
package llmwork

import (
	"fmt"
	"time"
)

type Config struct {
	Enabled         bool          `yaml:"enabled"`
	Kinds           []Kind        `yaml:"kinds"`
	WorkerEnabled   bool          `yaml:"worker_enabled"`
	PollInterval    time.Duration `yaml:"poll_interval"`
	LeaseDuration   time.Duration `yaml:"lease_duration"`
	TaskTimeout     time.Duration `yaml:"task_timeout"`
	EnqueueTimeout  time.Duration `yaml:"enqueue_timeout"`
	MaxPending      int           `yaml:"max_pending"`
	MaxTaskAge      time.Duration `yaml:"max_task_age"`
	TimeBuckets     int           `yaml:"time_buckets"`
	SpreadAdmission bool          `yaml:"spread_admission"`
}

func NewDefaultConfig() Config {
	return Config{
		Enabled: false, WorkerEnabled: true,
		Kinds:        []Kind{Type, Language, Matcher},
		PollInterval: 5 * time.Second, LeaseDuration: 5 * time.Minute,
		TaskTimeout: 3 * time.Minute, EnqueueTimeout: 100 * time.Millisecond,
		MaxPending: 2400, MaxTaskAge: 7 * 24 * time.Hour,
		TimeBuckets: 24, SpreadAdmission: true,
	}
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	seen := map[Kind]bool{}
	for _, k := range c.Kinds {
		if (k != Type && k != Language && k != Matcher) || seen[k] {
			return fmt.Errorf("llm_work: kinds must be unique supported stages")
		}
		seen[k] = true
	}
	if len(seen) == 0 {
		return fmt.Errorf("llm_work: enabled queue requires explicit stages")
	}
	if c.PollInterval < time.Second || c.LeaseDuration < time.Minute ||
		c.TaskTimeout <= 0 || c.TaskTimeout >= c.LeaseDuration ||
		c.EnqueueTimeout <= 0 || c.EnqueueTimeout > time.Second ||
		c.MaxPending < 24 || c.MaxPending > 100000 ||
		c.MaxTaskAge < time.Hour || c.MaxTaskAge > 30*24*time.Hour ||
		c.TimeBuckets < 1 || c.TimeBuckets > 24 {
		return fmt.Errorf("llm_work: invalid bounded queue configuration")
	}
	return nil
}

func (c Config) Accepts(k Kind) bool {
	if !c.Enabled {
		return false
	}
	for _, v := range c.Kinds {
		if v == k {
			return true
		}
	}
	return false
}
