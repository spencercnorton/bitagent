package contentfilter

import (
	"strings"
	"testing"
	"time"
)

// Test plan:
//   - normalizeTitle strips release tags (year, codec, source) and
//     punctuation while preserving content words.
//   - isReleaseTag distinguishes scene-tag tokens from content words.
//   - llmCacheKey changes when ANY of (model, promptVersion,
//     normalizedTitle) change — that's the cache-bust contract.
//   - llmCache get/put round-trip + Len() reporting.

func TestNormalizeTitle_ReleaseTagStripping(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Trailing scene tags drop off; content words preserved.
		{"Some.Movie.2024.1080p.x264-RARBG", "some movie"},
		{"Some Movie 2024 [1080p] [WEB-DL]", "some movie"},
		{"Movie.Title.2024.UHD.HDR.Atmos", "movie title"},
		// Year strips (1900-2099 four-digit token).
		{"My Show 2023", "my show"},
		// Lowercase + separator normalisation.
		{"FILM_NAME-1999-720p", "film name"},
		// Three-digit year doesn't strip (false positive guard).
		{"Test 999 Movie 2024", "test 999 movie"},
		// All-caps codec strips.
		{"Title 2024 X265 HEVC", "title"},
	}
	for _, tt := range tests {
		got := normalizeTitle(tt.in)
		if got != tt.want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeTitle_NoTrailingTagsLeaveAlone(t *testing.T) {
	// Content-only title (no scene tags) survives normalisation
	// unchanged except for separator + casing.
	got := normalizeTitle("My.Cool.Movie")
	if got != "my cool movie" {
		t.Errorf("got %q want my cool movie", got)
	}
}

func TestIsReleaseTag(t *testing.T) {
	// Tokens listed here are what survives tokenisation — anything
	// hyphenated like "web-dl" gets split before isReleaseTag sees
	// it, so we test the post-split fragments instead.
	tags := []string{"1080p", "x264", "x265", "h265", "web", "dl", "bluray", "amzn", "rarbg", "1999", "2024", "atmos"}
	for _, tag := range tags {
		if !isReleaseTag(tag) {
			t.Errorf("isReleaseTag(%q) = false, want true", tag)
		}
	}
	notTags := []string{"movie", "title", "the", "a", "1234567", "999", "moviefoo"}
	for _, w := range notTags {
		if isReleaseTag(w) {
			t.Errorf("isReleaseTag(%q) = true, want false", w)
		}
	}
}

func TestLLMCacheKey_StableAndUnique(t *testing.T) {
	k1 := llmCacheKey("gpt-5.4-nano", "v1", "some movie")
	k2 := llmCacheKey("gpt-5.4-nano", "v1", "some movie")
	if k1 != k2 {
		t.Errorf("same inputs should produce same key: %q vs %q", k1, k2)
	}
	if len(k1) != 64 {
		t.Errorf("expected sha256 hex (64 chars), got %d", len(k1))
	}

	// Different model busts the cache.
	if llmCacheKey("gpt-5.5-nano", "v1", "x") == llmCacheKey("gpt-5.4-nano", "v1", "x") {
		t.Errorf("model change should bust cache key")
	}
	// Different prompt version busts the cache.
	if llmCacheKey("m", "v1", "x") == llmCacheKey("m", "v2", "x") {
		t.Errorf("promptVersion change should bust cache key")
	}
	// Different title busts the cache.
	if llmCacheKey("m", "v1", "alpha") == llmCacheKey("m", "v1", "beta") {
		t.Errorf("title change should bust cache key")
	}
}

func TestLLMCacheKey_HexEncoding(t *testing.T) {
	k := llmCacheKey("a", "b", "c")
	for _, r := range k {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			t.Errorf("non-hex char %q in key %q", r, k)
			break
		}
	}
}

func TestLLMCache_RoundTrip(t *testing.T) {
	c := newLLMCache(100, time.Hour)

	if _, ok := c.Get("missing"); ok {
		t.Errorf("empty cache should miss")
	}

	v := LLMVerdict{IsEnglish: false, Confidence: 0.91, Reason: "cyrillic-words"}
	c.Put("k1", v)

	got, ok := c.Get("k1")
	if !ok {
		t.Fatalf("Put then Get should hit")
	}
	if got != v {
		t.Errorf("got %+v want %+v", got, v)
	}

	if c.Len() != 1 {
		t.Errorf("Len after one Put: got %d want 1", c.Len())
	}
}

func TestLLMCache_DefaultsOnZeroInputs(t *testing.T) {
	// Constructor protects callers from misconfigured zero values
	// — passing 0 should yield documented defaults rather than a
	// panic-prone "no entries allowed" cache.
	c := newLLMCache(0, 0)
	c.Put("k", LLMVerdict{IsEnglish: true, Reason: "english-clear"})
	if _, ok := c.Get("k"); !ok {
		t.Errorf("default cache should accept Put/Get")
	}
}

func TestNormalizeTitle_TitleNotJustReleaseTags(t *testing.T) {
	// A title that's ONLY release tags (degenerate) returns "".
	// We don't want this to crash the cache path.
	got := normalizeTitle("1080p.x264.WEB-DL.2024")
	// All tokens are release tags so they all strip; the result is
	// empty. The caller should treat empty-normalized as "skip LLM."
	if got != "" {
		t.Errorf("all-release-tag title should normalize to empty; got %q", got)
	}
	// Sanity: ensure it's not panicking on weird inputs.
	if strings.Contains(got, "1080p") {
		t.Errorf("release tag survived normalisation: %q", got)
	}
}
