package processors

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/buildidentity"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
)

func NewConfiguredStore(c Config, p lazy.Lazy[*pgxpool.Pool], cc classifier.Config, cf contentfilter.Config, n *namepolicy.Policy) (*Store, error) {
	// The policy generation covers deterministic settings, not credentials.
	s, e := NewStore(c, p, digest([]any{"processor-core-policy-v1", buildidentity.CurrentBuildIdentity(), cc, cf}))
	if e != nil {
		return nil, e
	}
	s.noiseV2 = cc.ParseNoiseV2
	s.singleEpisodeMax = cc.SingleEpisodeMaxBytes
	s.names = n
	s.filter = contentfilter.New(cf)
	s.filter.SetNamePolicy(n)
	s.forbiddenTypes = map[string]bool{}
	for _, kind := range cc.DeleteContentTypes {
		s.forbiddenTypes[kind] = true
	}
	if cc.DeleteXxx {
		s.forbiddenTypes["xxx"] = true
	}
	return s, nil
}
func (s *Store) Collectors() []prometheus.Collector { return s.metrics.Collectors() }
