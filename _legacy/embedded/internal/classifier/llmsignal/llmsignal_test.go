package llmsignal

import (
	"context"
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestRecordAndEnglish(t *testing.T) {
	// No holder: Record is a no-op and English is invalid.
	bare := context.Background()
	Record(bare, "dub", true)
	assert.False(t, English(bare).Valid, "no holder must yield invalid")

	// Definite anime read is recorded.
	ctx := WithHolder(context.Background())
	Record(ctx, "none", true)
	got := English(ctx)
	assert.True(t, got.Valid)
	assert.Equal(t, model.EnglishAudioNone, got.EnglishAudio)

	// Non-anime and indefinite reads are dropped.
	for _, c := range []struct {
		english string
		isAnime bool
	}{
		{"dub", false},
		{"unknown", true},
		{"", true},
		{"garbage", true},
	} {
		ctx := WithHolder(context.Background())
		Record(ctx, c.english, c.isAnime)
		assert.Falsef(t, English(ctx).Valid, "english=%q isAnime=%v must not record", c.english, c.isAnime)
	}

	// A later definite read overwrites an earlier one (same run, e.g. alias
	// normalisation upgraded the extraction).
	ctx2 := WithHolder(context.Background())
	Record(ctx2, "sub", true)
	Record(ctx2, "dub", true)
	assert.Equal(t, model.EnglishAudioDub, English(ctx2).EnglishAudio)
}
