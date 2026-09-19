package processor

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

// newTorrentContent merges the deterministic english_audio derivation with
// the LLM extraction under explicit precedence: a deterministic name signal
// wins (source 'name'); otherwise a definite LLM read fills in (source
// 'llm', the only path that can assign 'none'); neither leaves both NULL.
func TestNewTorrentContent_EnglishAudioPrecedence(t *testing.T) {
	llmNone := model.NewNullEnglishAudio(model.EnglishAudioNone)
	llmDub := model.NewNullEnglishAudio(model.EnglishAudioDub)

	cases := []struct {
		name       string
		llm        model.NullEnglishAudio
		wantAudio  model.NullEnglishAudio
		wantSource model.NullString
		notes      string
	}{
		{
			name:       "[Anime Time] Naruto Dual Audio 1080p",
			llm:        llmNone,
			wantAudio:  model.NewNullEnglishAudio(model.EnglishAudioDub),
			wantSource: model.NewNullString(model.EnglishAudioSourceName),
			notes:      "deterministic-explicit beats a conflicting LLM read",
		},
		{
			name:       "[Ohys-Raws] Sousou no Frieren - 12 (1080p)",
			llm:        llmNone,
			wantAudio:  llmNone,
			wantSource: model.NewNullString(model.EnglishAudioSourceLLM),
			notes:      "no deterministic signal: the LLM 'none' (raw) fills in",
		},
		{
			name:       "[SomeGroup] Show Title - 05 (1080p)",
			llm:        model.NullEnglishAudio{},
			wantAudio:  model.NullEnglishAudio{},
			wantSource: model.NullString{},
			notes:      "no signal from either layer: both stay NULL",
		},
		{
			name:       "The Wire S01E01 1080p BluRay x264-GROUP",
			llm:        llmDub,
			wantAudio:  llmDub,
			wantSource: model.NewNullString(model.EnglishAudioSourceLLM),
			notes:      "non-anime by name signals, but a definite LLM read still lands (the LLM judged anime-ness itself)",
		},
	}

	for _, tc := range cases {
		got := newTorrentContent(model.Torrent{Name: tc.name}, classification.Result{}, tc.llm)
		assert.Equalf(t, tc.wantAudio, got.EnglishAudio, "audio: %s (%s)", tc.name, tc.notes)
		assert.Equalf(t, tc.wantSource, got.EnglishAudioSource, "source: %s (%s)", tc.name, tc.notes)
	}
}
