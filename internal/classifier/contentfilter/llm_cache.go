package contentfilter

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// llmCacheKey hashes (model, promptVersion, normalizedTitle) so a
// model upgrade or prompt rewrite invalidates everything cleanly.
// Deliberately keyed on the NORMALIZED title (lowercase, collapsed
// whitespace, year/quality tags stripped) so trivially-different
// release names (e.g. "Some.Movie.2024.1080p" vs "Some Movie 2024
// [1080p]") share a cache slot.
func llmCacheKey(model, promptVersion, normalizedTitle string) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0x1f})
	h.Write([]byte(promptVersion))
	h.Write([]byte{0x1f})
	h.Write([]byte(normalizedTitle))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeTitle collapses release-tag noise so titles that differ
// only in framerate / source / encoder share a cache entry. The
// goal isn't perfect canonicalisation — it's "torrents that are
// the same content but different releases hit the same cache slot."
//
// Steps:
//
//	lowercase
//	replace .  _  -  [  ]  (  )  with spaces
//	collapse runs of whitespace
//	drop ANY token that matches the conservative release-tag set
//	  (codec / quality / source / scene-group / year)
//
// Tokens are filtered everywhere, not just at the trailing edge,
// because real titles have tags interspersed: "Some Movie 2024
// [WEB-DL]" tokenises to {some, movie, 2024, web, dl} after the
// bracket+dash normalisation, and a trailing-only sweep would stop
// at the first non-tag and leave "dl" stuck in the cache key. The
// list is intentionally conservative — false positives risk losing
// a content word, which would over-collapse the cache.
func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	// Replace common separators with spaces.
	r := strings.NewReplacer(".", " ", "_", " ", "-", " ", "[", " ", "]", " ", "(", " ", ")", " ")
	s = r.Replace(s)
	// Tokenise + drop any release-tag token regardless of position.
	parts := strings.Fields(s)
	out := parts[:0]
	for _, tok := range parts {
		if isReleaseTag(tok) {
			continue
		}
		out = append(out, tok)
	}
	return strings.Join(out, " ")
}

// isReleaseTag returns true for tokens that are probably scene-tag
// noise — quality, source, codec, year, etc. List is conservative;
// false negatives just mean a slightly less-effective cache, which
// is fine. False positives risk dropping a real word from the
// title, which is bad — so the rules are strict.
func isReleaseTag(tok string) bool {
	switch tok {
	case "1080p", "2160p", "720p", "480p", "4k", "uhd", "hdr", "dv", "imax",
		"x264", "x265", "h264", "h265", "hevc", "av1", "avc",
		"web", "webrip", "webdl", "bluray", "brrip", "bdrip", "dvdrip",
		"hdrip", "hdtv", "dvd", "remux", "amzn", "atmos", "dts", "ac3", "aac",
		// Post-split fragments. Tokenisation by `-` turns "web-dl" /
		// "blu-ray" / "h.264" into separate tokens; we keep the head
		// (web/bluray/h264) above and add the tails here so the
		// pair fully collapses.
		"dl", "ray",
		"complete", "internal", "repack", "proper",
		"yify", "yts", "rarbg", "ettv", "eztv":
		return true
	}
	// 4-digit year (1900-2099) — common trailing tag.
	if len(tok) == 4 {
		ok := true
		for _, r := range tok {
			if r < '0' || r > '9' {
				ok = false
				break
			}
		}
		if ok && (tok[0] == '1' || tok[0] == '2') {
			return true
		}
	}
	return false
}

// llmCache caches LLM verdicts by hashed key with TTL+LRU eviction.
// Thread-safe: the underlying expirable.LRU is internally synced.
//
// Persistence: in-memory only. A future enhancement could write a
// snapshot to /data on graceful shutdown so a container restart
// doesn't re-pay the LLM cost on common titles, but that's a
// scope-creep concern and not in this MR.
type llmCache struct {
	lru *lru.LRU[string, llmCacheEntry]
	mu  sync.RWMutex
}

type llmCacheEntry struct {
	Verdict   LLMVerdict
	CachedAt  time.Time
}

func newLLMCache(maxEntries int, ttl time.Duration) *llmCache {
	if maxEntries <= 0 {
		maxEntries = 50_000
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &llmCache{
		lru: lru.NewLRU[string, llmCacheEntry](maxEntries, nil, ttl),
	}
}

func (c *llmCache) Get(key string) (LLMVerdict, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.lru.Get(key)
	if !ok {
		return LLMVerdict{}, false
	}
	return v.Verdict, true
}

func (c *llmCache) Put(key string, v LLMVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Add(key, llmCacheEntry{Verdict: v, CachedAt: time.Now()})
}

func (c *llmCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lru.Len()
}
