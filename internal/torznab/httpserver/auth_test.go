package httpserver

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/stretchr/testify/assert"
)

// helper: build a handler with a configured key + a default profile so
// the auth path is the only thing under test.
func authTestHandler(apiKey string) handler {
	gin.SetMode(gin.TestMode)
	return handler{
		config: torznab.Config{
			APIKey: apiKey,
			Profiles: []torznab.Profile{
				(torznab.Profile{ID: "default", Title: "default"}).MergeDefaults(),
			},
		},
	}
}

// authTestHandlerMultiKey builds a handler with the multi-key form
// for the new TORZNAB_API_KEYS path. apiKeys is the raw env value
// (e.g. "friend-bob:abc,spencer-prowlarr:xyz"). Pass "" for
// legacy-only configs.
func authTestHandlerMultiKey(legacyKey, apiKeys string) handler {
	gin.SetMode(gin.TestMode)
	return handler{
		config: torznab.Config{
			APIKey:  legacyKey,
			APIKeys: apiKeys,
			Profiles: []torznab.Profile{
				(torznab.Profile{ID: "default", Title: "default"}).MergeDefaults(),
			},
		},
	}
}

func doRequest(h handler, target string, headers map[string]string) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx.Request = req
	return w, ctx
}

func TestRequireAPIKey_OpenModeWhenUnset(t *testing.T) {
	h := authTestHandler("")
	w, ctx := doRequest(h, "/torznab/api?t=caps", nil)
	assert.True(t, h.requireAPIKey(ctx),
		"empty config.APIKey must allow request without credentials (backward compat)")
	assert.Equal(t, http.StatusOK, w.Code,
		"open mode must not write any status from auth path")
	assert.Empty(t, resolvedAPIKeyName(ctx),
		"open mode must NOT stash a key name (handler labels these as 'open')")
}

func TestRequireAPIKey_AcceptsMatchingQueryParam(t *testing.T) {
	h := authTestHandler("secret-key")
	w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=secret-key", nil)
	assert.True(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "default", resolvedAPIKeyName(ctx),
		"legacy single-key match labels as 'default'")
}

func TestRequireAPIKey_AcceptsMatchingHeader(t *testing.T) {
	h := authTestHandler("secret-key")
	w, ctx := doRequest(h, "/torznab/api?t=caps", map[string]string{
		"X-Api-Key": "secret-key",
	})
	assert.True(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "default", resolvedAPIKeyName(ctx))
}

func TestRequireAPIKey_RejectsMissingKey(t *testing.T) {
	h := authTestHandler("secret-key")
	w, ctx := doRequest(h, "/torznab/api?t=caps", nil)
	assert.False(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, resolvedAPIKeyName(ctx),
		"rejected path must NOT stash a key name")

	// Body is the upstream Torznab error envelope. Note the upstream
	// bitmagnet XML tag is `error="100"` not `code="100"` (a quirk in
	// upstream `internal/torznab/errors.go` flagged with
	// `revive:disable-next-line:struct-tag`). We just verify the
	// envelope round-trips and carries code 100.
	body := w.Body.String()
	assert.True(t, strings.Contains(body, `"100"`),
		"401 body must include code 100; got %q", body)
	var e torznab.Error
	assert.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e))
	assert.Equal(t, 100, e.Code)
	assert.Contains(t, e.Description, "credentials")
}

func TestRequireAPIKey_RejectsWrongKey(t *testing.T) {
	h := authTestHandler("secret-key")
	w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=WRONG", nil)
	assert.False(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireAPIKey_QueryTakesPrecedenceWhenBothPresent(t *testing.T) {
	// If both query and header are present, the query (Torznab spec
	// path) wins. This matches Prowlarr's behaviour and keeps the
	// auth path simple-to-explain.
	h := authTestHandler("secret-key")
	w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=secret-key", map[string]string{
		"X-Api-Key": "BOGUS",
	})
	assert.True(t, h.requireAPIKey(ctx),
		"matching query apikey must be honoured even with a wrong header")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireAPIKey_FallsBackToHeaderWhenQueryEmpty(t *testing.T) {
	// Query absent, header present → use header.
	h := authTestHandler("secret-key")
	w, ctx := doRequest(h, "/torznab/api?t=caps", map[string]string{
		"X-Api-Key": "secret-key",
	})
	assert.True(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireAPIKey_ConstantTimeCompare_LengthDoesntLeak(t *testing.T) {
	// Just an existence/safety check: feeding a wrong-length key never
	// crashes and always returns false. We can't measure timing in a
	// unit test, but constant-time intent is preserved.
	h := authTestHandler("a-very-long-secret-key-of-many-bytes")
	for _, bad := range []string{"", "x", "short", strings.Repeat("a", 256)} {
		bad := bad
		t.Run("bad="+bad[:min(len(bad), 8)], func(t *testing.T) {
			w, ctx := doRequest(h, "/torznab/api?t=caps&apikey="+bad, nil)
			assert.False(t, h.requireAPIKey(ctx))
			assert.Equal(t, http.StatusUnauthorized, w.Code)
		})
	}
}

// --- Multi-key (TORZNAB_API_KEYS) tests ---------------------------------

func TestRequireAPIKey_MultiKey_AcceptsNamedKeyByQuery(t *testing.T) {
	h := authTestHandlerMultiKey("", "friend-bob:friend-secret,spencer-prowlarr:prowlarr-secret")
	w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=friend-secret", nil)
	assert.True(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "friend-bob", resolvedAPIKeyName(ctx),
		"matched key's name must be available for downstream metrics")
}

func TestRequireAPIKey_MultiKey_AcceptsAlternateNamedKey(t *testing.T) {
	h := authTestHandlerMultiKey("", "friend-bob:friend-secret,spencer-prowlarr:prowlarr-secret")
	w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=prowlarr-secret", nil)
	assert.True(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "spencer-prowlarr", resolvedAPIKeyName(ctx))
}

func TestRequireAPIKey_MultiKey_RejectsUnknownKey(t *testing.T) {
	h := authTestHandlerMultiKey("", "friend-bob:friend-secret")
	w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=who-am-i", nil)
	assert.False(t, h.requireAPIKey(ctx))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireAPIKey_LegacyAndMulti_BothAccepted(t *testing.T) {
	// Backward compatibility: when both APIKey and APIKeys are set,
	// either the legacy single key OR any named key is accepted.
	h := authTestHandlerMultiKey("legacy-key", "friend-bob:friend-secret")
	for _, c := range []struct{ key, expectedName string }{
		{"legacy-key", "default"},
		{"friend-secret", "friend-bob"},
	} {
		c := c
		t.Run("key="+c.key, func(t *testing.T) {
			w, ctx := doRequest(h, "/torznab/api?t=caps&apikey="+c.key, nil)
			assert.True(t, h.requireAPIKey(ctx))
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, c.expectedName, resolvedAPIKeyName(ctx))
		})
	}
}

func TestRequireAPIKey_MultiKey_MalformedEntriesIgnored(t *testing.T) {
	// Whitespace / missing colon / empty values are silently dropped
	// so a typo in one slot doesn't take down the indexer for everyone.
	// (See APIKeys doc.)
	h := authTestHandlerMultiKey("",
		"  friend-bob:friend-secret  , malformed-no-colon , :no-name , empty-value: , ok-too:second-secret",
	)

	// The two well-formed entries still match.
	for _, c := range []struct{ key, expectedName string }{
		{"friend-secret", "friend-bob"},
		{"second-secret", "ok-too"},
	} {
		c := c
		t.Run("good="+c.key, func(t *testing.T) {
			w, ctx := doRequest(h, "/torznab/api?t=caps&apikey="+c.key, nil)
			assert.True(t, h.requireAPIKey(ctx))
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, c.expectedName, resolvedAPIKeyName(ctx))
		})
	}

	// Malformed entries don't accidentally match anything.
	t.Run("malformed-not-matchable", func(t *testing.T) {
		w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=malformed-no-colon", nil)
		assert.False(t, h.requireAPIKey(ctx))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestRequireAPIKey_MultiKey_InvalidNameCharsetSkipped(t *testing.T) {
	// Names with disallowed characters (uppercase, dots, spaces) are
	// dropped at parse time so they can't poison Prometheus label
	// cardinality. The valid sibling still matches.
	h := authTestHandlerMultiKey("", "Bad.Name:bad-key,good-name:good-key")
	t.Run("bad-name-key-dropped", func(t *testing.T) {
		w, ctx := doRequest(h, "/torznab/api?t=caps&apikey=bad-key", nil)
		assert.False(t, h.requireAPIKey(ctx))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
	t.Run("good-name-key-still-matches", func(t *testing.T) {
		_, ctx := doRequest(h, "/torznab/api?t=caps&apikey=good-key", nil)
		assert.True(t, h.requireAPIKey(ctx))
		assert.Equal(t, "good-name", resolvedAPIKeyName(ctx))
	})
}

func TestRequireAPIKey_MultiKey_OpenModeWhenAllMalformed(t *testing.T) {
	// Edge case: only malformed entries → AnyAPIKeyConfigured() still
	// returns true (because the *raw* config has non-empty pieces),
	// but LookupAPIKey returns no match for anything. So a request
	// missing a key gets rejected (NOT open mode), preserving the
	// safe-fail behaviour. The operator's typo doesn't accidentally
	// open the indexer.
	h := authTestHandlerMultiKey("", "malformed,also-malformed")
	w, ctx := doRequest(h, "/torznab/api?t=caps", nil)
	assert.False(t, h.requireAPIKey(ctx),
		"any non-empty TORZNAB_API_KEYS must require auth even if all entries are malformed")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
