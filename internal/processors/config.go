// Package processors owns optional service registration, durable execution and
// result application. It has no model/provider/framework dependencies.
package processors

import (
	"fmt"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"time"
)

type Config struct {
	CredentialDirectory string
	AdminToken          namepolicy.SecretToken
	Concurrency         int
	QueueCapacity       int
	RetainedJobs        int
	PollInterval        time.Duration
	RequestTimeout      time.Duration
	MaxAttempts         int
	Retention           time.Duration
	ScanBatch           int
}

func NewDefaultConfig() Config {
	return Config{CredentialDirectory: "/run/secrets/bitagent-processors", Concurrency: 2, QueueCapacity: 1000, RetainedJobs: 10000, PollInterval: time.Second, RequestTimeout: 10 * time.Second, MaxAttempts: 3, Retention: 7 * 24 * time.Hour, ScanBatch: 32}
}
func (c Config) Validate() error {
	if c.Concurrency < 1 || c.Concurrency > 16 || c.QueueCapacity < 1 || c.QueueCapacity > 100000 || c.RetainedJobs < c.QueueCapacity || c.RetainedJobs > 1000000 || c.ScanBatch < 1 || c.ScanBatch > 256 || c.MaxAttempts < 1 || c.MaxAttempts > 5 || c.RequestTimeout < 100*time.Millisecond || c.RequestTimeout > time.Minute || c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute || c.Retention < time.Hour || c.Retention > 30*24*time.Hour {
		return fmt.Errorf("invalid processor resource bounds")
	}
	return nil
}
