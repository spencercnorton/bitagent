package llmeval

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/spencercnorton/bitagent/internal/titlenorm"
)

const wilson95Z = 1.959963984540054

// WilsonInterval is a two-sided 95% Wilson score interval.
type WilsonInterval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// RateMetric always carries its numerator and denominator so reports cannot
// silently compare rates computed over different safety populations.
type RateMetric struct {
	Count       int            `json:"count"`
	Denominator int            `json:"denominator"`
	Rate        float64        `json:"rate"`
	Wilson95    WilsonInterval `json:"wilson_95"`
}

// OutcomeMetrics contains the common, safety-relevant result states. Correct
// may overlap Abstentions when abstention is explicitly correct for a matcher
// case; each metric therefore retains its own denominator and interval.
type OutcomeMetrics struct {
	Cases          int        `json:"cases"`
	UniqueGroups   int        `json:"unique_groups"`
	Correct        RateMetric `json:"correct"`
	SchemaErrors   RateMetric `json:"schema_errors"`
	RuntimeErrors  RateMetric `json:"runtime_errors"`
	Abstentions    RateMetric `json:"abstentions"`
	MissingResults int        `json:"missing_results"`
}

// UsageTotals deduplicates packed outputs by Usage.RequestID.
type UsageTotals struct {
	Results            int     `json:"results"`
	Requests           int     `json:"requests"`
	InputTokens        int64   `json:"input_tokens"`
	CachedInputTokens  int64   `json:"cached_input_tokens"`
	CacheWriteTokens   int64   `json:"cache_write_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	CostMicroUSD       int64   `json:"cost_micro_usd"`
	CostUSD            float64 `json:"cost_usd"`
	AccountingComplete bool    `json:"accounting_complete"`
	IncompleteRequests int     `json:"incomplete_requests"`
}

// LatencySummary reports synchronous request timing evidence. Observed and
// Missing count result rows with and without timing evidence respectively;
// absent corpus results remain tracked by OutcomeMetrics.MissingResults.
// TimeoutRate uses all returned result rows (Observed + Missing) so readable
// legacy timeout results remain part of the reliability denominator.
type LatencySummary struct {
	Observed     int `json:"observed"`
	Missing      int `json:"missing"`
	TimeoutCount int `json:"timeout_count"`
	// DeadlineExceededCount is a subset of TimeoutCount derived from immutable
	// timing rather than the provider error tag. It prevents a late successful
	// envelope from silently escaping timeout accounting.
	DeadlineExceededCount int     `json:"deadline_exceeded_count"`
	TimeoutRate           float64 `json:"timeout_rate"`
	P50MS                 int64   `json:"p50_ms"`
	P95MS                 int64   `json:"p95_ms"`
	P99MS                 int64   `json:"p99_ms"`
	MaxMS                 int64   `json:"max_ms"`
	DeadlineMS            int64   `json:"deadline_ms"`
}

type MatcherExtractMetrics struct {
	ExtractionAttempts int `json:"extraction_attempts"`
	// CorrectExtractions is the production-aligned primary metric: title uses
	// the matcher normalizer and every other field must match the same gold
	// tuple. ExactExtractions is the stricter all-fields diagnostic.
	CorrectExtractions      RateMetric                 `json:"correct_extractions"`
	ExactExtractions        RateMetric                 `json:"exact_extractions"`
	IncorrectExtractions    RateMetric                 `json:"incorrect_extractions"`
	RequiredExtractions     int                        `json:"required_extractions"`
	MissedExtractions       RateMetric                 `json:"missed_extractions"`
	AbstainAllowedCases     int                        `json:"abstain_allowed_cases"`
	CorrectAbstentions      RateMetric                 `json:"correct_abstentions"`
	FieldComparableAttempts int                        `json:"field_comparable_attempts"`
	Fields                  MatcherExtractFieldMetrics `json:"fields"`
}

// MatcherExtractFieldMetrics provides diagnostics below the all-fields
// extraction score. Every field counts as matching when it equals that field
// in at least one explicitly acceptable output. NormalizedTitle uses the same
// canonical title normalizer as the production matcher. The denominator
// excludes abstentions, errors, and abstain-only cases that have no gold fields.
type MatcherExtractFieldMetrics struct {
	TitleExact      RateMetric `json:"title_exact"`
	TitleNormalized RateMetric `json:"title_normalized"`
	Year            RateMetric `json:"year"`
	Type            RateMetric `json:"type"`
	Season          RateMetric `json:"season"`
	Episode         RateMetric `json:"episode"`
	IsAnime         RateMetric `json:"is_anime"`
	English         RateMetric `json:"english"`
	IsPack          RateMetric `json:"is_pack"`
	IsAdult         RateMetric `json:"is_adult"`
}

type MatcherRerankMetrics struct {
	AttachmentAttempts  int        `json:"attachment_attempts"`
	CorrectAttachments  RateMetric `json:"correct_attachments"`
	WrongAttachments    RateMetric `json:"wrong_attachments"`
	RequiredAttachments int        `json:"required_attachments"`
	MissedByAbstention  RateMetric `json:"missed_by_abstention"`
	AbstainAllowedCases int        `json:"abstain_allowed_cases"`
	CorrectAbstentions  RateMetric `json:"correct_abstentions"`
}

type ContentFilterMetrics struct {
	EnglishCases             int        `json:"english_cases"`
	NonEnglishCases          int        `json:"non_english_cases"`
	UncertainCases           int        `json:"uncertain_cases"`
	CorrectKeeps             RateMetric `json:"correct_keeps"`
	FalseDrops               RateMetric `json:"false_drops"`
	CorrectDrops             RateMetric `json:"correct_drops"`
	MissedNonEnglish         RateMetric `json:"missed_non_english"`
	CorrectUncertainAbstains RateMetric `json:"correct_uncertain_abstains"`
	ActionsOnUncertain       RateMetric `json:"actions_on_uncertain"`
}

// JunkPurgeMetrics scores the DISPOSITION axis. KeepCases is the harm
// denominator and the only one: false-junk is a rate over content policy
// intends to retain.
type JunkPurgeMetrics struct {
	// Tier is the single gold tier these metrics were computed over. Tiers are
	// never blended — Tier A is the easy end of the distribution by
	// construction and would mask Tier B, which is where false-junk lives.
	Tier             GoldTier            `json:"tier,omitempty"`
	KeepCases        int                 `json:"keep_cases"`
	DeleteCases      int                 `json:"delete_cases"`
	AbstainCases     int                 `json:"abstain_cases"`
	CorrectKeeps     RateMetric          `json:"correct_keeps"`
	FalseJunk        RateMetric          `json:"false_junk"`
	CorrectJunk      RateMetric          `json:"correct_junk"`
	MissedJunk       RateMetric          `json:"missed_junk"`
	CorrectAbstains  RateMetric          `json:"correct_abstains"`
	ActionsOnAbstain RateMetric          `json:"actions_on_abstain"`
	ScoredVerdicts   int                 `json:"scored_verdicts"`
	Confusion        []JunkConfusionCell `json:"confusion"`
	// PerClass is a SECONDARY cut over ContentClass. It is never the scored
	// axis; crossing it with the disposition axis is what collapses MacroF1.
	PerClass       []JunkClassMetrics `json:"per_class"`
	MacroF1        float64            `json:"macro_f1"`
	MacroF1Classes int                `json:"macro_f1_classes"`
}

// JunkConfusionCell is one cell of the square 3x3 disposition matrix: expected
// policy disposition against the model's action projected onto the same axis.
type JunkConfusionCell struct {
	Expected  JunkDisposition `json:"expected"`
	Predicted JunkDisposition `json:"predicted"`
	Count     int             `json:"count"`
}

// JunkClassMetrics breaks results down by the durable content class. It carries
// support and the action split, not precision/recall/F1 — a model emits an
// action, never a content class, so there is no predicted class to score
// against and any "precision" here would be a category error.
type JunkClassMetrics struct {
	ContentClass JunkContentClass `json:"content_class"`
	Support      int              `json:"support"`
	Kept         int              `json:"kept"`
	Deleted      int              `json:"deleted"`
	Abstained    int              `json:"abstained"`
}

// TaskScore has exactly one task-specific metrics pointer.
type TaskScore struct {
	Task           Task                   `json:"task"`
	Outcomes       OutcomeMetrics         `json:"outcomes"`
	Usage          UsageTotals            `json:"usage"`
	Latency        LatencySummary         `json:"latency"`
	MatcherExtract *MatcherExtractMetrics `json:"matcher_extract,omitempty"`
	MatcherRerank  *MatcherRerankMetrics  `json:"matcher_rerank,omitempty"`
	ContentFilter  *ContentFilterMetrics  `json:"contentfilter,omitempty"`
	JunkPurge      *JunkPurgeMetrics      `json:"junkpurge,omitempty"`
}

// SuiteScore keeps representative and deliberately over-sampled traffic in
// separate quality populations. Usage is attributable to this suite only.
type SuiteScore struct {
	Suite    string         `json:"suite"`
	Outcomes OutcomeMetrics `json:"outcomes"`
	Usage    UsageTotals    `json:"usage"`
	Latency  LatencySummary `json:"latency"`
	Tasks    []TaskScore    `json:"tasks"`
}

// ScoreReport is one system's deterministic score over one immutable corpus.
// Usage is the deduplicated execution total across suites. Outcomes and Tasks
// are compatibility aliases populated only when the corpus has exactly one
// suite, so production natural and safety quality can never be blended.
type ScoreReport struct {
	CorpusSHA256         string              `json:"corpus_sha256"`
	EvaluatorBuildSHA256 string              `json:"evaluator_build_sha256"`
	System               SystemDescriptor    `json:"system"`
	Campaign             *CampaignRunBinding `json:"campaign,omitempty"`
	QualityAggregation   string              `json:"quality_aggregation"`
	Outcomes             *OutcomeMetrics     `json:"outcomes,omitempty"`
	Usage                UsageTotals         `json:"usage"`
	Latency              LatencySummary      `json:"latency"`
	Tasks                []TaskScore         `json:"tasks,omitempty"`
	Suites               []SuiteScore        `json:"suites"`
}

type taskAccumulator struct {
	task           Task
	cases          int
	groups         map[string]struct{}
	correct        int
	schemaErrors   int
	runtimeErrors  int
	abstentions    int
	missingResults int
	usage          UsageTotals
	latency        latencyAccumulator

	extractAttempts          int
	extractCorrect           int
	extractExact             int
	extractIncorrect         int
	extractRequired          int
	extractMissed            int
	extractAbstainAllowed    int
	extractCorrectAbstention int
	extractFieldComparable   int
	extractTitleExact        int
	extractTitleNormalized   int
	extractYear              int
	extractType              int
	extractSeason            int
	extractEpisode           int
	extractIsAnime           int
	extractEnglish           int
	extractIsPack            int
	extractIsAdult           int

	rerankAttempts          int
	rerankCorrect           int
	rerankWrong             int
	rerankRequired          int
	rerankMissedAbstention  int
	rerankAbstainAllowed    int
	rerankCorrectAbstention int

	contentEnglish          int
	contentNonEnglish       int
	contentCorrectKeep      int
	contentFalseDrop        int
	contentCorrectDrop      int
	contentMissedNonEnglish int
	contentUncertain        int
	contentCorrectAbstain   int
	contentActionUncertain  int

	junkKeep            int
	junkDelete          int
	junkAbstainExpected int
	junkCorrectKeep     int
	junkFalseJunk       int
	junkCorrectJunk     int
	junkMissedJunk      int
	junkCorrectAbstain  int
	junkActionOnAbstain int
	junkConfusion       map[junkConfusionKey]int
	junkClassSplit      map[JunkContentClass]*junkClassSplit
	junkTier            GoldTier
}

type junkConfusionKey struct {
	expected  JunkDisposition
	predicted JunkDisposition
}

type junkClassSplit struct {
	support   int
	kept      int
	deleted   int
	abstained int
}

type requestUsage struct {
	task  Task
	suite string
	usage Usage
}

type suiteScoreAccumulator struct {
	suite        string
	taskAccs     map[Task]*taskAccumulator
	groups       map[string]struct{}
	usage        UsageTotals
	latency      latencyAccumulator
	seenRequests map[string]requestUsage
}

type latencyAccumulator struct {
	elapsedMS        []int64
	missing          int
	timeouts         int
	deadlineExceeded int
	deadline         int64
}

// Score validates and scores one system's result set. Missing results are
// counted as runtime errors; unknown cases, mixed systems, mismatched corpus
// identities, duplicate results, and impossible normalized outputs fail.
func Score(
	corpus Corpus,
	results []ResultRecord,
	evaluatorBuildSHA256 string,
) (ScoreReport, error) {
	return ScoreWithThresholds(
		corpus,
		results,
		evaluatorBuildSHA256,
		ProductionThresholds(),
	)
}

// ScoreWithThresholds scores actions only after proving they are reachable
// under the exact decision thresholds bound to the evaluated run.
func ScoreWithThresholds(
	corpus Corpus,
	results []ResultRecord,
	evaluatorBuildSHA256 string,
	thresholds DecisionThresholds,
) (ScoreReport, error) {
	if err := validateSHA256(
		"evaluator_build_sha256",
		evaluatorBuildSHA256,
	); err != nil {
		return ScoreReport{}, err
	}
	canonical, err := NewCorpus(corpus.Records)
	if err != nil {
		return ScoreReport{}, fmt.Errorf("corpus: %w", err)
	}
	if corpus.SHA256 != "" && corpus.SHA256 != canonical.SHA256 {
		return ScoreReport{}, fmt.Errorf(
			"corpus SHA-256 mismatch: provided %s, computed %s",
			corpus.SHA256,
			canonical.SHA256,
		)
	}
	if err := ValidateGoldCorpus(canonical); err != nil {
		return ScoreReport{}, err
	}
	if err := rejectCrossTierJunkPurge(canonical.Records); err != nil {
		return ScoreReport{}, err
	}
	if len(results) == 0 {
		return ScoreReport{}, fmt.Errorf("results are empty")
	}
	if err := ValidateResultsWithThresholds(results, thresholds); err != nil {
		return ScoreReport{}, err
	}
	for i, result := range results {
		if err := validateResultRequestTiming(result); err != nil {
			return ScoreReport{}, fmt.Errorf(
				"result %d request timing: %w",
				i+1,
				err,
			)
		}
	}

	system := results[0].System
	caseByID := make(map[string]CorpusRecord, len(canonical.Records))
	suiteByCase := make(map[string]string, len(canonical.Records))
	suiteAccs := make(map[string]*suiteScoreAccumulator)
	for _, record := range canonical.Records {
		caseByID[record.CaseID] = record
		suite, err := scoringPrimarySuite(record)
		if err != nil {
			return ScoreReport{}, fmt.Errorf("case %q: %w", record.CaseID, err)
		}
		suiteByCase[record.CaseID] = suite
		suiteAcc := suiteAccs[suite]
		if suiteAcc == nil {
			suiteAcc = &suiteScoreAccumulator{
				suite:        suite,
				taskAccs:     make(map[Task]*taskAccumulator),
				groups:       make(map[string]struct{}),
				seenRequests: make(map[string]requestUsage),
			}
			suiteAccs[suite] = suiteAcc
		}
		acc := suiteAcc.taskAccs[record.Task]
		if acc == nil {
			acc = &taskAccumulator{task: record.Task, groups: make(map[string]struct{})}
			suiteAcc.taskAccs[record.Task] = acc
		}
		acc.cases++
		acc.groups[record.GroupID] = struct{}{}
		suiteAcc.groups[record.GroupID] = struct{}{}
		acc.observeGold(record)
	}

	resultByCase := make(map[string]ResultRecord, len(results))
	for i, result := range results {
		if result.EvaluatorBuildSHA256 != evaluatorBuildSHA256 {
			return ScoreReport{}, fmt.Errorf(
				"result %d case %q belongs to a different evaluator build",
				i+1,
				result.CaseID,
			)
		}
		if result.System != system {
			return ScoreReport{}, fmt.Errorf(
				"result %d: mixed systems %q and %q",
				i+1,
				system.SystemID,
				result.System.SystemID,
			)
		}
		if result.CorpusSHA256 != canonical.SHA256 {
			return ScoreReport{}, fmt.Errorf(
				"result %d case %q: corpus_sha256 %s does not match %s",
				i+1,
				result.CaseID,
				result.CorpusSHA256,
				canonical.SHA256,
			)
		}
		record, exists := caseByID[result.CaseID]
		if !exists {
			return ScoreReport{}, fmt.Errorf("result %d: unknown case_id %q", i+1, result.CaseID)
		}
		if result.Task != record.Task {
			return ScoreReport{}, fmt.Errorf(
				"result %d case %q: task %q does not match corpus task %q",
				i+1,
				result.CaseID,
				result.Task,
				record.Task,
			)
		}
		if err := validateResultForCase(record, result); err != nil {
			return ScoreReport{}, fmt.Errorf("result %d case %q: %w", i+1, result.CaseID, err)
		}
		resultByCase[result.CaseID] = result
	}

	var (
		overallUsage   UsageTotals
		overallLatency latencyAccumulator
		seenRequests   = make(map[string]requestUsage)
	)
	for _, record := range canonical.Records {
		suite := suiteByCase[record.CaseID]
		suiteAcc := suiteAccs[suite]
		acc := suiteAcc.taskAccs[record.Task]
		result, exists := resultByCase[record.CaseID]
		if !exists {
			acc.runtimeErrors++
			acc.missingResults++
			continue
		}
		if err := acc.latency.observe(result); err != nil {
			return ScoreReport{}, fmt.Errorf(
				"case %q task latency: %w",
				record.CaseID,
				err,
			)
		}
		if err := suiteAcc.latency.observe(result); err != nil {
			return ScoreReport{}, fmt.Errorf(
				"case %q suite latency: %w",
				record.CaseID,
				err,
			)
		}
		if err := overallLatency.observe(result); err != nil {
			return ScoreReport{}, fmt.Errorf(
				"case %q overall latency: %w",
				record.CaseID,
				err,
			)
		}

		acc.usage.Results++
		suiteAcc.usage.Results++
		overallUsage.Results++
		firstRequest, err := accountUsage(
			seenRequests,
			system.SystemID,
			record.Task,
			suite,
			record.CaseID,
			result.Usage,
		)
		if err != nil {
			return ScoreReport{}, err
		}
		if firstRequest {
			accountingComplete := scoreUsageAccountingComplete(result)
			if err := acc.usage.addRequest(result.Usage, accountingComplete); err != nil {
				return ScoreReport{}, fmt.Errorf(
					"case %q task usage: %w",
					record.CaseID,
					err,
				)
			}
			if err := overallUsage.addRequest(result.Usage, accountingComplete); err != nil {
				return ScoreReport{}, fmt.Errorf(
					"case %q overall usage: %w",
					record.CaseID,
					err,
				)
			}
			suiteFirstRequest, err := accountUsage(
				suiteAcc.seenRequests,
				system.SystemID,
				record.Task,
				suite,
				record.CaseID,
				result.Usage,
			)
			if err != nil {
				return ScoreReport{}, err
			}
			if !suiteFirstRequest {
				return ScoreReport{}, fmt.Errorf(
					"case %q: internal suite usage accounting mismatch",
					record.CaseID,
				)
			}
			if err := suiteAcc.usage.addRequest(result.Usage, accountingComplete); err != nil {
				return ScoreReport{}, fmt.Errorf(
					"case %q suite usage: %w",
					record.CaseID,
					err,
				)
			}
		}

		switch result.Status {
		case ResultStatusSchemaError:
			acc.schemaErrors++
			continue
		case ResultStatusError:
			acc.runtimeErrors++
			continue
		case ResultStatusOK:
			acc.scoreOK(record, result)
		default:
			return ScoreReport{}, fmt.Errorf(
				"case %q: unsupported status %q after validation",
				record.CaseID,
				result.Status,
			)
		}
	}

	suiteNames := make([]string, 0, len(suiteAccs))
	for suite := range suiteAccs {
		suiteNames = append(suiteNames, suite)
	}
	sort.Strings(suiteNames)
	report := ScoreReport{
		CorpusSHA256:         canonical.SHA256,
		EvaluatorBuildSHA256: evaluatorBuildSHA256,
		System:               system,
		Campaign:             cloneCampaignRunBinding(results[0].ExecutionAudit.Campaign),
		QualityAggregation:   "suite_separated",
		Usage:                overallUsage,
		Latency:              overallLatency.finalize(),
	}
	for _, suite := range suiteNames {
		report.Suites = append(report.Suites, suiteAccs[suite].finalize())
	}
	if len(report.Suites) == 1 {
		outcomes := report.Suites[0].Outcomes
		report.Outcomes = &outcomes
		report.Tasks = append([]TaskScore(nil), report.Suites[0].Tasks...)
	}
	return report, nil
}

func scoringPrimarySuite(record CorpusRecord) (string, error) {
	var suites []string
	for _, sliceID := range record.SliceIDs {
		if strings.HasPrefix(sliceID, CorpusPrimarySuiteSlicePrefix) {
			suite := strings.TrimPrefix(sliceID, CorpusPrimarySuiteSlicePrefix)
			if suite == "" {
				return "", fmt.Errorf("empty primary suite tag")
			}
			suites = append(suites, suite)
		}
	}
	if len(suites) == 0 && record.Label.Provenance == LabelProvenanceSynthetic {
		return "synthetic_unstratified", nil
	}
	if len(suites) != 1 {
		return "", fmt.Errorf(
			"expected exactly one %q primary suite tag, got %d",
			CorpusPrimarySuiteSlicePrefix,
			len(suites),
		)
	}
	return suites[0], nil
}

func (s *suiteScoreAccumulator) finalize() SuiteScore {
	var (
		tasks          []TaskScore
		correct        int
		schemaErrors   int
		runtimeErrors  int
		abstentions    int
		missingResults int
		cases          int
	)
	for _, task := range orderedTasks {
		acc := s.taskAccs[task]
		if acc == nil {
			continue
		}
		tasks = append(tasks, acc.finalize())
		cases += acc.cases
		correct += acc.correct
		schemaErrors += acc.schemaErrors
		runtimeErrors += acc.runtimeErrors
		abstentions += acc.abstentions
		missingResults += acc.missingResults
	}
	return SuiteScore{
		Suite: s.suite,
		Outcomes: OutcomeMetrics{
			Cases:          cases,
			UniqueGroups:   len(s.groups),
			Correct:        newRateMetric(correct, cases),
			SchemaErrors:   newRateMetric(schemaErrors, cases),
			RuntimeErrors:  newRateMetric(runtimeErrors, cases),
			Abstentions:    newRateMetric(abstentions, cases),
			MissingResults: missingResults,
		},
		Usage:   s.usage,
		Latency: s.latency.finalize(),
		Tasks:   tasks,
	}
}

func (a *taskAccumulator) observeGold(record CorpusRecord) {
	switch record.Task {
	case TaskMatcherExtract:
		expected := record.MatcherExtract.Expected
		if len(expected.Acceptable) > 0 && !expected.AllowAbstain {
			a.extractRequired++
		}
		if expected.AllowAbstain {
			a.extractAbstainAllowed++
		}
	case TaskMatcherRerank:
		expected := record.MatcherRerank.Expected
		if len(expected.AcceptableTMDBIDs) > 0 && !expected.AllowAbstain {
			a.rerankRequired++
		}
		if expected.AllowAbstain {
			a.rerankAbstainAllowed++
		}
	case TaskContentFilter:
		if record.ContentFilter.Expected.AllowAbstain {
			a.contentUncertain++
		} else if record.ContentFilter.Expected.Language == LanguageEnglish {
			a.contentEnglish++
		} else {
			a.contentNonEnglish++
		}
	case TaskJunkPurge:
		// Explicit and exhaustive, with no default arm. A default here is how a
		// class silently lands in the harm denominator: the old code counted
		// "everything that is not junk or unsure" as real, so any new expected
		// value would have become harm-eligible by omission.
		switch record.JunkPurge.Expected.Disposition {
		case JunkDispositionKeep:
			a.junkKeep++
		case JunkDispositionDelete:
			a.junkDelete++
		case JunkDispositionAbstain:
			a.junkAbstainExpected++
		}
		if a.junkTier == "" {
			a.junkTier = record.Tier
		}
	}
}

func (a *taskAccumulator) scoreOK(record CorpusRecord, result ResultRecord) {
	switch record.Task {
	case TaskMatcherExtract:
		a.scoreMatcherExtract(record.MatcherExtract, result.MatcherExtract)
	case TaskMatcherRerank:
		a.scoreMatcherRerank(record.MatcherRerank, result.MatcherRerank)
	case TaskContentFilter:
		a.scoreContentFilter(record.ContentFilter, result.ContentFilter)
	case TaskJunkPurge:
		a.scoreJunkPurge(record.JunkPurge, result.JunkPurge)
	}
}

func (a *taskAccumulator) scoreMatcherExtract(
	corpus *MatcherExtractCase,
	result *MatcherExtractResult,
) {
	if result.Action == MatcherExtractActionAbstain {
		a.abstentions++
		if corpus.Expected.AllowAbstain {
			a.correct++
			a.extractCorrectAbstention++
		} else {
			a.extractMissed++
		}
		return
	}

	a.extractAttempts++
	extraction := *result.Extraction
	exactMatch := false
	normalizedMatch := false
	for _, acceptable := range corpus.Expected.Acceptable {
		if extractionMatchesNormalized(extraction, acceptable) {
			normalizedMatch = true
		}
		if extraction == acceptable {
			exactMatch = true
		}
	}
	if normalizedMatch {
		a.correct++
		a.extractCorrect++
	}
	if exactMatch {
		a.extractExact++
	}
	if !normalizedMatch {
		a.extractIncorrect++
	}
	a.observeExtractionFields(extraction, corpus.Expected.Acceptable)
}

func (a *taskAccumulator) scoreMatcherRerank(
	corpus *MatcherRerankCase,
	result *MatcherRerankResult,
) {
	if result.Action == MatcherRerankActionAbstain {
		a.abstentions++
		if corpus.Expected.AllowAbstain {
			a.correct++
			a.rerankCorrectAbstention++
		} else {
			a.rerankMissedAbstention++
		}
		return
	}

	a.rerankAttempts++
	for _, acceptable := range corpus.Expected.AcceptableTMDBIDs {
		if result.TMDBID == acceptable {
			a.correct++
			a.rerankCorrect++
			return
		}
	}
	a.rerankWrong++
}

func (a *taskAccumulator) scoreContentFilter(
	corpus *ContentFilterCase,
	result *ContentFilterResult,
) {
	if corpus.Expected.AllowAbstain {
		if result.Action == ContentFilterActionAbstain {
			a.abstentions++
			a.correct++
			a.contentCorrectAbstain++
		} else {
			a.contentActionUncertain++
		}
		return
	}
	if result.Action == ContentFilterActionAbstain {
		a.abstentions++
		return
	}

	isEnglish := corpus.Expected.Language == LanguageEnglish
	switch {
	case result.Action == ContentFilterActionDrop && isEnglish:
		a.contentFalseDrop++
	case result.Action == ContentFilterActionDrop && !isEnglish:
		a.correct++
		a.contentCorrectDrop++
	case result.Action == ContentFilterActionKeep && isEnglish:
		a.correct++
		a.contentCorrectKeep++
	case result.Action == ContentFilterActionKeep && !isEnglish:
		a.contentMissedNonEnglish++
	}
}

func (a *taskAccumulator) scoreJunkPurge(
	corpus *JunkPurgeCase,
	result *JunkPurgeResult,
) {
	if a.junkConfusion == nil {
		a.junkConfusion = make(map[junkConfusionKey]int)
	}
	if a.junkClassSplit == nil {
		a.junkClassSplit = make(map[JunkContentClass]*junkClassSplit)
	}
	// Both axes are dispositions. The model emits an ACTION, which is projected
	// onto the disposition axis (junk -> delete). Scoring the model's verdict
	// against the expected label would be scoring provenance again.
	predicted := dispositionOfAction(result.Action)
	a.junkConfusion[junkConfusionKey{
		expected:  corpus.Expected.Disposition,
		predicted: predicted,
	}]++

	split := a.junkClassSplit[corpus.Expected.ContentClass]
	if split == nil {
		split = &junkClassSplit{}
		a.junkClassSplit[corpus.Expected.ContentClass] = split
	}
	split.support++
	switch predicted {
	case JunkDispositionKeep:
		split.kept++
	case JunkDispositionDelete:
		split.deleted++
	case JunkDispositionAbstain:
		split.abstained++
	}

	if corpus.Expected.Disposition == JunkDispositionAbstain {
		if result.Action == JunkPurgeActionAbstain {
			a.abstentions++
			a.correct++
			a.junkCorrectAbstain++
		} else {
			a.junkActionOnAbstain++
		}
		return
	}
	if result.Action == JunkPurgeActionAbstain {
		// A model abstention on a decidable case is neither harm nor success.
		// It must stay out of both, or abstaining everywhere would read as a
		// perfect safety record.
		a.abstentions++
		return
	}

	isDelete := corpus.Expected.Disposition == JunkDispositionDelete
	switch {
	case result.Action == JunkPurgeActionJunk && !isDelete:
		a.junkFalseJunk++
	case result.Action == JunkPurgeActionJunk && isDelete:
		a.correct++
		a.junkCorrectJunk++
	case result.Action == JunkPurgeActionKeep && !isDelete:
		a.correct++
		a.junkCorrectKeep++
	case result.Action == JunkPurgeActionKeep && isDelete:
		a.junkMissedJunk++
	}
}

func (a *taskAccumulator) finalize() TaskScore {
	score := TaskScore{
		Task: a.task,
		Outcomes: OutcomeMetrics{
			Cases:          a.cases,
			UniqueGroups:   len(a.groups),
			Correct:        newRateMetric(a.correct, a.cases),
			SchemaErrors:   newRateMetric(a.schemaErrors, a.cases),
			RuntimeErrors:  newRateMetric(a.runtimeErrors, a.cases),
			Abstentions:    newRateMetric(a.abstentions, a.cases),
			MissingResults: a.missingResults,
		},
		Usage:   a.usage,
		Latency: a.latency.finalize(),
	}

	switch a.task {
	case TaskMatcherExtract:
		score.MatcherExtract = &MatcherExtractMetrics{
			ExtractionAttempts:      a.extractAttempts,
			CorrectExtractions:      newRateMetric(a.extractCorrect, a.extractAttempts),
			ExactExtractions:        newRateMetric(a.extractExact, a.extractAttempts),
			IncorrectExtractions:    newRateMetric(a.extractIncorrect, a.extractAttempts),
			RequiredExtractions:     a.extractRequired,
			MissedExtractions:       newRateMetric(a.extractMissed, a.extractRequired),
			AbstainAllowedCases:     a.extractAbstainAllowed,
			CorrectAbstentions:      newRateMetric(a.extractCorrectAbstention, a.extractAbstainAllowed),
			FieldComparableAttempts: a.extractFieldComparable,
			Fields: MatcherExtractFieldMetrics{
				TitleExact:      newRateMetric(a.extractTitleExact, a.extractFieldComparable),
				TitleNormalized: newRateMetric(a.extractTitleNormalized, a.extractFieldComparable),
				Year:            newRateMetric(a.extractYear, a.extractFieldComparable),
				Type:            newRateMetric(a.extractType, a.extractFieldComparable),
				Season:          newRateMetric(a.extractSeason, a.extractFieldComparable),
				Episode:         newRateMetric(a.extractEpisode, a.extractFieldComparable),
				IsAnime:         newRateMetric(a.extractIsAnime, a.extractFieldComparable),
				English:         newRateMetric(a.extractEnglish, a.extractFieldComparable),
				IsPack:          newRateMetric(a.extractIsPack, a.extractFieldComparable),
				IsAdult:         newRateMetric(a.extractIsAdult, a.extractFieldComparable),
			},
		}
	case TaskMatcherRerank:
		score.MatcherRerank = &MatcherRerankMetrics{
			AttachmentAttempts:  a.rerankAttempts,
			CorrectAttachments:  newRateMetric(a.rerankCorrect, a.rerankAttempts),
			WrongAttachments:    newRateMetric(a.rerankWrong, a.rerankAttempts),
			RequiredAttachments: a.rerankRequired,
			MissedByAbstention:  newRateMetric(a.rerankMissedAbstention, a.rerankRequired),
			AbstainAllowedCases: a.rerankAbstainAllowed,
			CorrectAbstentions:  newRateMetric(a.rerankCorrectAbstention, a.rerankAbstainAllowed),
		}
	case TaskContentFilter:
		score.ContentFilter = &ContentFilterMetrics{
			EnglishCases:             a.contentEnglish,
			NonEnglishCases:          a.contentNonEnglish,
			UncertainCases:           a.contentUncertain,
			CorrectKeeps:             newRateMetric(a.contentCorrectKeep, a.contentEnglish),
			FalseDrops:               newRateMetric(a.contentFalseDrop, a.contentEnglish),
			CorrectDrops:             newRateMetric(a.contentCorrectDrop, a.contentNonEnglish),
			MissedNonEnglish:         newRateMetric(a.contentMissedNonEnglish, a.contentNonEnglish),
			CorrectUncertainAbstains: newRateMetric(a.contentCorrectAbstain, a.contentUncertain),
			ActionsOnUncertain:       newRateMetric(a.contentActionUncertain, a.contentUncertain),
		}
	case TaskJunkPurge:
		score.JunkPurge = a.finalizeJunkPurge()
	}

	return score
}

func (a *latencyAccumulator) observe(result ResultRecord) error {
	if result.RequestTiming == nil {
		a.missing++
	} else {
		deadline := result.RequestTiming.DeadlineMS
		if a.deadline != 0 && a.deadline != deadline {
			return fmt.Errorf(
				"inconsistent request deadlines %d and %d",
				a.deadline,
				deadline,
			)
		}
		a.deadline = deadline
		a.elapsedMS = append(a.elapsedMS, result.RequestTiming.ElapsedMS)
		if result.RequestTiming.ExceededDeadline() {
			a.deadlineExceeded++
		}
	}
	if result.ErrorCode == "timeout" ||
		(result.RequestTiming != nil && result.RequestTiming.ExceededDeadline()) {
		a.timeouts++
	}
	return nil
}

func (a latencyAccumulator) finalize() LatencySummary {
	observed := len(a.elapsedMS)
	result := LatencySummary{
		Observed:              observed,
		Missing:               a.missing,
		TimeoutCount:          a.timeouts,
		DeadlineExceededCount: a.deadlineExceeded,
		DeadlineMS:            a.deadline,
	}
	denominator := observed + a.missing
	if denominator > 0 {
		result.TimeoutRate = float64(a.timeouts) / float64(denominator)
	}
	if observed == 0 {
		return result
	}
	values := append([]int64(nil), a.elapsedMS...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	result.P50MS = nearestRankPercentile(values, 50)
	result.P95MS = nearestRankPercentile(values, 95)
	result.P99MS = nearestRankPercentile(values, 99)
	result.MaxMS = values[len(values)-1]
	return result
}

// nearestRankPercentile requires a non-empty, ascending slice and implements
// the nearest-rank definition: value at ceil(percentile*N/100), one-indexed.
func nearestRankPercentile(sortedValues []int64, percentile int) int64 {
	rank := (percentile*len(sortedValues) + 99) / 100
	return sortedValues[rank-1]
}

func (a *taskAccumulator) finalizeJunkPurge() *JunkPurgeMetrics {
	dispositions := JunkDispositions()
	metrics := &JunkPurgeMetrics{
		Tier:             a.junkTier,
		KeepCases:        a.junkKeep,
		DeleteCases:      a.junkDelete,
		AbstainCases:     a.junkAbstainExpected,
		CorrectKeeps:     newRateMetric(a.junkCorrectKeep, a.junkKeep),
		FalseJunk:        newRateMetric(a.junkFalseJunk, a.junkKeep),
		CorrectJunk:      newRateMetric(a.junkCorrectJunk, a.junkDelete),
		MissedJunk:       newRateMetric(a.junkMissedJunk, a.junkDelete),
		CorrectAbstains:  newRateMetric(a.junkCorrectAbstain, a.junkAbstainExpected),
		ActionsOnAbstain: newRateMetric(a.junkActionOnAbstain, a.junkAbstainExpected),
	}
	// Square 3x3, every cell emitted including zeros.
	for _, expected := range dispositions {
		for _, predicted := range dispositions {
			count := a.junkConfusion[junkConfusionKey{
				expected:  expected,
				predicted: predicted,
			}]
			metrics.Confusion = append(metrics.Confusion, JunkConfusionCell{
				Expected:  expected,
				Predicted: predicted,
				Count:     count,
			})
			metrics.ScoredVerdicts += count
		}
	}

	// Macro-F1 over the DISPOSITION axis, which is the only axis on which both
	// an expected and a predicted value exist.
	macroTotal := 0.0
	for _, disposition := range dispositions {
		support := 0
		predicted := 0
		for _, other := range dispositions {
			support += a.junkConfusion[junkConfusionKey{
				expected:  disposition,
				predicted: other,
			}]
			predicted += a.junkConfusion[junkConfusionKey{
				expected:  other,
				predicted: disposition,
			}]
		}
		truePositive := a.junkConfusion[junkConfusionKey{
			expected:  disposition,
			predicted: disposition,
		}]
		precision := newRateMetric(truePositive, predicted)
		recall := newRateMetric(truePositive, support)
		f1 := 0.0
		if precision.Rate+recall.Rate > 0 {
			f1 = 2 * precision.Rate * recall.Rate / (precision.Rate + recall.Rate)
		}
		if support > 0 {
			macroTotal += f1
			metrics.MacroF1Classes++
		}
	}
	if metrics.MacroF1Classes > 0 {
		metrics.MacroF1 = macroTotal / float64(metrics.MacroF1Classes)
	}

	// Secondary cut: how the model acted on each content class. Emitted in
	// derivation order, and only for classes with support, so a 15-row table of
	// zeros does not drown the signal.
	for _, class := range JunkContentClasses() {
		split := a.junkClassSplit[class]
		if split == nil || split.support == 0 {
			continue
		}
		metrics.PerClass = append(metrics.PerClass, JunkClassMetrics{
			ContentClass: class,
			Support:      split.support,
			Kept:         split.kept,
			Deleted:      split.deleted,
			Abstained:    split.abstained,
		})
	}
	return metrics
}

func extractionMatchesNormalized(actual, expected MatcherExtraction) bool {
	return titleMatchesNormalized(actual.Title, expected.Title) &&
		actual.Year == expected.Year &&
		actual.Type == expected.Type &&
		actual.Season == expected.Season &&
		actual.Episode == expected.Episode &&
		actual.IsAnime == expected.IsAnime &&
		actual.English == expected.English &&
		actual.IsPack == expected.IsPack &&
		actual.IsAdult == expected.IsAdult
}

func titleMatchesNormalized(actual, expected string) bool {
	actual = titlenorm.NormalizeTitleForMatch(actual)
	expected = titlenorm.NormalizeTitleForMatch(expected)
	return actual != "" && actual == expected
}

func (a *taskAccumulator) observeExtractionFields(
	actual MatcherExtraction,
	acceptable []MatcherExtraction,
) {
	if len(acceptable) == 0 {
		return
	}
	a.extractFieldComparable++

	var (
		titleExact, titleNormalized, year, mediaType, season bool
		episode, isAnime, english, isPack, isAdult           bool
	)
	for _, expected := range acceptable {
		titleExact = titleExact || actual.Title == expected.Title
		titleNormalized = titleNormalized ||
			titleMatchesNormalized(actual.Title, expected.Title)
		year = year || actual.Year == expected.Year
		mediaType = mediaType || actual.Type == expected.Type
		season = season || actual.Season == expected.Season
		episode = episode || actual.Episode == expected.Episode
		isAnime = isAnime || actual.IsAnime == expected.IsAnime
		english = english || actual.English == expected.English
		isPack = isPack || actual.IsPack == expected.IsPack
		isAdult = isAdult || actual.IsAdult == expected.IsAdult
	}
	a.extractTitleExact += boolInt(titleExact)
	a.extractTitleNormalized += boolInt(titleNormalized)
	a.extractYear += boolInt(year)
	a.extractType += boolInt(mediaType)
	a.extractSeason += boolInt(season)
	a.extractEpisode += boolInt(episode)
	a.extractIsAnime += boolInt(isAnime)
	a.extractEnglish += boolInt(english)
	a.extractIsPack += boolInt(isPack)
	a.extractIsAdult += boolInt(isAdult)
}

func validateResultForCase(record CorpusRecord, result ResultRecord) error {
	if record.Task == TaskMatcherRerank &&
		result.Status == ResultStatusOK &&
		result.MatcherRerank.Action == MatcherRerankActionAttach {
		for _, candidate := range record.MatcherRerank.Input.Candidates {
			if candidate.TMDBID == result.MatcherRerank.TMDBID {
				return nil
			}
		}
		return fmt.Errorf(
			"matcher_rerank.tmdb_id %d was not in the frozen candidate list",
			result.MatcherRerank.TMDBID,
		)
	}

	return nil
}

func accountUsage(
	seen map[string]requestUsage,
	systemID string,
	task Task,
	suite string,
	caseID string,
	usage Usage,
) (bool, error) {
	if usage.RequestID == "" {
		return true, nil
	}

	key := systemID + "\x00" + usage.RequestID
	previous, exists := seen[key]
	if !exists {
		seen[key] = requestUsage{task: task, suite: suite, usage: usage}
		return true, nil
	}
	if previous.task != task {
		return false, fmt.Errorf(
			"request_id %q spans tasks %q and %q",
			usage.RequestID,
			previous.task,
			task,
		)
	}
	if previous.suite != suite {
		return false, fmt.Errorf(
			"request_id %q spans primary suites %q and %q",
			usage.RequestID,
			previous.suite,
			suite,
		)
	}
	if previous.usage != usage {
		return false, fmt.Errorf(
			"request_id %q has inconsistent usage at case %q",
			usage.RequestID,
			caseID,
		)
	}

	return false, nil
}

func scoreUsageAccountingComplete(result ResultRecord) bool {
	usage := result.Usage
	if !usage.AccountingComplete || usage.InputTokens <= 0 {
		return false
	}
	// Embedding/rerank specialists account only input/search units. Their
	// successful normalized result carries the deterministic specialist audit;
	// generative result rows must also prove positive output accounting.
	if result.MatcherRerank != nil &&
		result.MatcherRerank.SpecialistAudit != nil {
		return true
	}
	return usage.OutputTokens > 0
}

func (u *UsageTotals) addRequest(usage Usage, accountingComplete bool) error {
	if u.Requests == int(^uint(0)>>1) {
		return fmt.Errorf("request count overflow")
	}
	requestTokens, err := checkedAddInt64(usage.InputTokens, usage.OutputTokens)
	if err != nil {
		return fmt.Errorf("request total_tokens: %w", err)
	}
	input, err := checkedAddInt64(u.InputTokens, usage.InputTokens)
	if err != nil {
		return fmt.Errorf("input_tokens: %w", err)
	}
	cached, err := checkedAddInt64(u.CachedInputTokens, usage.CachedInputTokens)
	if err != nil {
		return fmt.Errorf("cached_input_tokens: %w", err)
	}
	cacheWrite, err := checkedAddInt64(u.CacheWriteTokens, usage.CacheWriteTokens)
	if err != nil {
		return fmt.Errorf("cache_write_tokens: %w", err)
	}
	output, err := checkedAddInt64(u.OutputTokens, usage.OutputTokens)
	if err != nil {
		return fmt.Errorf("output_tokens: %w", err)
	}
	reasoning, err := checkedAddInt64(u.ReasoningTokens, usage.ReasoningTokens)
	if err != nil {
		return fmt.Errorf("reasoning_tokens: %w", err)
	}
	total, err := checkedAddInt64(u.TotalTokens, requestTokens)
	if err != nil {
		return fmt.Errorf("total_tokens: %w", err)
	}
	cost, err := checkedAddInt64(u.CostMicroUSD, usage.CostMicroUSD)
	if err != nil {
		return fmt.Errorf("cost_micro_usd: %w", err)
	}

	if u.Requests == 0 {
		u.AccountingComplete = true
	}
	if !accountingComplete {
		u.AccountingComplete = false
		u.IncompleteRequests++
	}
	u.Requests++
	u.InputTokens = input
	u.CachedInputTokens = cached
	u.CacheWriteTokens = cacheWrite
	u.OutputTokens = output
	u.ReasoningTokens = reasoning
	u.TotalTokens = total
	u.CostMicroUSD = cost
	u.CostUSD = float64(cost) / 1_000_000
	return nil
}

func checkedAddInt64(left, right int64) (int64, error) {
	if left < 0 || right < 0 {
		return 0, fmt.Errorf("negative operand")
	}
	if right > math.MaxInt64-left {
		return 0, fmt.Errorf("integer overflow")
	}
	return left + right, nil
}

func newRateMetric(count, denominator int) RateMetric {
	interval := Wilson95(count, denominator)
	rate := 0.0
	if denominator > 0 {
		rate = float64(count) / float64(denominator)
	}

	return RateMetric{
		Count:       count,
		Denominator: denominator,
		Rate:        rate,
		Wilson95:    interval,
	}
}

// Wilson95 returns a two-sided 95% Wilson score interval. With no
// observations, [0,1] honestly represents complete uncertainty.
func Wilson95(successes, total int) WilsonInterval {
	if total <= 0 {
		return WilsonInterval{Lower: 0, Upper: 1}
	}
	if successes < 0 {
		successes = 0
	}
	if successes > total {
		successes = total
	}

	n := float64(total)
	p := float64(successes) / n
	z2 := wilson95Z * wilson95Z
	denominator := 1 + z2/n
	center := (p + z2/(2*n)) / denominator
	margin := wilson95Z *
		math.Sqrt((p*(1-p)+z2/(4*n))/n) /
		denominator

	return WilsonInterval{
		Lower: math.Max(0, center-margin),
		Upper: math.Min(1, center+margin),
	}
}

// rejectCrossTierJunkPurge makes blending gold tiers a schema error rather than
// a convention someone has to remember.
//
// Tier A is the oracle-resolvable end of the distribution by construction and
// Tier B is the hard end where false-junk actually lives, so one pooled
// false-junk rate lets the easy tier mask the hard one. That is the same
// mistake as scoring against the 2,506 off-distribution import-confirmed
// hashes. Tier C contains no keep-worthy content at all and can only dilute a
// safety denominator. Score each tier and report them side by side.
func rejectCrossTierJunkPurge(records []CorpusRecord) error {
	seen := make(map[GoldTier]string, 4)
	for _, record := range records {
		if record.Task != TaskJunkPurge {
			continue
		}
		if _, ok := seen[record.Tier]; !ok {
			seen[record.Tier] = record.CaseID
		}
	}
	if len(seen) <= 1 {
		return nil
	}
	tiers := make([]string, 0, len(seen))
	for tier, caseID := range seen {
		tiers = append(tiers, fmt.Sprintf("%s (e.g. %s)", tier, caseID))
	}
	sort.Strings(tiers)
	return fmt.Errorf(
		"junkpurge corpus mixes gold tiers %s: tiers are scored separately and "+
			"never blended, because Tier A is the easy end of the distribution "+
			"by construction and would mask Tier B",
		strings.Join(tiers, ", "),
	)
}
