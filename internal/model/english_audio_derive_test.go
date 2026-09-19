package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeriveEnglishAudio(t *testing.T) {
	t.Parallel()

	assert.Equal(t, NewNullEnglishAudio(EnglishAudioDub), DeriveEnglishAudio(true, true, false))
	assert.Equal(t, NewNullEnglishAudio(EnglishAudioDub), DeriveEnglishAudio(true, true, true), "audio wins over sub")
	assert.Equal(t, NewNullEnglishAudio(EnglishAudioSub), DeriveEnglishAudio(true, false, true))
	assert.Equal(t, NullEnglishAudio{}, DeriveEnglishAudio(true, false, false), "no signal stays unknown")
	assert.Equal(t, NullEnglishAudio{}, DeriveEnglishAudio(false, true, true), "non-anime never labelled")
}
