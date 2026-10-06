package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseGroupClaimsAreBoundedAndCannotStealBareTitleSuffixes(t *testing.T) {
	for _, input := range []string{"The-Thing.mkv", "Spider-Man.2002.mkv", "x265-" + strings.Repeat("G", 65), "x265-PROPER", "WEB-DL", "DTS-HD", "x265-HEVC", "x265-GROUP-two", "x265-1080p"} {
		require.False(t, InferReleaseGroup(input).Valid, input)
	}
	require.Equal(t, "Ab12_Group", InferReleaseGroup("x265.DTS-Ab12_Group.mkv").String)
}
