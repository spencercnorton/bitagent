package config

import (
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
)

func TestEnvBindingMatcherEmbeddingRouteIsIndependent(t *testing.T) {
	env := map[string]string{
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_ENABLED":             "true",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_ENDPOINT":            "https://api.example.test/v1/embeddings",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_MODEL":               "synthetic-embedding",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_API_KEY":             "synthetic-embedding-key",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_OPENROUTER_PROVIDER": "pinned/provider",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_TIMEOUT":             "12s",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_DIMENSIONS":          "128",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_MAX_DIMENSIONS":      "512",
		"CLASSIFIER_LLM_MATCH_EMBEDDINGS_SHORTLIST_SIZE":      "4",
	}
	cfg := resolveSectionFromEnv(t, "classifier_llm_match", llmmatch.NewDefaultConfig(), env).(llmmatch.Config)
	if !cfg.Embeddings.Enabled || cfg.Embeddings.Endpoint != env["CLASSIFIER_LLM_MATCH_EMBEDDINGS_ENDPOINT"] || cfg.Embeddings.Model != "synthetic-embedding" ||
		cfg.Embeddings.APIKey != "synthetic-embedding-key" || cfg.Embeddings.OpenrouterProvider != "pinned/provider" || cfg.Embeddings.Timeout != 12*time.Second ||
		cfg.Embeddings.Dimensions != 128 || cfg.Embeddings.MaxDimensions != 512 || cfg.Embeddings.ShortlistSize != 4 {
		t.Fatalf("documented embedding env keys did not bind")
	}
	defaults := llmmatch.NewDefaultConfig()
	if cfg.Enabled || cfg.EnableLive || cfg.Model != defaults.Model || cfg.Endpoint != defaults.Endpoint || cfg.APIKey != "" || cfg.OpenrouterProvider != "" {
		t.Fatal("embedding route leaked into chat or enabled matching")
	}
}
