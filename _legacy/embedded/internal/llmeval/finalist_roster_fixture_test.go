package llmeval

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalistEvidenceRejectsIncompleteCostAccounting(t *testing.T) {
	taskCorpus := finalistStageOneCorpusForTest(t, TaskContentFilter)
	roster := finalistTaskRosterFromCorpus(
		t,
		taskCorpus,
		TaskContentFilter,
		finalistRosterSystemIDs{
			normalized: "normalized-control",
			production: "production-control",
			candidate:  "candidate",
		},
		finalistFixtureSHA("stage-one"),
		finalistFixtureSHA("promotion"),
	)
	candidateIndex := -1
	for index := range roster.SystemEvidence {
		if roster.SystemEvidence[index].Role == PromotionRoleCandidate {
			candidateIndex = index
			break
		}
	}
	require.NotEqual(t, -1, candidateIndex)
	candidate := roster.SystemEvidence[candidateIndex]
	candidate.Score.Suites[0].Usage.AccountingComplete = false
	candidate.Score.Suites[0].Usage.IncompleteRequests = 1
	candidate.Score.Suites[0].Tasks[0].Usage.AccountingComplete = false
	candidate.Score.Suites[0].Tasks[0].Usage.IncompleteRequests = 1
	canonicalScore, err := canonicalJSONIdentity(candidate.Score)
	require.NoError(t, err)
	candidate.CanonicalScoreReportSHA256 = canonicalScore
	roster.SystemEvidence[candidateIndex] = candidate

	_, _, err = validateFinalistSystemEvidence(
		roster,
		candidate,
		finalistEvidenceByID(roster.SystemEvidence),
	)
	require.ErrorContains(t, err, "score.usage differs from its suite usage aggregate")
}

// finalistTaskRosterForTest builds one Stage 1 task roster entry from genuine
// Score and Compare artifacts over the task-filtered development corpus.
// FinalistRosterManifest.Validate recomputes every gate decision from the
// embedded metrics, so hand-stamped attestations cannot satisfy it; the fixture
// therefore has to run the real scorer and comparator.
func finalistTaskRosterForTest(
	t *testing.T,
	development Corpus,
	task Task,
	stageOneRosterSHA string,
	promotionRosterSHA string,
) FinalistTaskRoster {
	t.Helper()

	taskCorpus, err := FilterCorpus(development, task, 0)
	require.NoError(t, err)

	return finalistTaskRosterFromCorpus(
		t,
		taskCorpus,
		task,
		finalistRosterSystemIDs{
			normalized: "stage1-normalized-control",
			production: "stage1-production-control",
			candidate:  "stage1-candidate",
		},
		stageOneRosterSHA,
		promotionRosterSHA,
	)
}

type finalistRosterSystemIDs struct {
	normalized string
	production string
	candidate  string
}

func finalistTaskRosterFromCorpus(
	t *testing.T,
	taskCorpus Corpus,
	task Task,
	systems finalistRosterSystemIDs,
	stageOneRosterSHA string,
	promotionRosterSHA string,
) FinalistTaskRoster {
	t.Helper()

	policy := DefaultStageOneComparisonPolicy(task)

	normalized := finalistSystemEvidenceForTest(
		t,
		taskCorpus,
		task,
		systems.normalized,
		EvaluationLaneNormalizedStrict,
		PromotionRoleControl,
	)
	production := finalistSystemEvidenceForTest(
		t,
		taskCorpus,
		task,
		systems.production,
		EvaluationLaneProductionFidelity,
		PromotionRoleControl,
	)
	candidate := finalistSystemEvidenceForTest(
		t,
		taskCorpus,
		task,
		systems.candidate,
		EvaluationLaneNormalizedStrict,
		PromotionRoleCandidate,
	)
	candidate.ControlSystemID = normalized.SystemID
	candidate.ProductionControlSystemID = production.SystemID
	candidate.NormalizedComparison = finalistComparisonForTest(
		t,
		taskCorpus,
		policy,
		normalized,
		candidate,
	)
	candidate.ProductionComparison = finalistComparisonForTest(
		t,
		taskCorpus,
		policy,
		production,
		candidate,
	)

	roster := FinalistTaskRoster{
		Task:                        task,
		TaskDevelopmentCorpusSHA256: taskCorpus.SHA256,
		StageOneRosterSHA256:        stageOneRosterSHA,
		PromotionRosterSHA256:       promotionRosterSHA,
		ComparisonPolicy:            policy,
		SystemEvidence: []FinalistStageOneSystemEvidence{
			normalized,
			production,
			candidate,
		},
	}

	return finalistRecomputeRosterForTest(t, roster)
}

// finalistRecomputeRosterForTest derives Passed, ReasonCodes, the ordered
// passer list, and the counts the same way the validator recomputes them, so a
// fixture never hand-stamps an outcome the evidence does not support.
func finalistRecomputeRosterForTest(
	t *testing.T,
	roster FinalistTaskRoster,
) FinalistTaskRoster {
	t.Helper()

	byID := finalistEvidenceByID(roster.SystemEvidence)
	type passer struct {
		systemID string
		cost     int64
	}
	var passing []passer
	candidates := 0
	for index, evidence := range roster.SystemEvidence {
		if evidence.Role == PromotionRoleCandidate {
			candidates++
		}
		pass, reasons, err := validateFinalistSystemEvidence(
			roster,
			evidence,
			byID,
		)
		require.NoError(t, err)
		roster.SystemEvidence[index].Passed = pass
		roster.SystemEvidence[index].ReasonCodes = reasons
		if pass {
			passing = append(passing, passer{
				systemID: evidence.SystemID,
				cost: evidence.
					ManifestRecomputedStageOneCostMicroUSD,
			})
		}
	}
	sort.Slice(passing, func(left, right int) bool {
		if passing[left].cost != passing[right].cost {
			return passing[left].cost < passing[right].cost
		}
		return passing[left].systemID < passing[right].systemID
	})
	roster.SystemIDs = nil
	for _, entry := range passing {
		roster.SystemIDs = append(roster.SystemIDs, entry.systemID)
	}
	roster.StageOneCandidateSystems = candidates
	roster.StageOnePassingSystems = len(roster.SystemIDs)

	return roster
}

// finalistRosterAddCandidateForTest screens one more candidate into a frozen
// task roster with real evidence, then recomputes the passer list.
func finalistRosterAddCandidateForTest(
	t *testing.T,
	roster FinalistTaskRoster,
	taskCorpus Corpus,
	systemID string,
) FinalistTaskRoster {
	t.Helper()

	candidate := finalistSystemEvidenceForTest(
		t,
		taskCorpus,
		roster.Task,
		systemID,
		EvaluationLaneNormalizedStrict,
		PromotionRoleCandidate,
	)
	for _, evidence := range roster.SystemEvidence {
		if evidence.Role != PromotionRoleControl {
			continue
		}
		if evidence.Lane == EvaluationLaneNormalizedStrict {
			candidate.ControlSystemID = evidence.SystemID
			candidate.NormalizedComparison = finalistComparisonForTest(
				t,
				taskCorpus,
				roster.ComparisonPolicy,
				evidence,
				candidate,
			)
		}
		if evidence.Lane == EvaluationLaneProductionFidelity {
			candidate.ProductionControlSystemID = evidence.SystemID
			candidate.ProductionComparison = finalistComparisonForTest(
				t,
				taskCorpus,
				roster.ComparisonPolicy,
				evidence,
				candidate,
			)
		}
	}
	roster.SystemEvidence = append(roster.SystemEvidence, candidate)

	return finalistRecomputeRosterForTest(t, roster)
}

// finalistTaskRosterBindManifestForTest replaces the generic fixture
// descriptors with the exact manifest systems used by a promotion-bundle
// fixture, then regenerates every comparison and canonical score identity.
// Production validation deliberately rejects same-ID evidence from a
// different provider/model/prompt/lane.
func finalistTaskRosterBindManifestForTest(
	t *testing.T,
	roster FinalistTaskRoster,
	taskCorpus Corpus,
	manifest SystemManifest,
) FinalistTaskRoster {
	t.Helper()
	configs := make(map[string]SystemConfig, len(manifest.Systems))
	for _, config := range manifest.Systems {
		configs[config.SystemID] = config
	}
	for index := range roster.SystemEvidence {
		evidence := &roster.SystemEvidence[index]
		config, exists := configs[evidence.SystemID]
		require.True(t, exists, "fixture manifest system %q", evidence.SystemID)
		evidence.Score.System = config.Descriptor()
		evidence.Lane = config.EvaluationLane
		repriceFinalistScoreForManifestTest(t, &evidence.Score, config)
		evidence.MeasuredStageOneCostMicroUSD = evidence.Score.Usage.CostMicroUSD
		recomputedCost, err := promotionAggregateManifestCost(
			config,
			evidence.Score.Usage,
			evidence.DeploymentServiceTier,
			"fixture",
		)
		require.NoError(t, err)
		evidence.ManifestRecomputedStageOneCostMicroUSD = recomputedCost
		canonicalScore, err := canonicalJSONIdentity(evidence.Score)
		require.NoError(t, err)
		evidence.CanonicalScoreReportSHA256 = canonicalScore
	}

	byID := finalistEvidenceByID(roster.SystemEvidence)
	for index := range roster.SystemEvidence {
		candidate := &roster.SystemEvidence[index]
		if candidate.Role != PromotionRoleCandidate {
			continue
		}
		normalized, exists := byID[candidate.ControlSystemID]
		require.True(t, exists)
		production, exists := byID[candidate.ProductionControlSystemID]
		require.True(t, exists)
		candidate.NormalizedComparison = finalistComparisonForTest(
			t,
			taskCorpus,
			roster.ComparisonPolicy,
			normalized,
			*candidate,
		)
		candidate.ProductionComparison = finalistComparisonForTest(
			t,
			taskCorpus,
			roster.ComparisonPolicy,
			production,
			*candidate,
		)
	}

	return finalistRecomputeRosterForTest(t, roster)
}

func repriceFinalistScoreForManifestTest(
	t *testing.T,
	score *ScoreReport,
	config SystemConfig,
) {
	t.Helper()
	var overallCost int64
	for suiteIndex := range score.Suites {
		suite := &score.Suites[suiteIndex]
		var suiteCost int64
		for taskIndex := range suite.Tasks {
			task := &suite.Tasks[taskIndex]
			cost, err := promotionAggregateManifestCost(
				config,
				task.Usage,
				PromotionServiceStandard,
				"fixture task",
			)
			require.NoError(t, err)
			task.Usage.CostMicroUSD = cost
			task.Usage.CostUSD = float64(cost) / 1_000_000
			suiteCost, err = checkedAddInt64(suiteCost, cost)
			require.NoError(t, err)
		}
		suite.Usage.CostMicroUSD = suiteCost
		suite.Usage.CostUSD = float64(suiteCost) / 1_000_000
		var err error
		overallCost, err = checkedAddInt64(overallCost, suiteCost)
		require.NoError(t, err)
	}
	score.Usage.CostMicroUSD = overallCost
	score.Usage.CostUSD = float64(overallCost) / 1_000_000
}

func finalistEvidenceByID(
	evidence []FinalistStageOneSystemEvidence,
) map[string]FinalistStageOneSystemEvidence {
	byID := make(map[string]FinalistStageOneSystemEvidence, len(evidence))
	for _, entry := range evidence {
		byID[entry.SystemID] = entry
	}

	return byID
}

func finalistSystemEvidenceForTest(
	t *testing.T,
	taskCorpus Corpus,
	task Task,
	systemID string,
	lane EvaluationLane,
	role PromotionSystemRole,
) FinalistStageOneSystemEvidence {
	t.Helper()

	descriptor := testSystem()
	descriptor.SystemID = systemID
	results := finalistCorrectResultsForTest(taskCorpus, descriptor)
	score, err := Score(taskCorpus, results, testEvaluatorBuildSHA)
	require.NoError(t, err)
	require.NotEmpty(t, score.Suites)
	for _, suite := range score.Suites {
		require.Len(t, suite.Tasks, 1)
		require.Equal(t, task, suite.Tasks[0].Task)
	}

	canonicalScore, err := canonicalJSONIdentity(score)
	require.NoError(t, err)

	return FinalistStageOneSystemEvidence{
		Role:                                   role,
		SystemID:                               systemID,
		DecisionThresholds:                     promotionDecisionThresholds(ProductionThresholds()),
		DeploymentServiceTier:                  PromotionServiceStandard,
		Lane:                                   lane,
		ResultsArtifactSHA256:                  finalistFixtureSHA(systemID + ":results"),
		CanonicalResultsSHA256:                 finalistFixtureSHA(systemID + ":canonical-results"),
		ScoreReportArtifactSHA256:              finalistFixtureSHA(systemID + ":score"),
		CanonicalScoreReportSHA256:             canonicalScore,
		Score:                                  score,
		MeasuredStageOneCostMicroUSD:           score.Usage.CostMicroUSD,
		ManifestRecomputedStageOneCostMicroUSD: score.Usage.CostMicroUSD,
	}
}

func finalistComparisonForTest(
	t *testing.T,
	taskCorpus Corpus,
	policy StageOneComparisonPolicy,
	control,
	candidate FinalistStageOneSystemEvidence,
) *FinalistStageOneComparisonEvidence {
	t.Helper()

	controlDescriptor := control.Score.System
	candidateDescriptor := candidate.Score.System

	report, err := Compare(
		taskCorpus,
		finalistCorrectResultsForTest(taskCorpus, controlDescriptor),
		finalistCorrectResultsForTest(taskCorpus, candidateDescriptor),
		policy.options(testEvaluatorBuildSHA),
	)
	require.NoError(t, err)

	canonicalReport, err := canonicalJSONIdentity(report)
	require.NoError(t, err)

	return &FinalistStageOneComparisonEvidence{
		ReportArtifactSHA256: finalistFixtureSHA(
			control.SystemID + ":" + candidate.SystemID + ":report",
		),
		CanonicalReportSHA256: canonicalReport,
		Report:                report,
	}
}

// finalistCorrectResultsForTest answers every frozen case correctly, so the
// Stage 1 gates measure the protocol wiring rather than a seeded regression.
func finalistCorrectResultsForTest(
	corpus Corpus,
	system SystemDescriptor,
) []ResultRecord {
	results := make([]ResultRecord, 0, len(corpus.Records))
	for index, record := range corpus.Records {
		result := testResult(corpus, record.CaseID, record.Task)
		result.System = system
		if system.Provider != "openrouter" {
			result.ExecutionAudit.Route = RouteAudit{
				ReturnedModel: system.Model,
				Proof:         RouteProofDirectResponseModel,
				Verified:      true,
			}
		}
		result.Usage = Usage{
			RequestID:          fmt.Sprintf("%s:%d", system.SystemID, index),
			InputTokens:        10,
			OutputTokens:       5,
			CostMicroUSD:       7,
			AccountingComplete: true,
		}
		switch record.Task {
		case TaskMatcherExtract:
			expected := record.MatcherExtract.Expected
			if len(expected.Acceptable) > 0 {
				extraction := expected.Acceptable[0]
				result.MatcherExtract = &MatcherExtractResult{
					Action:     MatcherExtractActionExtract,
					Extraction: &extraction,
				}
			} else {
				result.MatcherExtract = &MatcherExtractResult{
					Action: MatcherExtractActionAbstain,
				}
			}
		case TaskMatcherRerank:
			expected := record.MatcherRerank.Expected
			if len(expected.AcceptableTMDBIDs) > 0 {
				result.MatcherRerank = &MatcherRerankResult{
					Action:     MatcherRerankActionAttach,
					TMDBID:     expected.AcceptableTMDBIDs[0],
					Confidence: 0.99,
				}
			} else {
				result.MatcherRerank = &MatcherRerankResult{
					Action: MatcherRerankActionAbstain,
				}
			}
		case TaskContentFilter:
			switch record.ContentFilter.Expected.Language {
			case LanguageEnglish:
				result.ContentFilter = &ContentFilterResult{
					Action:     ContentFilterActionKeep,
					IsEnglish:  boolPointer(true),
					Confidence: 0.99,
					ReasonTag:  "fixture",
				}
			case LanguageNonEnglish:
				result.ContentFilter = &ContentFilterResult{
					Action:     ContentFilterActionDrop,
					IsEnglish:  boolPointer(false),
					Confidence: 0.99,
					ReasonTag:  "fixture",
				}
			default:
				result.ContentFilter = &ContentFilterResult{
					Action:     ContentFilterActionAbstain,
					Confidence: 0.5,
					ReasonTag:  "fixture",
				}
			}
		case TaskJunkPurge:
			// A perfect-model fixture: the action agrees with the gold
			// disposition. The model's own verdict is derived from the ACTION,
			// not copied from gold — echoing a gold field into a result is how
			// a fixture stops being able to detect a scoring bug.
			action := JunkPurgeActionKeep
			verdict := JunkVerdictRealMangled
			confidence := 0.99
			switch record.JunkPurge.Expected.Disposition {
			case JunkDispositionDelete:
				action, verdict = JunkPurgeActionJunk, JunkVerdictJunk
			case JunkDispositionAbstain:
				action, verdict = JunkPurgeActionAbstain, JunkVerdictUnsure
				confidence = 0.5
			}
			result.JunkPurge = &JunkPurgeResult{
				Action:     action,
				Verdict:    verdict,
				Confidence: confidence,
			}
		}
		results = append(results, result)
	}

	return results
}

func finalistFixtureSHA(seed string) string {
	return sha256Hex([]byte(seed))
}

// finalistStageOneCorpusForTest builds the minimum Stage 1 shape for one task:
// one natural and one safety case, which is what the suite-separated gates
// require.
func finalistStageOneCorpusForTest(t *testing.T, task Task) Corpus {
	t.Helper()

	records := make([]CorpusRecord, 0, 2)
	for _, suite := range []string{
		StageOneRequiredPrimarySuite,
		StageOneSafetySuite(task),
	} {
		id := fmt.Sprintf("stage1:%s:%s", task, suite)
		var record CorpusRecord
		switch task {
		case TaskMatcherExtract:
			record = testExtractRecord(id, MatcherExtractExpected{
				Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
			})
		case TaskMatcherRerank:
			record = testRerankRecord(id, MatcherRerankExpected{
				AcceptableTMDBIDs: []int64{101},
			})
		case TaskContentFilter:
			record = testContentRecord(id, LanguageEnglish)
		case TaskJunkPurge:
			record = testJunkRecord(id, JunkClassDegenerate)
		default:
			t.Fatalf("unsupported task %q", task)
		}
		record.GroupID = "group:" + id
		record.SliceIDs = []string{
			"all",
			"fixture",
			CorpusPrimarySuiteSlicePrefix + suite,
		}
		records = append(records, record)
	}

	return mustCorpus(t, records)
}
