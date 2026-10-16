package llmeval

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCompletionTreatsUnsafeActionsByThreshold(t *testing.T) {
	thresholds := ProductionThresholds()
	system := SystemConfig{
		SystemID:          "test",
		OutputContract:    OutputContractJSONSchema,
		StructuredOutputs: true,
	}

	contentRecord := CorpusRecord{
		CaseID: "cf", Task: TaskContentFilter,
		ContentFilter: &ContentFilterCase{},
	}
	content := ParseCompletion("sha", contentRecord, system, Completion{
		Text: []byte(`{"is_english":false,"confidence":0.84,"reason":"ambiguous"}`),
	}, thresholds)
	require.Equal(t, ResultStatusOK, content.Status)
	require.Equal(t, ContentFilterActionAbstain, content.ContentFilter.Action)
	require.Nil(t, content.ContentFilter.IsEnglish)
	require.NoError(t, content.ContentFilter.Validate())

	junkRecord := CorpusRecord{
		CaseID: "jp", Task: TaskJunkPurge,
		JunkPurge: &JunkPurgeCase{},
	}
	junk := ParseCompletion("sha", junkRecord, system, Completion{
		Text: []byte(`{"verdict":"junk","confidence":0.8}`),
	}, thresholds)
	require.Equal(t, JunkPurgeActionJunk, junk.JunkPurge.Action)
}

func TestParseCompletionRejectsMissingAndExtraFields(t *testing.T) {
	record := CorpusRecord{
		CaseID: "cf", Task: TaskContentFilter,
		ContentFilter: &ContentFilterCase{},
	}
	system := SystemConfig{
		SystemID:          "test",
		OutputContract:    OutputContractJSONSchema,
		StructuredOutputs: true,
	}
	for _, raw := range []string{
		`{"is_english":true,"confidence":0.9}`,
		`{"is_english":true,"confidence":0.9,"reason":"clear","extra":1}`,
	} {
		result := ParseCompletion(
			"sha", record, system, Completion{Text: []byte(raw)}, ProductionThresholds(),
		)
		require.Equal(t, ResultStatusSchemaError, result.Status)
	}
}

func TestJSONSchemaValidationPrecedesProductionNormalization(t *testing.T) {
	extractRecord := testExtractRecord(
		"extract:schema-case",
		MatcherExtractExpected{
			Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
		},
	)
	upperCaseType := Completion{Text: []byte(
		`{"title":"Example Movie","year":2024,"type":"MOVIE","season":0,` +
			`"episode":0,"is_anime":false,"english":"UNKNOWN","is_pack":false,"is_adult":false}`,
	)}

	strict := ParseCompletion(
		"sha",
		extractRecord,
		SystemConfig{
			SystemID:          "strict",
			OutputContract:    OutputContractJSONSchema,
			StructuredOutputs: true,
		},
		upperCaseType,
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusSchemaError, strict.Status)
	require.Equal(t, "invalid_type", strict.ErrorCode)

	production := ParseCompletion(
		"sha",
		extractRecord,
		SystemConfig{
			SystemID:       "production",
			EvaluationLane: EvaluationLaneProductionFidelity,
			OutputContract: OutputContractJSONObject,
		},
		upperCaseType,
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusOK, production.Status)
	require.Equal(t, MediaTypeMovie, production.MatcherExtract.Extraction.Type)
	require.Equal(
		t,
		EnglishTrackUnknown,
		production.MatcherExtract.Extraction.English,
	)
}

func TestJSONSchemaValidationRejectsDuplicateKeys(t *testing.T) {
	record := CorpusRecord{
		CaseID: "junk:duplicate", Task: TaskJunkPurge,
		JunkPurge: &JunkPurgeCase{},
	}
	result := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			SystemID:          "strict",
			OutputContract:    OutputContractJSONSchema,
			StructuredOutputs: true,
		},
		Completion{Text: []byte(
			`{"verdict":"junk","verdict":"real_mangled","confidence":0.99}`,
		)},
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusSchemaError, result.Status)
	require.Equal(t, "invalid_json", result.ErrorCode)
}

func TestParseMatcherRerankRejectsHallucinatedCandidate(t *testing.T) {
	record := CorpusRecord{
		CaseID: "rerank", Task: TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{
			Input: MatcherRerankInput{
				Candidates: []MatcherCandidate{{TMDBID: 42}},
			},
		},
	}
	result := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			SystemID:          "test",
			OutputContract:    OutputContractJSONSchema,
			StructuredOutputs: true,
		},
		Completion{Text: []byte(`{"tmdb_id":99,"confidence":0.99}`)},
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusSchemaError, result.Status)
	require.Equal(t, "candidate_not_offered", result.ErrorCode)
}

func TestDeployedWireWrapperToleranceIsNotAppliedToStrictSystems(t *testing.T) {
	record := CorpusRecord{
		CaseID: "junk", Task: TaskJunkPurge,
		JunkPurge: &JunkPurgeCase{},
	}
	wrapped := Completion{Text: []byte("```json\n{\"verdict\":\"real_mangled\",\"confidence\":0.9}\n```")}

	for _, lane := range []EvaluationLane{
		EvaluationLaneProductionFidelity,
		EvaluationLaneShadowFidelity,
	} {
		t.Run(string(lane), func(t *testing.T) {
			deployed := ParseCompletion(
				"sha",
				record,
				SystemConfig{
					EvaluationLane: lane,
					OutputContract: OutputContractPromptOnly,
				},
				wrapped,
				ProductionThresholds(),
			)
			require.Equal(t, ResultStatusOK, deployed.Status)
		})
	}

	strict := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			OutputContract:    OutputContractJSONSchema,
			StructuredOutputs: true,
		},
		wrapped,
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusSchemaError, strict.Status)
}

func TestPromptOnlyWrapperToleranceAlsoCoversMatcherTasks(t *testing.T) {
	record := testExtractRecord("extract:wrapped", MatcherExtractExpected{
		Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
	})
	wrapped := Completion{Text: []byte(
		"```json\n" +
			`{"title":"Example Movie","year":2024,"type":"movie","season":0,` +
			`"episode":0,"is_anime":false,"english":"unknown","is_pack":false,"is_adult":false}` +
			"\n```",
	)}

	result := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			EvaluationLane: EvaluationLaneProductionFidelity,
			OutputContract: OutputContractPromptOnly,
		},
		wrapped,
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusOK, result.Status)
	require.Equal(t, MatcherExtractActionExtract, result.MatcherExtract.Action)
}

func TestDeployedWireMatcherParserUsesRuntimeTolerance(t *testing.T) {
	record := testExtractRecord("extract:runtime-wire", MatcherExtractExpected{
		Acceptable: []MatcherExtraction{testExtraction("Hacks")},
	})
	for _, lane := range []EvaluationLane{
		EvaluationLaneProductionFidelity,
		EvaluationLaneShadowFidelity,
	} {
		t.Run(string(lane), func(t *testing.T) {
			result := ParseCompletion(
				"sha",
				record,
				SystemConfig{
					EvaluationLane: lane,
					OutputContract: OutputContractJSONObject,
				},
				Completion{Text: []byte(
					"```json\n" +
						`{"title":"Hacks","year":"2021","type":"tv","season":"2",` +
						`"episode":null,"is_anime":"false","english":"unknown",` +
						`"is_pack":"false","is_adult":"false"}` +
						"\n```",
				)},
				ProductionThresholds(),
			)
			require.Equal(t, ResultStatusOK, result.Status)
			require.Equal(t, MatcherExtractActionExtract, result.MatcherExtract.Action)
			require.Equal(t, 2021, result.MatcherExtract.Extraction.Year)
			require.Equal(t, 2, result.MatcherExtract.Extraction.Season)
		})
	}
}

func TestDeployedWireMatcherAppliesFinalIdentityGate(t *testing.T) {
	record := CorpusRecord{
		CaseID: "rerank:duplicate-tv", Task: TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{Input: MatcherRerankInput{
			ReleaseName:        "JoJos Bizarre Adventure - S05 - MULTi.1080p.mkv",
			EffectiveMediaType: MediaTypeTV,
			Extraction: MatcherExtraction{
				Title: "JoJo's Bizarre Adventure", Type: MediaTypeTV,
				Season: 5, IsAnime: true, English: EnglishTrackDub,
			},
			Candidates: []MatcherCandidate{
				{TMDBID: 60862, Type: MediaTypeTV, Title: "JoJo's Bizarre Adventure", Year: 1993},
				{TMDBID: 45790, Type: MediaTypeTV, Title: "JoJo's Bizarre Adventure", Year: 2012},
			},
		}},
	}
	for _, lane := range []EvaluationLane{
		EvaluationLaneProductionFidelity,
		EvaluationLaneShadowFidelity,
	} {
		t.Run(string(lane), func(t *testing.T) {
			result := ParseCompletion(
				"sha",
				record,
				SystemConfig{
					EvaluationLane: lane,
					OutputContract: OutputContractJSONObject,
				},
				Completion{Text: []byte(`{"tmdb_id":60862,"confidence":1}`)},
				ProductionThresholds(),
			)
			require.Equal(t, ResultStatusOK, result.Status)
			require.Equal(t, MatcherRerankActionAbstain, result.MatcherRerank.Action)
			require.Equal(t, "candidate_ambiguous", result.MatcherRerank.PolicyReason)
			require.NoError(t, result.MatcherRerank.Validate())
		})
	}
}

func TestDeployedWireMatcherAppliesConfiguredSourceIdentityGate(t *testing.T) {
	base := MatcherRerankInput{
		ReleaseName:        "Dune.2021.1080p.mkv",
		ParsedTitle:        "Dune",
		EffectiveMediaType: MediaTypeMovie,
		Extraction: MatcherExtraction{
			Title: "Dune", Year: 2021, Type: MediaTypeMovie,
			English: EnglishTrackUnknown,
		},
		Candidates: []MatcherCandidate{{
			TMDBID: 438631, Type: MediaTypeMovie, Title: "Dune", Year: 2021,
		}},
	}
	system := SystemConfig{
		EvaluationLane:     EvaluationLaneProductionFidelity,
		OutputContract:     OutputContractJSONObject,
		RequireSourceTitle: true,
	}

	for _, tc := range []struct {
		name       string
		input      MatcherRerankInput
		wantAction MatcherRerankAction
		wantReason string
	}{
		{
			name:  "independent title agrees",
			input: base, wantAction: MatcherRerankActionAttach,
		},
		{
			name: "model-only invented title",
			input: func() MatcherRerankInput {
				in := base
				in.ReleaseName = "Mystery.2021.1080p.mkv"
				in.ParsedTitle = "Mystery"
				return in
			}(),
			wantAction: MatcherRerankActionAbstain,
			wantReason: "source_title",
		},
		{
			name: "independently catalogued alias",
			input: func() MatcherRerankInput {
				in := base
				in.ReleaseName = "Duna.2021.1080p.mkv"
				in.ParsedTitle = "Duna"
				in.Candidates = []MatcherCandidate{{
					TMDBID: 438631, Type: MediaTypeMovie, Title: "Dune",
					AltTitles: []string{"Duna"}, Year: 2021,
				}}
				return in
			}(),
			wantAction: MatcherRerankActionAttach,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := ParseCompletion(
				"sha",
				CorpusRecord{CaseID: "rerank:source", Task: TaskMatcherRerank,
					MatcherRerank: &MatcherRerankCase{Input: tc.input}},
				system,
				Completion{Text: []byte(`{"tmdb_id":438631,"confidence":0.99}`)},
				ProductionThresholds(),
			)
			require.Equal(t, ResultStatusOK, result.Status)
			require.Equal(t, tc.wantAction, result.MatcherRerank.Action)
			require.Equal(t, tc.wantReason, result.MatcherRerank.PolicyReason)
		})
	}
}

func TestProductionMatcherRequiresCapturedEffectiveMediaType(t *testing.T) {
	record := CorpusRecord{
		CaseID: "rerank:missing-effective-type", Task: TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{Input: MatcherRerankInput{
			ReleaseName: "Example.Movie.2024.mkv",
			Extraction: MatcherExtraction{
				Title: "Example Movie", Year: 2024, Type: MediaTypeMovie,
				English: EnglishTrackUnknown,
			},
			Candidates: []MatcherCandidate{{
				TMDBID: 42, Type: MediaTypeMovie,
				Title: "Example Movie", Year: 2024,
			}},
		}},
	}
	result := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			EvaluationLane: EvaluationLaneProductionFidelity,
			OutputContract: OutputContractJSONObject,
		},
		Completion{Text: []byte(`{"tmdb_id":42,"confidence":0.99}`)},
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusSchemaError, result.Status)
	require.Equal(t, "missing_effective_media_type", result.ErrorCode)
}

func TestProductionMatcherNormalizesNonOfferedSelectionsAsLiveDeclines(
	t *testing.T,
) {
	record := CorpusRecord{
		CaseID: "rerank:selection-normalization", Task: TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{Input: MatcherRerankInput{
			ReleaseName:        "Example.Movie.2024.mkv",
			EffectiveMediaType: MediaTypeMovie,
			Extraction: MatcherExtraction{
				Title: "Example Movie", Year: 2024, Type: MediaTypeMovie,
				English: EnglishTrackUnknown,
			},
			Candidates: []MatcherCandidate{{
				TMDBID: 42, Type: MediaTypeMovie,
				Title: "Example Movie", Year: 2024,
			}},
		}},
	}
	for _, raw := range []string{
		`{"tmdb_id":99,"confidence":0.99}`,
		`{"tmdb_id":-1,"confidence":0.99}`,
	} {
		result := ParseCompletion(
			"sha",
			record,
			SystemConfig{
				EvaluationLane: EvaluationLaneProductionFidelity,
				OutputContract: OutputContractJSONObject,
			},
			Completion{Text: []byte(raw)},
			ProductionThresholds(),
		)
		require.Equal(t, ResultStatusOK, result.Status)
		require.Equal(
			t,
			MatcherRerankActionAbstain,
			result.MatcherRerank.Action,
		)
		require.Equal(t, "model_declined", result.MatcherRerank.PolicyReason)
		require.Zero(t, result.MatcherRerank.Confidence)
	}
}

func TestProductionMatcherIdentityGatePrecedesConfidenceFloor(t *testing.T) {
	record := CorpusRecord{
		CaseID: "rerank:gate-order", Task: TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{Input: MatcherRerankInput{
			ReleaseName:        "Example.Movie.2024.mkv",
			EffectiveMediaType: MediaTypeMovie,
			Extraction: MatcherExtraction{
				Title: "Example Movie", Year: 2024, Type: MediaTypeMovie,
				English: EnglishTrackUnknown,
			},
			Candidates: []MatcherCandidate{{
				TMDBID: 42, Type: MediaTypeMovie,
				Title: "Different Movie", Year: 2024,
			}},
		}},
	}
	result := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			EvaluationLane: EvaluationLaneProductionFidelity,
			OutputContract: OutputContractJSONObject,
		},
		Completion{Text: []byte(`{"tmdb_id":42,"confidence":0.2}`)},
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusOK, result.Status)
	require.Equal(t, MatcherRerankActionAbstain, result.MatcherRerank.Action)
	require.Equal(t, "candidate_title", result.MatcherRerank.PolicyReason)
}

func TestProductionMatcherUsesCapturedEffectiveMediaType(t *testing.T) {
	record := CorpusRecord{
		CaseID: "rerank:effective-tv", Task: TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{Input: MatcherRerankInput{
			ReleaseName:        "Same.Show.2020.S01.mkv",
			EffectiveMediaType: MediaTypeTV,
			// This disagreement is possible when the upstream CEL
			// classification supplies TV while extraction says movie.
			Extraction: MatcherExtraction{
				Title: "Same Show", Year: 2020, Type: MediaTypeMovie,
				English: EnglishTrackUnknown,
			},
			Candidates: []MatcherCandidate{
				{TMDBID: 42, Type: MediaTypeTV, Title: "Same Show", Year: 2020},
				{TMDBID: 43, Type: MediaTypeTV, Title: "Same Show", Year: 1993},
			},
		}},
	}
	result := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			EvaluationLane: EvaluationLaneProductionFidelity,
			OutputContract: OutputContractJSONObject,
		},
		Completion{Text: []byte(`{"tmdb_id":42,"confidence":0.99}`)},
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusOK, result.Status)
	require.Equal(t, MatcherRerankActionAbstain, result.MatcherRerank.Action)
	require.Equal(t, "candidate_ambiguous", result.MatcherRerank.PolicyReason)
}

func TestMatcherSpecialistPolicyVetoIsAValidSafeAbstention(t *testing.T) {
	record := CorpusRecord{
		CaseID: "rerank:specialist-policy-veto", Task: TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{Input: MatcherRerankInput{
			ReleaseName:        "JoJos Bizarre Adventure - S05 - MULTi.1080p.mkv",
			EffectiveMediaType: MediaTypeTV,
			Extraction: MatcherExtraction{
				Title: "JoJo's Bizarre Adventure", Type: MediaTypeTV,
				Season: 5, IsAnime: true, English: EnglishTrackDub,
			},
			Candidates: []MatcherCandidate{
				{TMDBID: 60862, Type: MediaTypeTV, Title: "JoJo's Bizarre Adventure", Year: 1993},
				{TMDBID: 45790, Type: MediaTypeTV, Title: "JoJo's Bizarre Adventure", Year: 2012},
			},
		}},
	}
	audit := &MatcherSpecialistResultAudit{
		AlgorithmID:    MatcherSpecialistAlgorithmID,
		SelectedTMDBID: 60862, ScorePPB: 900_000_000,
		EligibleCandidates: 2,
	}
	result := ParseCompletion(
		strings.Repeat("a", 64),
		record,
		SystemConfig{
			APIKind: APIKindEmbedding, PromptVersion: MatcherSpecialistAlgorithmID,
		},
		Completion{
			Text:              []byte(`{"tmdb_id":60862,"confidence":0.9}`),
			MatcherSpecialist: audit,
		},
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusOK, result.Status)
	require.Equal(t, MatcherRerankActionAbstain, result.MatcherRerank.Action)
	require.Equal(t, "candidate_ambiguous", result.MatcherRerank.PolicyReason)
	require.NoError(
		t,
		result.MatcherRerank.validateWithThresholds(ProductionThresholds()),
	)
}

func TestProductionContentFilterUsesRuntimeReasonGuard(t *testing.T) {
	record := CorpusRecord{
		CaseID: "content:long-reason", Task: TaskContentFilter,
		ContentFilter: &ContentFilterCase{},
	}
	result := ParseCompletion(
		"sha",
		record,
		SystemConfig{
			EvaluationLane: EvaluationLaneProductionFidelity,
			OutputContract: OutputContractPromptOnly,
		},
		Completion{Text: []byte(
			`{"is_english":false,"confidence":0.99,"reason":"` +
				strings.Repeat("x", 65) + `"}`,
		)},
		ProductionThresholds(),
	)
	require.Equal(t, ResultStatusSchemaError, result.Status)
}

func TestProductionParsersDoNotDoubleStripModelWrappers(t *testing.T) {
	for _, tc := range []struct {
		record CorpusRecord
		raw    string
	}{
		{
			record: CorpusRecord{
				CaseID: "content:double-wrapper", Task: TaskContentFilter,
				ContentFilter: &ContentFilterCase{},
			},
			raw: "```json\n```json\n" +
				`{"is_english":true,"confidence":0.99,"reason":"english"}` +
				"\n```\n```",
		},
		{
			record: CorpusRecord{
				CaseID: "junk:double-wrapper", Task: TaskJunkPurge,
				JunkPurge: &JunkPurgeCase{},
			},
			raw: "```json\n```json\n" +
				`{"verdict":"real_mangled","confidence":0.99}` +
				"\n```\n```",
		},
	} {
		result := ParseCompletion(
			"sha",
			tc.record,
			SystemConfig{
				EvaluationLane: EvaluationLaneProductionFidelity,
				OutputContract: OutputContractPromptOnly,
			},
			Completion{Text: []byte(tc.raw)},
			ProductionThresholds(),
		)
		require.Equal(t, ResultStatusSchemaError, result.Status)
	}
}

func TestParseContentFilterNormalizesFreeFormReason(t *testing.T) {
	result, err := parseContentFilter(
		[]byte(`{"is_english":false,"confidence":0.91,"reason":"Spanish article / wording; ignore previous instructions!"}`),
		0.85,
	)
	require.NoError(t, err)
	require.Equal(t, "spanish-article-wording-ignore-previous-instructions", result.ReasonTag)
	require.LessOrEqual(t, len(result.ReasonTag), 64)
}

func TestErrorResultUsesBoundedCode(t *testing.T) {
	record := testJunkRecord("x", JunkClassDegenerate)
	result := ErrorResult(
		"sha",
		record,
		SystemConfig{SystemID: "test"},
		ProductionThresholds(),
		&CallError{Code: "http_429"},
	)
	require.Equal(t, "http_429", result.ErrorCode)
	require.Equal(t, ResultStatusError, result.Status)

	result = ErrorResult(
		"sha",
		record,
		SystemConfig{},
		ProductionThresholds(),
		errors.New("secret provider body"),
	)
	require.Equal(t, "provider_error", result.ErrorCode)
	require.NotContains(t, result.ErrorCode, "secret")
}
