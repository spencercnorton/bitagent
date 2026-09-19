package episodesbackfillcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCandidateRegexNewClasses locks the v0.50.0 candidate extensions:
// year-season rows and dash-resolution rows (INCLUDING the trailing-p form —
// \b alone fails against the word-char 'p').
func TestCandidateRegexNewClasses(t *testing.T) {
	t.Parallel()

	match := []string{
		"Shark.Week.S2022E16.1080p.WEB",
		"Show.S01E80 - 1080p.WEB",
		"Show.S01E01 - 720p.WEB",
		"Show.S01E80 - 1080.WEB",
	}
	noMatch := []string{
		"Show.S01E05.1080p.WEB",
		"Plain.Movie.2023.1080p",
	}
	for _, n := range match {
		assert.True(t, candidateNameRegex.MatchString(n), n)
	}
	for _, n := range noMatch {
		assert.False(t, candidateNameRegex.MatchString(n), n)
	}
}
