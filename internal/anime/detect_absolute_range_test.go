package anime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAbsoluteRangeAndIndependentGate locks the v0.51.0 review fixes: batch
// ranges set AbsoluteRange (IsAnime unchanged), and the independent-gate
// helper rejects the circular unlisted-group classes.
func TestAbsoluteRangeAndIndependentGate(t *testing.T) {
	t.Parallel()

	s := Detect("[Erai-raws] Show - 01 ~ 12 [1080p]")
	assert.Equal(t, 1, s.AbsoluteEpisode)
	assert.True(t, s.AbsoluteRange, "tilde batch range")
	assert.True(t, s.IsAnime(), "batch is still anime")

	s = Detect("[SubsPlease] Show - 01 - 24 [Batch]")
	assert.True(t, s.AbsoluteRange, "dash batch range")

	s = Detect("[SubsPlease] Show - 05.5 (1080p)")
	assert.True(t, s.AbsoluteRange, "fractional special")

	s = Detect("[SubsPlease] One Piece - 1071 (1080p)")
	assert.Equal(t, 1071, s.AbsoluteEpisode)
	assert.False(t, s.AbsoluteRange)
	assert.True(t, s.IsAnimeIndependentOfAbsolute(), "known group")

	assert.False(t, Detect("[Brazzers] Scene Name - 12").IsAnimeIndependentOfAbsolute())
	assert.False(t, Detect("[Multi] Thing - 12").IsAnimeIndependentOfAbsolute())
	assert.True(t, Detect("[NewSubGroup] Show - 12 [Eng Sub]").IsAnimeIndependentOfAbsolute())

	assert.Equal(t, 0, Detect("[Group] Show - 4320 [8K]").AbsoluteEpisode, "8K height rejected")
}
