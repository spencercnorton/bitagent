package episodesbackfillcmd

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

func row(id, name string, stored model.Episodes) *model.TorrentContent {
	return &model.TorrentContent{
		ID:       id,
		Episodes: stored,
		Torrent:  model.Torrent{Name: name},
	}
}

// candidateNameRegex must select exactly the mis-stored shapes and skip
// innocent names, so the backfill never re-parses (and risks re-writing) a row
// the fix cannot change.
func TestCandidateNameRegex(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"www.UIndex.org - Pokemon S20E048 Sparkle", true},
		{"The.Bold.and.the.Beautiful.S39E206.720p", true},
		{"Show S01E01E02 1080p WEB", true},
		{"One Piece 1x1000", true},
		{"Some Show Episode 128 720p", true},
		{"The Wire S01E05 1080p BluRay x264-GROUP", false}, // 2-digit ep, fine
		{"The Batman 2022 1080p x265", false},              // x265 is a codec, not an episode
		{"Movie 1920x1080 HDR", false},                     // resolution
	} {
		assert.Equal(t, tc.want, candidateNameRegex.MatchString(tc.name), "candidate(%q)", tc.name)
	}
}

// computeChanges must recover the truncated / dropped episodes for candidate
// rows and leave already-correct rows untouched.
func TestComputeChanges(t *testing.T) {
	p := Params{ClassifierConfig: classifier.Config{ParseNoiseV2: true}}
	rows := []*model.TorrentContent{
		// truncated 3-digit episode: stored E4, name says E048.
		row("a", "Pokemon S20E048 1080p WEB h264", model.Episodes{20: {4: {}}}),
		// dropped concatenated episode: stored E01, name says E01E02.
		row("b", "Some.Show.S01E01E02.1080p.WEB.h264", model.Episodes{1: {1: {}}}),
		// candidate (3-digit name) already stored correctly — must NOT change.
		row("c", "Pokemon S20E050 1080p WEB h264", model.Episodes{20: {50: {}}}),
		// not a candidate name (2-digit ep) — skipped before any recompute.
		row("d", "The Batman 2022 1080p x265", model.Episodes{}),
	}

	var st runStats
	changes := p.computeChanges(rows, &st)

	got := map[string]string{}
	for _, c := range changes {
		got[c.id] = c.newEps
	}
	assert.Equal(t, "S20E48", got["a"], "3-digit episode recovered")
	assert.Equal(t, "S01E01-02", got["b"], "concatenated episode recovered")
	assert.NotContains(t, got, "c", "already-correct candidate unchanged")
	assert.NotContains(t, got, "d", "non-candidate row skipped")
	assert.Equal(t, 3, st.candidates, "a, b, c are 3-digit/concat candidates; d is not")
}
