package llmmatch

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractInputMatchesProductionShape(t *testing.T) {
	require.Equal(
		t,
		"release_name: Example.2024.1080p\nfiles:\n  - Example.mkv\n  - Example.eng.srt\n",
		ExtractInput("Example.2024.1080p", []string{"Example.mkv", "Example.eng.srt"}),
	)
	require.Equal(
		t,
		"release_name: Example.2024.1080p",
		ExtractInput("Example.2024.1080p", []string{"1", "2", "3", "4", "5", "6"}),
	)
}

func TestRerankInputTruncatesOverview(t *testing.T) {
	input := RerankInput(
		"Example.2024",
		Extraction{Title: "Example", Year: 2024, Type: "movie"},
		[]Candidate{{ID: 42, Title: "Example", Year: 2024, Overview: strings.Repeat("x", 300)}},
	)
	require.Contains(t, input, `parsed: title="Example" year=2024 type=movie`)
	require.Contains(t, input, "1) tmdb_id=42 | Example (2024) | "+strings.Repeat("x", 240)+"\n")
	require.NotContains(t, input, strings.Repeat("x", 241))
}
