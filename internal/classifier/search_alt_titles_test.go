package classifier

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

//nolint:gosmopolitan // CJK alt-titles are exactly what this feature matches.
func TestContentMatchCandidates(t *testing.T) {
	t.Parallel()

	item := search.ContentResultItem{
		Content: model.Content{
			Title:         "Demon Slayer: Kimetsu no Yaiba",
			OriginalTitle: model.NewNullString("鬼滅の刃"),
			Attributes: []model.ContentAttribute{
				{
					Source: model.SourceTmdb,
					Key:    model.AltTitleAttributePrefix + "br:cafe0123",
					Value:  "Demon Slayer",
				},
				{Source: model.SourceImdb, Key: "id", Value: "tt9335498"},
			},
		},
	}

	t.Run("disabled: canonical titles only", func(t *testing.T) {
		t.Parallel()

		candidates := contentMatchCandidates(item, false)

		assert.Equal(t, []string{"Demon Slayer: Kimetsu no Yaiba", "鬼滅の刃"}, candidates)
	})

	t.Run("enabled: alt titles included, other attributes not", func(t *testing.T) {
		t.Parallel()

		candidates := contentMatchCandidates(item, true)

		assert.Equal(t, []string{"Demon Slayer: Kimetsu no Yaiba", "鬼滅の刃", "Demon Slayer"}, candidates)
		assert.NotContains(t, candidates, "tt9335498")
	})
}
