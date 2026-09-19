// Torznab API-key authentication.
//
// When TORZNAB_API_KEY (single legacy key) OR TORZNAB_API_KEYS (named
// per-consumer keys) is set, every request must carry the matching
// key in the standard `apikey` query parameter (Torznab spec) or the
// `X-Api-Key` header. Unset = open mode (backward compatible with
// operators who run bitagent on a private network).
//
// On match, the resolved key name (e.g. "default", "friend-bob",
// "spencer-prowlarr") is stored on the gin context so the handler can
// surface it in metrics for per-consumer audit and rate-limit work.
//
// The Torznab spec defines a small error-code vocabulary; we use 100
// "Incorrect user credentials" for any auth failure (whether missing or
// wrong key). The response is XML per spec — Prowlarr / Sonarr parse it
// and surface a "wrong API key" message in their UI.
package httpserver

import (
	"encoding/xml"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

// Standard query parameter name per the Torznab spec.
const queryParamAPIKey = "apikey"

// Common header name used by *arr-ecosystem clients. Both Prowlarr and
// Sonarr send the key as a query parameter; the header is convenience
// for hand-curl testing and for proxies that strip query strings from
// access logs.
const headerAPIKey = "X-Api-Key"

// gin context key under which a successful auth stashes the matched
// key's name. Read by handler.handleRequest to attach the key_name
// label to per-request metrics. Empty = open-mode (no key configured).
const ctxKeyAPIKeyName = "torznab_api_key_name"

// requireAPIKey gates every torznab request against the configured key
// set. Skipped entirely when no key is configured (operator opted in
// to open mode). Returns true when the request should proceed; on auth
// failure, writes a 401 + Torznab error XML and returns false.
//
// On success, ctx[ctxKeyAPIKeyName] is set to the matched key's name
// ("default" for legacy single-key matches; the configured name for
// per-consumer matches).
func (h handler) requireAPIKey(ctx *gin.Context) bool {
	if !h.config.AnyAPIKeyConfigured() {
		// Open mode — preserves the pre-2026-04-26 behaviour for
		// operators running bitagent on a trusted private network.
		// Public-deploy README REQUIRES setting TORZNAB_API_KEY
		// or TORZNAB_API_KEYS.
		return true
	}

	got := ctx.Query(queryParamAPIKey)
	if got == "" {
		got = ctx.GetHeader(headerAPIKey)
	}

	if name, ok := h.config.LookupAPIKey(got); ok {
		ctx.Set(ctxKeyAPIKeyName, name)
		return true
	}

	// Spec-compliant error envelope. Prowlarr surfaces the description
	// in its UI ("Indexer test failed: Incorrect user credentials").
	// We write the 401 response directly rather than via writeXML
	// because writeXML force-sets status 200 (it's the success path).
	body, _ := xml.Marshal(torznab.Error{
		Code:        100,
		Description: "Incorrect user credentials",
	})
	ctx.Header("Content-Type", "application/xml; charset=utf-8")
	ctx.Status(http.StatusUnauthorized)
	_, _ = ctx.Writer.Write(body)
	return false
}

// resolvedAPIKeyName returns the name stashed by requireAPIKey, or
// empty if open-mode / not yet matched. Safe to call regardless of
// auth path taken.
func resolvedAPIKeyName(ctx *gin.Context) string {
	if v, ok := ctx.Get(ctxKeyAPIKeyName); ok {
		if name, ok2 := v.(string); ok2 {
			return name
		}
	}
	return ""
}
