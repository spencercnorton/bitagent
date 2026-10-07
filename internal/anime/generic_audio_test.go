package anime

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGenericAudioDoesNotAdvertiseEnglishOrCauseNewDrops(t *testing.T) {
	for _, name := range []string{"[Judas] Show - 12 [Dual-Audio]", "[Group] Show - 12 [Multi Audio]", "[Group] Show - 12 [Dual]", "[SubsPlease] Show - 12 French Dubbed Dual Audio"} {
		s := Detect(name)
		require.False(t, s.EnglishAudioSignal, name)
		require.False(t, s.EnglishSubSignal, name)
		require.False(t, ForeignAudioOnly(name), "uncertainty must not cause a new destructive decision")
	}
	for _, name := range []string{"[Group] Show - 12 [Dual Audio [ENG+JPN]]", "[Group] Show - 12 English Dub", "[Group] Show - 12 English Audio"} {
		require.True(t, Detect(name).EnglishAudioSignal, name)
	}
	require.True(t, Detect("[Group] Show - 12 English Subs").EnglishSubSignal)
}
