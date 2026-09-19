package model

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/fts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContentUpdateTsv_altTitles(t *testing.T) {
	t.Parallel()

	content := Content{
		Title: "Demon Slayer",
		Attributes: []ContentAttribute{
			{Source: SourceTmdb, Key: AltTitleAttributePrefix + "jp:deadbeef", Value: "Kimetsu no Yaiba"},
			{Source: SourceImdb, Key: "id", Value: "tt9335498"},
			{Source: SourceTmdb, Key: "poster_path", Value: "/poster.jpg"},
		},
	}

	content.UpdateTsv()

	// Canonical title at weight A.
	require.Contains(t, content.Tsv, "demon")
	assertHasWeight(t, content.Tsv["demon"], fts.TsvectorWeightA)

	// Alt title lexemes present, at weight B — below the canonical title.
	require.Contains(t, content.Tsv, "kimetsu")
	require.Contains(t, content.Tsv, "yaiba")
	assertHasWeight(t, content.Tsv["kimetsu"], fts.TsvectorWeightB)

	// The id attribute keeps its weight-D behavior.
	require.Contains(t, content.Tsv, "tt9335498")
	assertHasWeight(t, content.Tsv["tt9335498"], fts.TsvectorWeightD)

	// Non-title attributes stay out of the tsvector.
	assert.NotContains(t, content.Tsv, "/poster.jpg")
	assert.NotContains(t, content.Tsv, "poster.jpg")
}

func assertHasWeight(t *testing.T, positions map[int]fts.TsvectorWeight, want fts.TsvectorWeight) {
	t.Helper()

	for _, w := range positions {
		if w == want {
			return
		}
	}

	t.Errorf("expected weight %c in %v", want, positions)
}
