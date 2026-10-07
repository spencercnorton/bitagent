package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

const publicHashBodyLimit = 4096

// NewPublicHash checks actual stored public releases through the mandatory
// consumer view. It never grants private membership or trusts a supplied name.
func NewPublicHash(s lazy.Lazy[search.ServingSearch], policy *namepolicy.Policy) httpserver.Option {
	return publicHashBuilder{search: s, policy: policy}
}

type publicHashBuilder struct {
	search lazy.Lazy[search.ServingSearch]
	policy *namepolicy.Policy
}

func (publicHashBuilder) Key() string { return "name-policy-public-hash" }

func (b publicHashBuilder) Apply(e *gin.Engine) error {
	e.POST("/internal/name-policy/check-public", b.check)
	return nil
}

func (b publicHashBuilder) check(c *gin.Context) {
	if !Authorize(c, b.policy) {
		return
	}
	var request struct {
		InfoHashes []string `json:"infoHashes"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, publicHashBodyLimit)
	if !Decode(c, &request) {
		return
	}
	if len(request.InfoHashes) == 0 || len(request.InfoHashes) > MaxBatch {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	hashes := make([]protocol.ID, len(request.InfoHashes))
	unique := make([]protocol.ID, 0, len(hashes))
	seen := make(map[protocol.ID]struct{}, len(hashes))
	for i, value := range request.InfoHashes {
		hash, err := protocol.ParseID(value)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		hashes[i] = hash
		if _, ok := seen[hash]; !ok {
			unique = append(unique, hash)
			seen[hash] = struct{}{}
		}
	}
	s, err := b.search.Get()
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	result, err := s.TorrentsWithMissingInfoHashes(ctx, unique,
		query.WithTotalCount(false), query.Limit(32),
		query.Where(query.DBCriteria{SQL: "torrents.private = FALSE"}, query.DBCriteria{
			SQL:  `NOT EXISTS(SELECT 1 FROM label_evidence public_privacy WHERE public_privacy.info_hash=torrents.info_hash AND ` + catalogueguard.QBPrivacySQL("public_privacy.source", "public_privacy.category", "?") + `)`,
			Args: []interface{}{catalogueguard.TagWhitespace, catalogueguard.TagWhitespace},
		}))
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	allowed := make(map[protocol.ID]namepolicy.Decision, len(result.Torrents))
	for _, torrent := range result.Torrents {
		if _, requested := seen[torrent.InfoHash]; !requested || torrent.Private {
			continue
		}
		allowed[torrent.InfoHash] = b.policy.Evaluate(torrent.InfoHash, torrent.Name)
	}
	results := make([]Result, len(hashes))
	for i, hash := range hashes {
		results[i] = Result{Index: i, Reason: "not_served", Version: namepolicy.Version}
		if decision, ok := allowed[hash]; ok {
			results[i].Eligible, results[i].Reason = decision.Eligible, decision.Reason
		}
		b.policy.Observe("public_hash_check", namepolicy.Decision{Eligible: results[i].Eligible, Reason: results[i].Reason, Version: namepolicy.Version})
	}
	// Result positions alone bind responses to the caller's request. Names and
	// hashes, including missing or suppressed records, are never echoed.
	c.JSON(http.StatusOK, Response{Enabled: true, Version: namepolicy.Version, Results: results})
}
