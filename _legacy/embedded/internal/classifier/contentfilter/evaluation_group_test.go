package contentfilter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvaluationGroupKeyCollapsesReleaseVariants(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  string
	}{
		{
			left:  "[SubsPlease] Some Show - S01E01 (1080p) [ABC123]",
			right: "[Erai-raws] Some.Show.S01E09.720p.WEBRip-GROUP",
			want:  "some show",
		},
		{
			left:  "Some.Movie.2024.2160p.BluRay.x265-GROUP",
			right: "Some Movie (2024) 1080p WEB-DL-OTHER",
			want:  "some movie",
		},
		{
			left:  "www.UIndex.org - Foreign.Title.2022.1080p",
			right: "Foreign Title 2022 720p",
			want:  "foreign title",
		},
	}
	for _, test := range tests {
		require.Equal(t, test.want, EvaluationGroupKey(test.left))
		require.Equal(t, test.want, EvaluationGroupKey(test.right))
	}
}

func TestEvaluationGroupKeyFailsConservatively(t *testing.T) {
	require.Equal(
		t,
		"unresolved-release-family",
		EvaluationGroupKey("[1080p] 2024"),
	)
}
