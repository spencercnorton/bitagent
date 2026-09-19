package animedb

import (
	"testing"

	"github.com/iancoleman/strcase"
)

// TestConfigEnvKeysResolveToDocumentedNames pins the strcase.ToSnake tokenizer
// output for every Config field, so an env-key regression (like the FeedURLs /
// TrackerURLs footgun) can't silently disable a documented ANIME_TITLES_* var.
// The config resolver derives env keys from the Go field name, NOT the yaml tag.
func TestConfigEnvKeysResolveToDocumentedNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		field     string
		wantSnake string
	}{
		{"Enabled", "enabled"},
		{"EnableWrite", "enable_write"},
		{"Interval", "interval"},
		{"MinRefreshAge", "min_refresh_age"},
		{"AnimeListUrl", "anime_list_url"},
		{"AnidbTitlesUrl", "anidb_titles_url"},
		{"CacheDir", "cache_dir"},
		{"UserAgent", "user_agent"},
		{"HttpTimeout", "http_timeout"},
		{"Languages", "languages"},
	}
	for _, c := range cases {
		if got := strcase.ToSnake(c.field); got != c.wantSnake {
			t.Errorf("strcase.ToSnake(%q) = %q, want %q", c.field, got, c.wantSnake)
		}
	}
}

func TestDefaultConfigIsOptIn(t *testing.T) {
	t.Parallel()
	cfg := NewDefaultConfig()
	if cfg.Enabled || cfg.EnableWrite {
		t.Errorf("defaults must be opt-in (Enabled=false, EnableWrite=false), got %+v", cfg)
	}
	if cfg.AnimeListUrl == "" || cfg.AnidbTitlesUrl == "" {
		t.Errorf("default source URLs must be set, got %+v", cfg)
	}
	if len(cfg.Languages) == 0 {
		t.Errorf("default language whitelist must be non-empty")
	}
}
