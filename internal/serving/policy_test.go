package serving

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyOffRetainsQuarantineGuardWithoutAdultCriteria(t *testing.T) {
	p, err := NewPolicy(Config{}, []string{"siterip"}, []string{"mkv"})
	require.NoError(t, err)
	for _, table := range []string{"torrents", "torrent_contents", "torrent_files", "torrent_tags"} {
		expr := p.TorrentCondition(table)
		require.Contains(t, expr.SQL, "junkpurge_quarantine")
		require.Contains(t, expr.SQL, "jsonb_typeof(serving_q.sources_snapshot) = 'array'")
		require.Contains(t, expr.SQL, "serving_q.torrent_snapshot->>'info_hash'")
		require.Contains(t, expr.SQL, "serving_q.torrent_snapshot->>'name'")
		require.NotContains(t, expr.SQL, "expired_at")
		require.NotContains(t, expr.SQL, "serving_tc")
		require.Empty(t, expr.Vars)
	}
	require.Equal(t, "TRUE", p.ContentCondition().SQL)
}

func TestAdultPolicyUsesPositiveNullableMetadataAndExactUnicodeBoundaries(t *testing.T) {
	p, err := NewPolicy(Config{ExcludeAdult: true}, []string{"siterip"}, []string{"mkv"})
	require.NoError(t, err)
	expr := p.TorrentCondition("torrent_contents")
	require.Contains(t, expr.SQL, "serving_c.adult IS TRUE")
	require.NotContains(t, expr.SQL, "adult <> true")
	require.NotContains(t, p.strongPattern, `\p{L}`)
	require.NotContains(t, p.strongPattern, `\d`)
	require.NotContains(t, p.strongPattern, "[:alpha:]")
	require.True(t, strings.Contains(p.strongPattern, "A-Z"))
	require.Equal(t, "content.type <> 'xxx' AND content.adult IS NOT TRUE", p.ContentCondition().SQL)
	require.Panics(t, func() { p.TorrentCondition("untrusted; DROP TABLE torrents") })
}
