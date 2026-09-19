package contentfilter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvaluationLLMEligibleReusesProductionGatesWithoutAClient(
	t *testing.T,
) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.LLMEnabled = true
	filter := New(cfg)

	base := Input{
		Title:            "Unclassified Latin Title",
		PrimaryExtension: "mkv",
		ContentType:      "movie",
	}
	require.True(t, filter.EvaluationLLMEligible(base))

	private := base
	private.Private = true
	require.False(t, filter.EvaluationLLMEligible(private))

	knownLanguage := base
	knownLanguage.Languages = []string{"en"}
	require.False(t, filter.EvaluationLLMEligible(knownLanguage))

	nonLatin := base
	nonLatin.Title = "Русский фильм"
	require.False(t, filter.EvaluationLLMEligible(nonLatin))

	blocked := base
	blocked.PrimaryExtension = "exe"
	require.False(t, filter.EvaluationLLMEligible(blocked))

	anime := base
	anime.Title = "[SubsPlease] Sousou no Frieren - 01"
	require.False(t, filter.EvaluationLLMEligible(anime))
}
