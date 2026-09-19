package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSeedersFreshestAuthoritativeWins locks the v0.52.0 semantics: the
// 'tracker' source row wins outright when it carries a count — including an
// authoritative 0 — and the MAX-across-sources fallback applies only when no
// tracker verdict exists.
func TestSeedersFreshestAuthoritativeWins(t *testing.T) {
	t.Parallel()

	src := func(name string, seeders, leechers int, valid bool) TorrentsTorrentSource {
		s := TorrentsTorrentSource{Source: name}
		if valid {
			s.Seeders = NewNullUint(uint(seeders))
			s.Leechers = NewNullUint(uint(leechers))
		}
		return s
	}

	cases := []struct {
		name         string
		sources      []TorrentsTorrentSource
		wantSeeders  NullUint
		wantLeechers NullUint
	}{
		{
			"fresh tracker 5 beats DHT fossil 50",
			[]TorrentsTorrentSource{src("dht", 50, 40, true), src(SourceKeyTracker, 5, 3, true)},
			NewNullUint(5), NewNullUint(3),
		},
		{
			"authoritative tracker 0 beats DHT fossil",
			[]TorrentsTorrentSource{src("dht", 50, 40, true), src(SourceKeyTracker, 0, 0, true)},
			NewNullUint(0), NewNullUint(0),
		},
		{
			"no tracker row: MAX fallback unchanged",
			[]TorrentsTorrentSource{src("dht", 7, 2, true), src("import", 12, 1, true)},
			NewNullUint(12), NewNullUint(2),
		},
		{
			"tracker row without counts falls back to MAX",
			[]TorrentsTorrentSource{src(SourceKeyTracker, 0, 0, false), src("dht", 9, 4, true)},
			NewNullUint(9), NewNullUint(4),
		},
		{
			"tracker higher than DHT still wins (not a MIN)",
			[]TorrentsTorrentSource{src("dht", 3, 1, true), src(SourceKeyTracker, 80, 20, true)},
			NewNullUint(80), NewNullUint(20),
		},
		{
			"no sources at all",
			nil,
			NullUint{}, NullUint{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tor := Torrent{Sources: tc.sources}
			assert.Equal(t, tc.wantSeeders, tor.Seeders())
			assert.Equal(t, tc.wantLeechers, tor.Leechers())
		})
	}
}
