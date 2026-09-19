package llmeval

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScoreCarriesExactCampaignRunBinding(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:campaign-score", LanguageEnglish),
	})
	result := contentResult(
		corpus,
		"content:campaign-score",
		ContentFilterActionKeep,
		boolPointer(true),
	)
	plan := testCampaignPlan()
	binding, err := plan.RunBinding(
		strings.Repeat("9", 64),
		"run-candidate-contentfilter",
	)
	require.NoError(t, err)
	binding.SystemID = result.System.SystemID
	binding.SystemManifestSHA256 = result.ExecutionAudit.ManifestSHA256
	binding.Corpus.SHA256 = corpus.SHA256
	binding.RouteSnapshot.SHA256 =
		result.ExecutionAudit.Route.SnapshotSHA256
	result.ExecutionAudit.Campaign = &binding

	report, err := Score(corpus, []ResultRecord{result}, testEvaluatorBuildSHA)
	require.NoError(t, err)
	require.NotNil(t, report.Campaign)
	require.Equal(t, binding, *report.Campaign)
	require.NotSame(t, result.ExecutionAudit.Campaign, report.Campaign)
}

func TestScoreDistinguishesSafetyErrorsAndDeduplicatesPackedUsage(t *testing.T) {
	records := []CorpusRecord{
		testExtractRecord("extract:correct", MatcherExtractExpected{
			Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
		}),
		testExtractRecord("extract:missed", MatcherExtractExpected{
			Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
		}),
		testExtractRecord("extract:abstain", MatcherExtractExpected{AllowAbstain: true}),
		testExtractRecord("extract:wrong", MatcherExtractExpected{
			Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
		}),
		testRerankRecord("rerank:correct", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
		testRerankRecord("rerank:wrong", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
		testRerankRecord("rerank:missed", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
		testRerankRecord("rerank:abstain", MatcherRerankExpected{AllowAbstain: true}),
		testContentRecord("content:false-drop", LanguageEnglish),
		testContentRecord("content:missed", LanguageNonEnglish),
		testContentRecord("content:schema", LanguageEnglish),
		testContentRecord("content:abstain", LanguageNonEnglish),
		testContentRecord("content:correct-keep", LanguageEnglish),
		testContentRecord("content:correct-drop", LanguageNonEnglish),
		testJunkRecord("junk:false-junk", JunkClassMovie),
		testJunkRecord("junk:missed", JunkClassDegenerate),
		testJunkRecord("junk:error", JunkClassTVShow),
		testJunkRecord("junk:abstain", JunkClassDegenerate),
		testJunkRecord("junk:correct-keep", JunkClassMovie),
		testJunkRecord("junk:correct-junk", JunkClassDegenerate),
	}
	corpus := mustCorpus(t, records)

	results := []ResultRecord{
		extractResult(corpus, "extract:correct", MatcherExtractActionExtract, testExtraction("Example Movie")),
		extractResult(corpus, "extract:missed", MatcherExtractActionAbstain, MatcherExtraction{}),
		extractResult(corpus, "extract:abstain", MatcherExtractActionAbstain, MatcherExtraction{}),
		extractResult(corpus, "extract:wrong", MatcherExtractActionExtract, testExtraction("Different Movie")),
		rerankResult(corpus, "rerank:correct", MatcherRerankActionAttach, 101),
		rerankResult(corpus, "rerank:wrong", MatcherRerankActionAttach, 202),
		rerankResult(corpus, "rerank:missed", MatcherRerankActionAbstain, 0),
		rerankResult(corpus, "rerank:abstain", MatcherRerankActionAbstain, 0),
		contentResult(corpus, "content:false-drop", ContentFilterActionDrop, boolPointer(false)),
		contentResult(corpus, "content:missed", ContentFilterActionKeep, boolPointer(true)),
		errorResult(corpus, "content:schema", TaskContentFilter, ResultStatusSchemaError, "invalid-json"),
		contentResult(corpus, "content:abstain", ContentFilterActionAbstain, nil),
		contentResult(corpus, "content:correct-keep", ContentFilterActionKeep, boolPointer(true)),
		contentResult(corpus, "content:correct-drop", ContentFilterActionDrop, boolPointer(false)),
		junkResult(corpus, "junk:false-junk", JunkPurgeActionJunk, JunkVerdictJunk),
		junkResult(corpus, "junk:missed", JunkPurgeActionKeep, JunkVerdictRealMangled),
		errorResult(corpus, "junk:error", TaskJunkPurge, ResultStatusError, "timeout"),
		junkResult(corpus, "junk:abstain", JunkPurgeActionAbstain, JunkVerdictUnsure),
		junkResult(corpus, "junk:correct-keep", JunkPurgeActionKeep, JunkVerdictRealMangled),
		junkResult(corpus, "junk:correct-junk", JunkPurgeActionJunk, JunkVerdictJunk),
	}

	packedUsage := Usage{
		RequestID:         "request:content-pack",
		InputTokens:       100,
		CachedInputTokens: 20,
		OutputTokens:      30,
		ReasoningTokens:   5,
		CostMicroUSD:      123,
	}
	for i := range results {
		if results[i].Task == TaskContentFilter {
			results[i].Usage = packedUsage
		}
	}

	report, err := Score(corpus, results, testEvaluatorBuildSHA)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}

	if report.Outcomes.Cases != 20 || report.Outcomes.Correct.Count != 8 {
		t.Fatalf("overall outcomes = %#v", report.Outcomes)
	}
	if report.Outcomes.SchemaErrors.Count != 1 ||
		report.Outcomes.RuntimeErrors.Count != 1 ||
		report.Outcomes.Abstentions.Count != 6 {
		t.Fatalf("overall error/abstention outcomes = %#v", report.Outcomes)
	}
	if report.Usage.Results != 20 || report.Usage.Requests != 15 {
		t.Fatalf("overall usage cardinality = %#v", report.Usage)
	}
	if report.Usage.InputTokens != 100 ||
		report.Usage.OutputTokens != 30 ||
		report.Usage.TotalTokens != 130 ||
		report.Usage.CostMicroUSD != 123 {
		t.Fatalf("packed usage was not deduplicated: %#v", report.Usage)
	}

	extract := findTaskScore(t, report, TaskMatcherExtract)
	if extract.MatcherExtract.CorrectExtractions.Count != 1 ||
		extract.MatcherExtract.IncorrectExtractions.Count != 1 ||
		extract.MatcherExtract.MissedExtractions.Count != 1 ||
		extract.MatcherExtract.CorrectAbstentions.Count != 1 {
		t.Fatalf("matcher extract metrics = %#v", extract.MatcherExtract)
	}

	rerank := findTaskScore(t, report, TaskMatcherRerank)
	if rerank.MatcherRerank.CorrectAttachments.Count != 1 ||
		rerank.MatcherRerank.WrongAttachments.Count != 1 ||
		rerank.MatcherRerank.MissedByAbstention.Count != 1 ||
		rerank.MatcherRerank.CorrectAbstentions.Count != 1 {
		t.Fatalf("matcher rerank metrics = %#v", rerank.MatcherRerank)
	}
	if rerank.MatcherRerank.WrongAttachments.Denominator != 2 {
		t.Fatalf("wrong-attachment denominator = %d, want 2", rerank.MatcherRerank.WrongAttachments.Denominator)
	}

	content := findTaskScore(t, report, TaskContentFilter)
	if content.ContentFilter.FalseDrops.Count != 1 ||
		content.ContentFilter.FalseDrops.Denominator != 3 ||
		content.ContentFilter.MissedNonEnglish.Count != 1 ||
		content.ContentFilter.MissedNonEnglish.Denominator != 3 {
		t.Fatalf("contentfilter safety metrics = %#v", content.ContentFilter)
	}
	if content.Outcomes.SchemaErrors.Count != 1 || content.Outcomes.Abstentions.Count != 1 {
		t.Fatalf("contentfilter outcomes = %#v", content.Outcomes)
	}
	if content.Usage.Results != 6 || content.Usage.Requests != 1 {
		t.Fatalf("contentfilter packed usage = %#v", content.Usage)
	}

	junk := findTaskScore(t, report, TaskJunkPurge)
	if junk.JunkPurge.FalseJunk.Count != 1 ||
		junk.JunkPurge.FalseJunk.Denominator != 3 ||
		junk.JunkPurge.MissedJunk.Count != 1 ||
		junk.JunkPurge.MissedJunk.Denominator != 3 {
		t.Fatalf("junkpurge safety metrics = %#v", junk.JunkPurge)
	}
	if junk.Outcomes.RuntimeErrors.Count != 1 || junk.Outcomes.Abstentions.Count != 1 {
		t.Fatalf("junkpurge outcomes = %#v", junk.Outcomes)
	}
}

func TestScoreSeparatesNaturalAndSafetySuites(t *testing.T) {
	natural := testContentRecord("content:natural", LanguageEnglish)
	natural.Label.Provenance = LabelProvenanceHumanReview
	natural.SliceIDs = []string{"suite:natural", "hard_must_keep_english"}
	safety := testContentRecord("content:safety", LanguageEnglish)
	safety.Label.Provenance = LabelProvenanceHumanReview
	safety.SliceIDs = []string{"suite:safety", "hard_must_keep_english"}
	corpus := mustCorpus(t, []CorpusRecord{natural, safety})
	results := []ResultRecord{
		contentResult(
			corpus,
			natural.CaseID,
			ContentFilterActionKeep,
			boolPointer(true),
		),
		contentResult(
			corpus,
			safety.CaseID,
			ContentFilterActionDrop,
			boolPointer(false),
		),
	}

	report, err := Score(corpus, results, testEvaluatorBuildSHA)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if report.QualityAggregation != "suite_separated" ||
		report.Outcomes != nil ||
		len(report.Tasks) != 0 ||
		len(report.Suites) != 2 {
		t.Fatalf("suite-separated report = %#v", report)
	}
	if report.Suites[0].Suite != "natural" ||
		report.Suites[0].Outcomes.Correct.Count != 1 {
		t.Fatalf("natural suite = %#v", report.Suites[0])
	}
	if report.Suites[1].Suite != "safety" ||
		report.Suites[1].Outcomes.Correct.Count != 0 {
		t.Fatalf("safety suite = %#v", report.Suites[1])
	}
}

func TestScoreRequiresOnePrimarySuiteForProductionGold(t *testing.T) {
	record := testContentRecord("content:missing-suite", LanguageEnglish)
	record.Label.Provenance = LabelProvenanceHumanReview
	record.SliceIDs = []string{"fixture"}
	corpus := mustCorpus(t, []CorpusRecord{record})
	results := []ResultRecord{
		contentResult(
			corpus,
			record.CaseID,
			ContentFilterActionKeep,
			boolPointer(true),
		),
	}

	_, err := Score(corpus, results, testEvaluatorBuildSHA)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("Score error = %v, want missing primary suite rejection", err)
	}
}

func TestScoreMatcherExtractReportsNormalizedCompositeAndFieldMetrics(t *testing.T) {
	normalizedGold := testExtraction("The Movie Part II")
	mixedGoldA := testExtraction("Alpha")
	mixedGoldB := mixedGoldA
	mixedGoldB.Title = "Beta"
	mixedGoldB.Year = 2025
	mixedGoldB.IsAnime = true

	records := []CorpusRecord{
		testExtractRecord("extract:normalized", MatcherExtractExpected{
			Acceptable: []MatcherExtraction{normalizedGold},
		}),
		testExtractRecord("extract:mixed", MatcherExtractExpected{
			Acceptable: []MatcherExtraction{mixedGoldA, mixedGoldB},
		}),
		testExtractRecord("extract:no-fields", MatcherExtractExpected{
			AllowAbstain: true,
		}),
	}
	corpus := mustCorpus(t, records)

	normalizedActual := normalizedGold
	normalizedActual.Title = "Movie Part 2"
	mixedActual := mixedGoldA
	mixedActual.Year = mixedGoldB.Year
	mixedActual.IsAnime = mixedGoldB.IsAnime
	noFieldsActual := testExtraction("Invented")
	results := []ResultRecord{
		extractResult(
			corpus,
			"extract:normalized",
			MatcherExtractActionExtract,
			normalizedActual,
		),
		extractResult(
			corpus,
			"extract:mixed",
			MatcherExtractActionExtract,
			mixedActual,
		),
		extractResult(
			corpus,
			"extract:no-fields",
			MatcherExtractActionExtract,
			noFieldsActual,
		),
	}

	report, err := Score(corpus, results, testEvaluatorBuildSHA)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	metrics := findTaskScore(t, report, TaskMatcherExtract).MatcherExtract
	if metrics.ExtractionAttempts != 3 ||
		metrics.FieldComparableAttempts != 2 ||
		metrics.CorrectExtractions.Count != 1 ||
		metrics.CorrectExtractions.Denominator != 3 ||
		metrics.ExactExtractions.Count != 0 ||
		metrics.IncorrectExtractions.Count != 2 {
		t.Fatalf("extraction metrics = %#v", metrics)
	}
	if metrics.Fields.TitleExact.Count != 1 ||
		metrics.Fields.TitleExact.Denominator != 2 ||
		metrics.Fields.TitleNormalized.Count != 2 ||
		metrics.Fields.Year.Count != 2 ||
		metrics.Fields.IsAnime.Count != 2 {
		t.Fatalf("field metrics = %#v", metrics.Fields)
	}
	if metrics.Fields.Type.Count != 2 ||
		metrics.Fields.Season.Count != 2 ||
		metrics.Fields.Episode.Count != 2 ||
		metrics.Fields.English.Count != 2 ||
		metrics.Fields.IsPack.Count != 2 ||
		metrics.Fields.IsAdult.Count != 2 {
		t.Fatalf("unchanged field metrics = %#v", metrics.Fields)
	}
	if report.Outcomes.Correct.Count != 1 {
		t.Fatalf("normalized extraction did not count toward primary correctness: %#v", report.Outcomes)
	}
}

func TestScoreTreatsGoldUncertaintyAsCorrectOnlyWhenAbstaining(t *testing.T) {
	contentAbstain := testContentRecord("content:abstain", LanguageEnglish)
	contentAbstain.ContentFilter.Expected = ContentFilterExpected{AllowAbstain: true}
	contentAction := testContentRecord("content:action", LanguageEnglish)
	contentAction.ContentFilter.Expected = ContentFilterExpected{AllowAbstain: true}
	junkAbstain := testJunkRecord("junk:abstain", JunkClassUnresolved)
	junkAction := testJunkRecord("junk:action", JunkClassUnresolved)
	corpus := mustCorpus(t, []CorpusRecord{
		contentAbstain,
		contentAction,
		junkAbstain,
		junkAction,
	})

	results := []ResultRecord{
		contentResult(
			corpus,
			contentAbstain.CaseID,
			ContentFilterActionAbstain,
			nil,
		),
		contentResult(
			corpus,
			contentAction.CaseID,
			ContentFilterActionKeep,
			boolPointer(true),
		),
		junkResult(
			corpus,
			junkAbstain.CaseID,
			JunkPurgeActionAbstain,
			JunkVerdictUnsure,
		),
		junkResult(
			corpus,
			junkAction.CaseID,
			JunkPurgeActionKeep,
			JunkVerdictRealMangled,
		),
	}
	report, err := Score(corpus, results, testEvaluatorBuildSHA)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if report.Outcomes.Correct.Count != 2 ||
		report.Outcomes.Abstentions.Count != 2 {
		t.Fatalf("uncertain outcomes = %#v", report.Outcomes)
	}
	content := findTaskScore(t, report, TaskContentFilter).ContentFilter
	if content.UncertainCases != 2 ||
		content.CorrectUncertainAbstains.Count != 1 ||
		content.ActionsOnUncertain.Count != 1 {
		t.Fatalf("content uncertainty metrics = %#v", content)
	}
	junk := findTaskScore(t, report, TaskJunkPurge).JunkPurge
	if junk.AbstainCases != 2 ||
		junk.CorrectAbstains.Count != 1 ||
		junk.ActionsOnAbstain.Count != 1 {
		t.Fatalf("junk abstain metrics = %#v", junk)
	}
}

// TestScoreJunkPurgeReportsConfusionAndMacroF1 scores the DISPOSITION axis.
//
// The v1 version of this test derived all its discriminating power from
// provenance: it asserted precision on the real_mangled class against a model
// verdict of real_mangled, which tested only that two provenance labels agreed.
// It could not have detected the defect this contract exists to remove. The
// fixture below is built so the two keep-disposition cases differ in CONTENT
// CLASS (movie, tv_show) and in outcome (kept, wrongly deleted), which is what
// makes false-junk observable.
func TestScoreJunkPurgeReportsConfusionAndMacroF1(t *testing.T) {
	records := []CorpusRecord{
		testJunkRecord("junk:degenerate", JunkClassDegenerate), // delete
		testJunkRecord("junk:movie", JunkClassMovie),           // keep
		testJunkRecord("junk:tvshow", JunkClassTVShow),         // keep
		testJunkRecord("junk:unresolved", JunkClassUnresolved), // abstain
	}
	corpus := mustCorpus(t, records)
	results := []ResultRecord{
		junkResult(corpus, "junk:degenerate", JunkPurgeActionJunk, JunkVerdictJunk),
		junkResult(corpus, "junk:movie", JunkPurgeActionKeep, JunkVerdictRealMangled),
		// The one harm in the fixture: a keep-disposition title deleted.
		junkResult(corpus, "junk:tvshow", JunkPurgeActionJunk, JunkVerdictJunk),
		junkResult(corpus, "junk:unresolved", JunkPurgeActionAbstain, JunkVerdictUnsure),
	}
	report, err := Score(corpus, results, testEvaluatorBuildSHA)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	metrics := findTaskScore(t, report, TaskJunkPurge).JunkPurge

	// Square 3x3 on the disposition axis, not 4x4 on provenance.
	if metrics.ScoredVerdicts != 4 || len(metrics.Confusion) != 9 {
		t.Fatalf("junk confusion shape = %#v", metrics)
	}
	// keep: P=1/1, R=1/2, F1=2/3. delete: P=1/2, R=1/1, F1=2/3. abstain: F1=1.
	if metrics.MacroF1Classes != 3 ||
		math.Abs(metrics.MacroF1-(2.0/3.0+2.0/3.0+1.0)/3.0) > 1e-12 {
		t.Fatalf("junk macro f1 = %#v", metrics)
	}

	// keep is the harm denominator and the only one.
	if metrics.KeepCases != 2 || metrics.DeleteCases != 1 ||
		metrics.AbstainCases != 1 {
		t.Fatalf("junk case counts = %#v", metrics)
	}
	if metrics.FalseJunk.Count != 1 || metrics.FalseJunk.Denominator != 2 {
		t.Fatalf("false junk = %#v", metrics.FalseJunk)
	}

	// PerClass is a secondary cut over content class, in derivation order,
	// carrying the action split and no precision/recall.
	if len(metrics.PerClass) != 4 {
		t.Fatalf("per-class rows = %#v", metrics.PerClass)
	}
	wantClasses := []JunkContentClass{
		JunkClassDegenerate, JunkClassMovie, JunkClassTVShow, JunkClassUnresolved,
	}
	for i, want := range wantClasses {
		if metrics.PerClass[i].ContentClass != want ||
			metrics.PerClass[i].Support != 1 {
			t.Fatalf("per-class[%d] = %#v, want %q", i, metrics.PerClass[i], want)
		}
	}
	if metrics.PerClass[1].Kept != 1 || metrics.PerClass[2].Deleted != 1 {
		t.Fatalf(
			"content-class action split lost the harm: movie=%#v tv_show=%#v",
			metrics.PerClass[1], metrics.PerClass[2],
		)
	}
}

func TestTitleMatchesNormalizedUsesProductionMatcherSemantics(t *testing.T) {
	tests := []struct {
		name     string
		actual   string
		expected string
		want     bool
	}{
		{
			name:     "leading article and roman numeral",
			actual:   "Movie Part 2",
			expected: "The Movie Part II",
			want:     true,
		},
		{
			name:     "diacritic and case",
			actual:   "AMELIE",
			expected: "Amélie",
			want:     true,
		},
		{
			name:     "different identity",
			actual:   "Dune",
			expected: "Dune Part Two",
			want:     false,
		},
		{
			name:     "empty never matches",
			actual:   "",
			expected: "",
			want:     false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := titleMatchesNormalized(test.actual, test.expected); got != test.want {
				t.Fatalf(
					"titleMatchesNormalized(%q, %q) = %v, want %v",
					test.actual,
					test.expected,
					got,
					test.want,
				)
			}
		})
	}
}

func TestScoreCountsMissingResultsAsRuntimeErrors(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:present", LanguageEnglish),
		testContentRecord("content:missing", LanguageEnglish),
	})
	present := contentResult(
		corpus,
		"content:present",
		ContentFilterActionKeep,
		boolPointer(true),
	)

	report, err := Score(corpus, []ResultRecord{present}, testEvaluatorBuildSHA)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if report.Outcomes.MissingResults != 1 || report.Outcomes.RuntimeErrors.Count != 1 {
		t.Fatalf("missing result outcomes = %#v", report.Outcomes)
	}
}

func TestScoreReportsNearestRankLatencyAtEveryLevel(t *testing.T) {
	records := make([]CorpusRecord, 0, 6)
	for i := 1; i <= 6; i++ {
		records = append(records, testContentRecord(
			fmt.Sprintf("content:latency-%d", i),
			LanguageEnglish,
		))
	}
	corpus := mustCorpus(t, records)
	values := []int64{1, 2, 3, 4, 100}
	results := make([]ResultRecord, 0, 6)
	for i, value := range values {
		result := contentResult(
			corpus,
			records[i].CaseID,
			ContentFilterActionKeep,
			boolPointer(true),
		)
		result.RequestTiming = &RequestTiming{
			ElapsedMS: value, DeadlineMS: 60_000,
		}
		results = append(results, result)
	}
	legacyTimeout := errorResult(
		corpus,
		records[5].CaseID,
		TaskContentFilter,
		ResultStatusError,
		"timeout",
	)
	results = append(results, legacyTimeout)

	report, err := Score(corpus, results, testEvaluatorBuildSHA)
	require.NoError(t, err)
	want := LatencySummary{
		Observed: 5, Missing: 1, TimeoutCount: 1, TimeoutRate: 1.0 / 6.0,
		P50MS: 3, P95MS: 100, P99MS: 100, MaxMS: 100, DeadlineMS: 60_000,
	}
	require.Equal(t, want, report.Latency)
	require.Len(t, report.Suites, 1)
	require.Equal(t, want, report.Suites[0].Latency)
	require.Len(t, report.Suites[0].Tasks, 1)
	require.Equal(t, want, report.Suites[0].Tasks[0].Latency)
	require.Equal(t, want, report.Tasks[0].Latency)
}

func TestScoreCountsDeadlineOverrunAsTimeoutEvidence(t *testing.T) {
	records := []CorpusRecord{
		testContentRecord("content:deadline-overrun", LanguageEnglish),
		testContentRecord("content:inside-deadline", LanguageEnglish),
	}
	corpus := mustCorpus(t, records)
	results := []ResultRecord{
		contentResult(
			corpus,
			records[0].CaseID,
			ContentFilterActionKeep,
			boolPointer(true),
		),
		contentResult(
			corpus,
			records[1].CaseID,
			ContentFilterActionKeep,
			boolPointer(true),
		),
	}
	results[0].RequestTiming = &RequestTiming{
		ElapsedMS: 8_001, DeadlineMS: 8_000,
	}
	results[1].RequestTiming = &RequestTiming{
		ElapsedMS: 8_000, DeadlineMS: 8_000,
	}

	report, err := Score(corpus, results, testEvaluatorBuildSHA)
	require.NoError(t, err)
	require.Equal(t, 2, report.Latency.Observed)
	require.Equal(t, 1, report.Latency.DeadlineExceededCount)
	require.Equal(t, 1, report.Latency.TimeoutCount)
	require.InDelta(t, 0.5, report.Latency.TimeoutRate, 1e-12)
	require.Equal(t, 1, report.Suites[0].Latency.DeadlineExceededCount)
	require.Equal(t, 1, report.Tasks[0].Latency.DeadlineExceededCount)
}

func TestNearestRankPercentile(t *testing.T) {
	values := make([]int64, 100)
	for i := range values {
		values[i] = int64(i + 1)
	}
	require.Equal(t, int64(50), nearestRankPercentile(values, 50))
	require.Equal(t, int64(95), nearestRankPercentile(values, 95))
	require.Equal(t, int64(99), nearestRankPercentile(values, 99))
}

func TestScoreRejectsInvalidOrInconsistentRequestTiming(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:one", LanguageEnglish),
		testContentRecord("content:two", LanguageEnglish),
	})
	base := []ResultRecord{
		contentResult(corpus, "content:one", ContentFilterActionKeep, boolPointer(true)),
		contentResult(corpus, "content:two", ContentFilterActionKeep, boolPointer(true)),
	}
	tests := []struct {
		name string
		one  RequestTiming
		two  *RequestTiming
		want string
	}{
		{name: "negative elapsed", one: RequestTiming{ElapsedMS: -1, DeadlineMS: 60_000}, want: "non-negative"},
		{name: "zero elapsed evidence", one: RequestTiming{DeadlineMS: 60_000}, want: "must be positive"},
		{name: "missing deadline", one: RequestTiming{ElapsedMS: 1}, want: "requires a positive deadline"},
		{name: "deadline too large", one: RequestTiming{ElapsedMS: 1, DeadlineMS: 600_001}, want: "exceeds maximum"},
		{name: "elapsed too large", one: RequestTiming{ElapsedMS: maxRequestElapsedMS + 1, DeadlineMS: 60_000}, want: "exceeds maximum"},
		{
			name: "inconsistent deadlines",
			one:  RequestTiming{ElapsedMS: 1, DeadlineMS: 60_000},
			two:  &RequestTiming{ElapsedMS: 1, DeadlineMS: 8_000},
			want: "inconsistent request deadlines",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := append([]ResultRecord(nil), base...)
			one := test.one
			results[0].RequestTiming = &one
			results[1].RequestTiming = test.two
			_, err := Score(corpus, results, testEvaluatorBuildSHA)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestScoreRejectsAttachmentOutsideFrozenCandidates(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testRerankRecord("rerank:invalid", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
	})
	result := rerankResult(corpus, "rerank:invalid", MatcherRerankActionAttach, 999)

	_, err := Score(corpus, []ResultRecord{result}, testEvaluatorBuildSHA)
	if err == nil || !strings.Contains(err.Error(), "not in the frozen candidate list") {
		t.Fatalf("Score error = %v", err)
	}
}

func TestScoreWithThresholdsUsesFrozenMatcherThreshold(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testRerankRecord("rerank:custom-threshold", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
	})
	result := rerankResult(
		corpus,
		"rerank:custom-threshold",
		MatcherRerankActionAttach,
		101,
	)
	result.MatcherRerank.Confidence = 0.40

	if _, err := Score(
		corpus,
		[]ResultRecord{result},
		testEvaluatorBuildSHA,
	); err == nil || !strings.Contains(err.Error(), "attach action requires at least") {
		t.Fatalf("production-threshold Score error = %v", err)
	}
	custom := ProductionThresholds()
	custom.MatcherAttachConfidence = 0.35
	if _, err := ScoreWithThresholds(
		corpus,
		[]ResultRecord{result},
		testEvaluatorBuildSHA,
		custom,
	); err != nil {
		t.Fatalf("ScoreWithThresholds: %v", err)
	}
}

func TestScoreRejectsInconsistentPackedUsage(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:one", LanguageEnglish),
		testContentRecord("content:two", LanguageEnglish),
	})
	one := contentResult(corpus, "content:one", ContentFilterActionKeep, boolPointer(true))
	two := contentResult(corpus, "content:two", ContentFilterActionKeep, boolPointer(true))
	one.Usage = Usage{RequestID: "request:shared", InputTokens: 10}
	two.Usage = Usage{RequestID: "request:shared", InputTokens: 11}

	_, err := Score(corpus, []ResultRecord{one, two}, testEvaluatorBuildSHA)
	if err == nil || !strings.Contains(err.Error(), "inconsistent usage") {
		t.Fatalf("Score error = %v", err)
	}
}

func TestScorePropagatesSemanticUsageAccountingCompleteness(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:complete", LanguageEnglish),
		testContentRecord("content:partial", LanguageEnglish),
	})
	complete := contentResult(
		corpus,
		"content:complete",
		ContentFilterActionKeep,
		boolPointer(true),
	)
	complete.Usage = Usage{
		RequestID:          "request:complete",
		InputTokens:        10,
		OutputTokens:       2,
		CostMicroUSD:       1,
		AccountingComplete: true,
	}
	partial := contentResult(
		corpus,
		"content:partial",
		ContentFilterActionKeep,
		boolPointer(true),
	)
	partial.Usage = Usage{
		RequestID:   "request:partial",
		InputTokens: 10,
		// A persisted true bit cannot make a generative request complete
		// without its positive output-token dimension.
		AccountingComplete: true,
	}

	report, err := Score(
		corpus,
		[]ResultRecord{complete, partial},
		testEvaluatorBuildSHA,
	)
	require.NoError(t, err)
	require.False(t, report.Usage.AccountingComplete)
	require.Equal(t, 1, report.Usage.IncompleteRequests)
	require.Len(t, report.Suites, 1)
	require.False(t, report.Suites[0].Usage.AccountingComplete)
	require.Equal(t, 1, report.Suites[0].Usage.IncompleteRequests)
	require.Len(t, report.Suites[0].Tasks, 1)
	require.False(t, report.Suites[0].Tasks[0].Usage.AccountingComplete)
	require.Equal(t, 1, report.Suites[0].Tasks[0].Usage.IncompleteRequests)
}

func TestScoreRejectsUsageIntegerOverflow(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:one", LanguageEnglish),
		testContentRecord("content:two", LanguageEnglish),
	})
	one := contentResult(corpus, "content:one", ContentFilterActionKeep, boolPointer(true))
	two := contentResult(corpus, "content:two", ContentFilterActionKeep, boolPointer(true))
	one.Usage = Usage{RequestID: "request:one", InputTokens: math.MaxInt64}
	two.Usage = Usage{RequestID: "request:two", InputTokens: 1}

	_, err := Score(corpus, []ResultRecord{one, two}, testEvaluatorBuildSHA)
	if err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("Score error = %v, want integer overflow", err)
	}
}

func TestScoreRejectsDifferentEvaluatorBuild(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:one", LanguageEnglish),
	})
	result := contentResult(
		corpus,
		"content:one",
		ContentFilterActionKeep,
		boolPointer(true),
	)

	_, err := Score(
		corpus,
		[]ResultRecord{result},
		"2222222222222222222222222222222222222222222222222222222222222222",
	)
	if err == nil || !strings.Contains(err.Error(), "different evaluator build") {
		t.Fatalf("Score error = %v, want evaluator build mismatch", err)
	}
}

func TestWilson95(t *testing.T) {
	zeroOfThreeThousand := Wilson95(0, 3000)
	if zeroOfThreeThousand.Lower != 0 ||
		zeroOfThreeThousand.Upper < 0.0012 ||
		zeroOfThreeThousand.Upper > 0.0014 {
		t.Fatalf("Wilson95(0,3000) = %#v", zeroOfThreeThousand)
	}

	half := Wilson95(5, 10)
	if math.Abs(half.Lower-0.2366) > 0.001 || math.Abs(half.Upper-0.7634) > 0.001 {
		t.Fatalf("Wilson95(5,10) = %#v", half)
	}

	empty := Wilson95(0, 0)
	if empty.Lower != 0 || empty.Upper != 1 {
		t.Fatalf("Wilson95(0,0) = %#v", empty)
	}
}

func extractResult(
	corpus Corpus,
	id string,
	action MatcherExtractAction,
	extraction MatcherExtraction,
) ResultRecord {
	result := testResult(corpus, id, TaskMatcherExtract)
	result.MatcherExtract = &MatcherExtractResult{Action: action}
	if action == MatcherExtractActionExtract {
		result.MatcherExtract.Extraction = &extraction
	}

	return result
}

func rerankResult(
	corpus Corpus,
	id string,
	action MatcherRerankAction,
	tmdbID int64,
) ResultRecord {
	result := testResult(corpus, id, TaskMatcherRerank)
	result.MatcherRerank = &MatcherRerankResult{
		Action:     action,
		TMDBID:     tmdbID,
		Confidence: 0.9,
	}

	return result
}

func contentResult(
	corpus Corpus,
	id string,
	action ContentFilterAction,
	isEnglish *bool,
) ResultRecord {
	result := testResult(corpus, id, TaskContentFilter)
	confidence := 0.9
	if action == ContentFilterActionAbstain {
		confidence = 0.5
	}
	result.ContentFilter = &ContentFilterResult{
		Action:     action,
		IsEnglish:  isEnglish,
		Confidence: confidence,
		ReasonTag:  "fixture",
	}

	return result
}

func junkResult(
	corpus Corpus,
	id string,
	action JunkPurgeAction,
	verdict JunkVerdict,
) ResultRecord {
	result := testResult(corpus, id, TaskJunkPurge)
	result.JunkPurge = &JunkPurgeResult{
		Action:     action,
		Verdict:    verdict,
		Confidence: 0.9,
	}

	return result
}

func errorResult(
	corpus Corpus,
	id string,
	task Task,
	status ResultStatus,
	errorCode string,
) ResultRecord {
	result := testResult(corpus, id, task)
	result.Status = status
	result.ErrorCode = errorCode

	return result
}

func findTaskScore(t *testing.T, report ScoreReport, task Task) TaskScore {
	t.Helper()
	for _, score := range report.Tasks {
		if score.Task == task {
			return score
		}
	}
	t.Fatalf("task score %q not found", task)

	return TaskScore{}
}
