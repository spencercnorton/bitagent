package llmmatch

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// EmbeddingConfig is an independent, explicit OpenAI-compatible embeddings
// route. Similarity only makes a shortlist; it never supplies match confidence.
// Enabled defaults false and no endpoint or model is selected automatically.
type EmbeddingConfig struct {
	Enabled            bool          `yaml:"enabled"`
	Endpoint           string        `yaml:"endpoint"`
	Model              string        `yaml:"model"`
	APIKey             string        `yaml:"api_key"`
	OpenrouterProvider string        `yaml:"openrouter_provider"`
	Timeout            time.Duration `yaml:"timeout"`
	MaxRequestBytes    int           `yaml:"max_request_bytes"`
	// Dimensions requests a bounded vector size. Zero omits this optional
	// provider parameter; MaxDimensions still bounds accepted responses.
	Dimensions    int `yaml:"dimensions"`
	MaxDimensions int `yaml:"max_dimensions"`
	ShortlistSize int `yaml:"shortlist_size"`
}

func NewDefaultEmbeddingConfig() EmbeddingConfig {
	return EmbeddingConfig{
		Timeout: 15 * time.Second, MaxRequestBytes: 8 << 10,
		Dimensions: 256, MaxDimensions: 4096, ShortlistSize: 3,
	}
}

func (c EmbeddingConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || u.Opaque != "" || !strings.HasSuffix(u.Path, "/embeddings") {
		return fmt.Errorf("embeddings requires a plain, full embeddings endpoint")
	}
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return fmt.Errorf("embeddings requires HTTPS except on loopback")
	}
	if strings.TrimSpace(c.Model) == "" || c.Model != strings.TrimSpace(c.Model) {
		return fmt.Errorf("embeddings requires an explicit model")
	}
	if !loopback && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("hosted embeddings requires its own API key")
	}
	if host == "openrouter.ai" || c.OpenrouterProvider != "" {
		if c.Endpoint != "https://openrouter.ai/api/v1/embeddings" ||
			strings.TrimSpace(c.OpenrouterProvider) == "" || c.OpenrouterProvider != strings.TrimSpace(c.OpenrouterProvider) {
			return fmt.Errorf("OpenRouter embeddings requires its HTTPS endpoint and an explicit provider pin")
		}
	}
	if host == "api.openai.com" && c.Dimensions > 0 && !strings.HasPrefix(c.Model, "text-embedding-3-") {
		return fmt.Errorf("OpenAI dimensions requires a text-embedding-3 model; use zero to omit")
	}
	if c.Timeout <= 0 || c.MaxRequestBytes < 1 || c.MaxRequestBytes > 1<<20 ||
		c.MaxDimensions < 1 || c.MaxDimensions > 4096 || c.Dimensions < 0 ||
		c.Dimensions > c.MaxDimensions || c.ShortlistSize < 2 || c.ShortlistSize > 100 {
		return fmt.Errorf("embeddings timeout, input, dimensions and shortlist bounds are invalid")
	}
	return nil
}
