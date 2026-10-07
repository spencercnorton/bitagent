package namepolicy

import (
	"encoding/json"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/protocol"
)

// SecretToken retains the configured credential while redacting configuration
// display, JSON and structured logging. It is never part of policy identity.
type SecretToken string

func (t SecretToken) String() string {
	if t == "" {
		return ""
	}
	return "[redacted]"
}
func (t SecretToken) GoString() string             { return t.String() }
func (t SecretToken) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

type Config struct {
	Enabled            bool        `yaml:"enabled"`
	ExcludedInfoHashes []string    `yaml:"excluded_info_hashes"`
	InternalCheckToken SecretToken `yaml:"internal_check_token"`
}

func NewDefaultConfig() Config { return Config{} }

func (c Config) Validate() error {
	if len(c.ExcludedInfoHashes) > 4096 {
		return fmt.Errorf("name policy: excluded hash list exceeds bound")
	}
	for _, h := range c.ExcludedInfoHashes {
		if _, err := protocol.ParseID(h); err != nil {
			return fmt.Errorf("name policy: invalid excluded info hash")
		}
	}
	if c.InternalCheckToken != "" && (len(c.InternalCheckToken) < 32 || len(c.InternalCheckToken) > 256) {
		return fmt.Errorf("name policy: internal credential must contain 32..256 bytes")
	}
	return nil
}
