package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	MatcherSpecialistCalibrationArtifactVersion = "bitagent-matcher-specialist-calibration-v1"
	MatcherSpecialistCalibrationStatus          = "development_threshold_fitted_zero_observed_wrong_attachments"
	MatcherSpecialistCalibrationObjectiveID     = "zero-wrong-attachments-maximize-correct-coverage-v1"
	MatcherSpecialistCalibrationTieBreakID      = "lowest-threshold-ppb-safety-wins-equal-score-v1"

	maxMatcherSpecialistCalibrationBytes = 4 << 20
)

type MatcherSpecialistCalibrationMetrics struct {
	ThresholdPPB       int64 `json:"threshold_ppb"`
	Cases              int   `json:"cases"`
	Groups             int   `json:"groups"`
	AttachmentAttempts int   `json:"attachment_attempts"`
	CorrectAttachments int   `json:"correct_attachments"`
	WrongAttachments   int   `json:"wrong_attachments"`
	Abstentions        int   `json:"abstentions"`
}

type MatcherSpecialistCalibrationSuite struct {
	Suite  string                              `json:"suite"`
	Before MatcherSpecialistCalibrationMetrics `json:"before"`
	After  MatcherSpecialistCalibrationMetrics `json:"after"`
}

// MatcherSpecialistCalibrationArtifact is a deterministic development-only
// threshold fit. It binds the complete immutable input chain and never carries
// prompts, release names, provider envelopes, or secrets.
type MatcherSpecialistCalibrationArtifact struct {
	SchemaVersion   int    `json:"schema_version"`
	ArtifactVersion string `json:"artifact_version"`
	Status          string `json:"status"`
	AlgorithmID     string `json:"algorithm_id"`
	ObjectiveID     string `json:"objective_id"`
	TieBreakID      string `json:"tie_break_id"`

	PlanID                           string `json:"plan_id"`
	PlanSHA256                       string `json:"plan_sha256"`
	DevelopmentClosureManifestSHA256 string `json:"development_closure_manifest_sha256"`
	CandidateFreezeManifestSHA256    string `json:"candidate_freeze_manifest_sha256"`
	FullDevelopmentCorpusSHA256      string `json:"full_development_corpus_sha256"`
	MatcherDevelopmentCorpusSHA256   string `json:"matcher_development_corpus_sha256"`

	SystemManifestSHA256 string           `json:"system_manifest_sha256"`
	SystemID             string           `json:"system_id"`
	System               SystemDescriptor `json:"system"`
	RouteSnapshotSHA256  string           `json:"route_snapshot_sha256"`
	EvaluatorBuildSHA256 string           `json:"evaluator_build_sha256"`

	RawResultsArtifactSHA256  string                              `json:"raw_results_artifact_sha256"`
	CanonicalRawResultsSHA256 string                              `json:"canonical_raw_results_sha256"`
	ThresholdPPB              int64                               `json:"threshold_ppb"`
	DevelopmentCases          int                                 `json:"development_cases"`
	DevelopmentGroups         int                                 `json:"development_groups"`
	Before                    MatcherSpecialistCalibrationMetrics `json:"before"`
	After                     MatcherSpecialistCalibrationMetrics `json:"after"`
	Suites                    []MatcherSpecialistCalibrationSuite `json:"suites"`
}

type MatcherSpecialistCalibrationInput struct {
	Development              Corpus
	DevelopmentClosure       GoldDevelopmentClosureManifest
	DevelopmentClosureSHA256 string
	System                   SystemConfig
	SystemManifestSHA256     string
	RouteSnapshotSHA256      string
	EvaluatorBuildSHA256     string
	RawResults               []ResultRecord
	RawResultsArtifactSHA256 string
}

func FitMatcherSpecialistCalibration(
	input MatcherSpecialistCalibrationInput,
) (MatcherSpecialistCalibrationArtifact, error) {
	matcher, resultByCase, canonicalResultsSHA, err :=
		validateMatcherSpecialistCalibrationInput(input)
	if err != nil {
		return MatcherSpecialistCalibrationArtifact{}, err
	}

	thresholdPPB := int64(0)
	for _, record := range matcher.Records {
		result := resultByCase[record.CaseID]
		audit := result.MatcherRerank.SpecialistAudit
		if audit.SelectedTMDBID == 0 ||
			matcherCandidatePolicyVeto(result.MatcherRerank.PolicyReason) ||
			matcherSpecialistSelectionCorrect(record, audit.SelectedTMDBID) {
			continue
		}
		if audit.ScorePPB == MatcherSpecialistScoreScalePPB {
			return MatcherSpecialistCalibrationArtifact{}, fmt.Errorf(
				"calibration ineligible: wrong attachment for case %q has maximum score_ppb",
				record.CaseID,
			)
		}
		candidateThreshold := audit.ScorePPB + 1
		if candidateThreshold > thresholdPPB {
			thresholdPPB = candidateThreshold
		}
	}

	before, beforeSuites, err := matcherSpecialistCalibrationMetrics(
		matcher,
		resultByCase,
		0,
	)
	if err != nil {
		return MatcherSpecialistCalibrationArtifact{}, err
	}
	after, afterSuites, err := matcherSpecialistCalibrationMetrics(
		matcher,
		resultByCase,
		thresholdPPB,
	)
	if err != nil {
		return MatcherSpecialistCalibrationArtifact{}, err
	}
	if after.WrongAttachments != 0 {
		return MatcherSpecialistCalibrationArtifact{}, fmt.Errorf(
			"calibration objective failed to eliminate wrong attachments",
		)
	}
	suiteNames := make([]string, 0, len(beforeSuites))
	for suite := range beforeSuites {
		suiteNames = append(suiteNames, suite)
	}
	sort.Strings(suiteNames)
	suites := make([]MatcherSpecialistCalibrationSuite, 0, len(suiteNames))
	for _, suite := range suiteNames {
		suites = append(suites, MatcherSpecialistCalibrationSuite{
			Suite:  suite,
			Before: beforeSuites[suite],
			After:  afterSuites[suite],
		})
	}

	artifact := MatcherSpecialistCalibrationArtifact{
		SchemaVersion:                    SchemaVersion,
		ArtifactVersion:                  MatcherSpecialistCalibrationArtifactVersion,
		Status:                           MatcherSpecialistCalibrationStatus,
		AlgorithmID:                      MatcherSpecialistAlgorithmID,
		ObjectiveID:                      MatcherSpecialistCalibrationObjectiveID,
		TieBreakID:                       MatcherSpecialistCalibrationTieBreakID,
		PlanID:                           input.DevelopmentClosure.PlanID,
		PlanSHA256:                       input.DevelopmentClosure.PlanSHA256,
		DevelopmentClosureManifestSHA256: input.DevelopmentClosureSHA256,
		CandidateFreezeManifestSHA256:    input.DevelopmentClosure.CandidateFreezeManifestSHA256,
		FullDevelopmentCorpusSHA256:      input.Development.SHA256,
		MatcherDevelopmentCorpusSHA256:   matcher.SHA256,
		SystemManifestSHA256:             input.SystemManifestSHA256,
		SystemID:                         input.System.SystemID,
		System:                           input.System.Descriptor(),
		RouteSnapshotSHA256:              input.RouteSnapshotSHA256,
		EvaluatorBuildSHA256:             input.EvaluatorBuildSHA256,
		RawResultsArtifactSHA256:         input.RawResultsArtifactSHA256,
		CanonicalRawResultsSHA256:        canonicalResultsSHA,
		ThresholdPPB:                     thresholdPPB,
		DevelopmentCases:                 len(matcher.Records),
		DevelopmentGroups:                before.Groups,
		Before:                           before,
		After:                            after,
		Suites:                           suites,
	}
	if err := artifact.Validate(); err != nil {
		return MatcherSpecialistCalibrationArtifact{}, err
	}
	return artifact, nil
}

func (a MatcherSpecialistCalibrationArtifact) Validate() error {
	if a.SchemaVersion != SchemaVersion ||
		a.ArtifactVersion != MatcherSpecialistCalibrationArtifactVersion ||
		a.Status != MatcherSpecialistCalibrationStatus ||
		a.AlgorithmID != MatcherSpecialistAlgorithmID ||
		a.ObjectiveID != MatcherSpecialistCalibrationObjectiveID ||
		a.TieBreakID != MatcherSpecialistCalibrationTieBreakID {
		return fmt.Errorf("matcher specialist calibration header is unsupported")
	}
	if err := validateIdentifier("plan_id", a.PlanID); err != nil {
		return err
	}
	if err := validateIdentifier("system_id", a.SystemID); err != nil {
		return err
	}
	if err := a.System.Validate(); err != nil {
		return fmt.Errorf("system: %w", err)
	}
	if a.System.SystemID != a.SystemID ||
		a.System.PromptVersion != MatcherSpecialistAlgorithmID {
		return fmt.Errorf("system descriptor does not match specialist binding")
	}
	for name, value := range map[string]string{
		"plan_sha256":                         a.PlanSHA256,
		"development_closure_manifest_sha256": a.DevelopmentClosureManifestSHA256,
		"candidate_freeze_manifest_sha256":    a.CandidateFreezeManifestSHA256,
		"full_development_corpus_sha256":      a.FullDevelopmentCorpusSHA256,
		"matcher_development_corpus_sha256":   a.MatcherDevelopmentCorpusSHA256,
		"system_manifest_sha256":              a.SystemManifestSHA256,
		"route_snapshot_sha256":               a.RouteSnapshotSHA256,
		"evaluator_build_sha256":              a.EvaluatorBuildSHA256,
		"raw_results_artifact_sha256":         a.RawResultsArtifactSHA256,
		"canonical_raw_results_sha256":        a.CanonicalRawResultsSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if a.ThresholdPPB < 0 ||
		a.ThresholdPPB > MatcherSpecialistScoreScalePPB {
		return fmt.Errorf("threshold_ppb is out of range")
	}
	if a.DevelopmentCases <= 0 || a.DevelopmentGroups <= 0 ||
		a.DevelopmentGroups > a.DevelopmentCases {
		return fmt.Errorf("development case/group counts are invalid")
	}
	if err := validateMatcherSpecialistMetric(
		"before",
		a.Before,
		a.DevelopmentCases,
		a.DevelopmentGroups,
		0,
	); err != nil {
		return err
	}
	if err := validateMatcherSpecialistMetric(
		"after",
		a.After,
		a.DevelopmentCases,
		a.DevelopmentGroups,
		a.ThresholdPPB,
	); err != nil {
		return err
	}
	if a.After.WrongAttachments != 0 {
		return fmt.Errorf("after metrics must have zero wrong attachments")
	}
	if len(a.Suites) != 2 ||
		a.Suites[0].Suite != "natural" ||
		a.Suites[1].Suite != "safety" {
		return fmt.Errorf("calibration requires canonical natural and safety suites")
	}
	totalCases := 0
	totalGroups := 0
	for index, suite := range a.Suites {
		if index > 0 && suite.Suite <= a.Suites[index-1].Suite {
			return fmt.Errorf("calibration suites are not canonically ordered")
		}
		if err := validateMatcherSpecialistMetric(
			"suites.before",
			suite.Before,
			suite.Before.Cases,
			suite.Before.Groups,
			0,
		); err != nil {
			return err
		}
		if err := validateMatcherSpecialistMetric(
			"suites.after",
			suite.After,
			suite.Before.Cases,
			suite.Before.Groups,
			a.ThresholdPPB,
		); err != nil {
			return err
		}
		if suite.After.WrongAttachments != 0 {
			return fmt.Errorf("suite after metrics must have zero wrong attachments")
		}
		totalCases += suite.Before.Cases
		totalGroups += suite.Before.Groups
	}
	if totalCases != a.DevelopmentCases ||
		totalGroups != a.DevelopmentGroups {
		return fmt.Errorf("suite totals do not match development totals")
	}
	return nil
}

func ReadMatcherSpecialistCalibrationArtifact(
	r io.Reader,
) (MatcherSpecialistCalibrationArtifact, string, error) {
	if r == nil {
		return MatcherSpecialistCalibrationArtifact{}, "", fmt.Errorf(
			"matcher specialist calibration reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(
		r,
		maxMatcherSpecialistCalibrationBytes+1,
	))
	if err != nil {
		return MatcherSpecialistCalibrationArtifact{}, "", err
	}
	if len(raw) == 0 || len(raw) > maxMatcherSpecialistCalibrationBytes {
		return MatcherSpecialistCalibrationArtifact{}, "", fmt.Errorf(
			"matcher specialist calibration is empty or too large",
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return MatcherSpecialistCalibrationArtifact{}, "", err
	}
	var artifact MatcherSpecialistCalibrationArtifact
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&artifact); err != nil {
		return MatcherSpecialistCalibrationArtifact{}, "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return MatcherSpecialistCalibrationArtifact{}, "", fmt.Errorf(
			"matcher specialist calibration has trailing data",
		)
	}
	if err := artifact.Validate(); err != nil {
		return MatcherSpecialistCalibrationArtifact{}, "", err
	}
	return artifact, sha256Hex(raw), nil
}

func ValidateMatcherSpecialistCalibrationBinding(
	artifact MatcherSpecialistCalibrationArtifact,
	system SystemConfig,
	systemManifestSHA256 string,
	routeSnapshotSHA256 string,
	evaluatorBuildSHA256 string,
) (DecisionThresholds, error) {
	if err := artifact.Validate(); err != nil {
		return DecisionThresholds{}, err
	}
	if err := system.Validate(); err != nil {
		return DecisionThresholds{}, err
	}
	if system.APIKind != APIKindEmbedding ||
		system.PromptVersion != MatcherSpecialistAlgorithmID ||
		artifact.SystemID != system.SystemID ||
		artifact.System != system.Descriptor() ||
		artifact.SystemManifestSHA256 != systemManifestSHA256 ||
		artifact.RouteSnapshotSHA256 != routeSnapshotSHA256 ||
		artifact.EvaluatorBuildSHA256 != evaluatorBuildSHA256 {
		return DecisionThresholds{}, fmt.Errorf(
			"matcher specialist calibration does not bind the exact system, manifest, route snapshot, and evaluator build",
		)
	}
	return MatcherSpecialistThresholds(
		ProductionThresholds(),
		artifact.ThresholdPPB,
	)
}

func RethresholdMatcherSpecialistDevelopment(
	development Corpus,
	system SystemConfig,
	rawResults []ResultRecord,
	artifact MatcherSpecialistCalibrationArtifact,
) ([]ResultRecord, error) {
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	matcher, err := FilterCorpus(development, TaskMatcherRerank, 0)
	if err != nil {
		return nil, err
	}
	if development.SHA256 != artifact.FullDevelopmentCorpusSHA256 ||
		matcher.SHA256 != artifact.MatcherDevelopmentCorpusSHA256 ||
		system.SystemID != artifact.SystemID ||
		system.Descriptor() != artifact.System {
		return nil, fmt.Errorf(
			"calibration does not bind the supplied development corpus and system",
		)
	}
	canonicalRawSHA, err := ResultsIdentity(rawResults)
	if err != nil {
		return nil, err
	}
	if canonicalRawSHA != artifact.CanonicalRawResultsSHA256 {
		return nil, fmt.Errorf(
			"raw results do not match the calibration artifact",
		)
	}
	thresholds, err := MatcherSpecialistThresholds(
		ProductionThresholds(),
		artifact.ThresholdPPB,
	)
	if err != nil {
		return nil, err
	}
	caseByID := make(map[string]CorpusRecord, len(matcher.Records))
	for _, record := range matcher.Records {
		caseByID[record.CaseID] = record
	}
	out := make([]ResultRecord, len(rawResults))
	for index, raw := range rawResults {
		record, ok := caseByID[raw.CaseID]
		if !ok || raw.Status != ResultStatusOK ||
			raw.MatcherRerank == nil ||
			raw.MatcherRerank.SpecialistAudit == nil {
			return nil, fmt.Errorf(
				"raw result %q is not a complete matcher specialist development row",
				raw.CaseID,
			)
		}
		result := raw
		rerank := *raw.MatcherRerank
		audit := *raw.MatcherRerank.SpecialistAudit
		rerank.SpecialistAudit = &audit
		if audit.SelectedTMDBID > 0 &&
			audit.ScorePPB >= artifact.ThresholdPPB &&
			!matcherCandidatePolicyVeto(rerank.PolicyReason) {
			rerank.Action = MatcherRerankActionAttach
			rerank.TMDBID = audit.SelectedTMDBID
			rerank.PolicyReason = ""
		} else {
			rerank.Action = MatcherRerankActionAbstain
			rerank.TMDBID = 0
			if !matcherCandidatePolicyVeto(rerank.PolicyReason) {
				if audit.SelectedTMDBID == 0 {
					rerank.PolicyReason = "model_declined"
				} else {
					rerank.PolicyReason = "below_confidence"
				}
			}
		}
		result.MatcherRerank = &rerank
		result.RequestContractSHA256, err = RequestContractSHA256(
			record,
			system,
			thresholds,
		)
		if err != nil {
			return nil, err
		}
		marker := MatcherSpecialistDevelopmentExecutionAudit{
			Mode:                             MatcherSpecialistDevelopmentCalibrated,
			DevelopmentClosureManifestSHA256: artifact.DevelopmentClosureManifestSHA256,
			ThresholdPPB:                     artifact.ThresholdPPB,
			DerivedFromResultsSHA256:         artifact.RawResultsArtifactSHA256,
		}
		result.ExecutionAudit.MatcherSpecialistDevelopment = &marker
		if err := result.ValidateWithThresholds(thresholds); err != nil {
			return nil, fmt.Errorf(
				"rethresholded result %q: %w",
				result.CaseID,
				err,
			)
		}
		out[index] = result
	}
	if len(out) != len(matcher.Records) {
		return nil, fmt.Errorf("raw results do not cover every matcher development case")
	}
	return canonicalResults(out), nil
}

func ValidateMatcherSpecialistDevelopmentClosure(
	development Corpus,
	manifest GoldDevelopmentClosureManifest,
	manifestSHA256 string,
) error {
	if err := validateSHA256(
		"development_closure_manifest_sha256",
		manifestSHA256,
	); err != nil {
		return err
	}
	canonical, err := NewCorpus(development.Records)
	if err != nil {
		return err
	}
	if development.SHA256 != canonical.SHA256 ||
		manifest.SchemaVersion != SchemaVersion ||
		manifest.Status != GoldDevelopmentClosureStatus ||
		manifest.SelectionAlgorithmID !=
			GoldDevelopmentClosureAlgorithmID ||
		manifest.DevelopmentCorpusSHA256 != canonical.SHA256 ||
		manifest.DevelopmentCases != len(canonical.Records) {
		return fmt.Errorf(
			"development closure does not bind the exact full development corpus",
		)
	}
	if err := validateIdentifier("plan_id", manifest.PlanID); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"plan_sha256":                         manifest.PlanSHA256,
		"candidate_corpus_sha256":             manifest.CandidateCorpusSHA256,
		"candidate_freeze_manifest_sha256":    manifest.CandidateFreezeManifestSHA256,
		"candidate_development_corpus_sha256": manifest.CandidateDevelopmentSHA256,
		"development_corpus_sha256":           manifest.DevelopmentCorpusSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if err := ValidateGoldCorpus(canonical); err != nil {
		return err
	}
	matcher, err := FilterCorpus(canonical, TaskMatcherRerank, 0)
	if err != nil {
		return err
	}
	groups := make(map[string]struct{}, len(matcher.Records))
	suites := make(map[string]int)
	for _, record := range matcher.Records {
		if _, duplicate := groups[record.GroupID]; duplicate {
			return fmt.Errorf(
				"matcher development group %q has multiple representatives",
				record.GroupID,
			)
		}
		groups[record.GroupID] = struct{}{}
		suite, err := scoringPrimarySuite(record)
		if err != nil {
			return err
		}
		suites[suite]++
	}
	if suites["natural"] == 0 || suites["safety"] == 0 ||
		len(suites) != 2 {
		return fmt.Errorf(
			"matcher development calibration requires natural and safety suites",
		)
	}
	summaryFound := false
	for _, summary := range manifest.Tasks {
		if summary.Task != TaskMatcherRerank {
			continue
		}
		if summaryFound ||
			summary.SelectedCases != len(matcher.Records) ||
			summary.SelectedGroups != len(groups) {
			return fmt.Errorf(
				"development closure matcher task summary is inconsistent",
			)
		}
		summaryFound = true
	}
	if !summaryFound {
		return fmt.Errorf("development closure has no matcher_rerank summary")
	}
	return nil
}

func ValidateMatcherSpecialistDevelopmentPrivacy(
	development Corpus,
	closure GoldDevelopmentClosureManifest,
	closureSHA256 string,
	freeze CorpusFreezeManifest,
	freezeSHA256 string,
	sidecar ProductionPrivacySidecar,
	sidecarSHA256 string,
) error {
	if err := ValidateMatcherSpecialistDevelopmentClosure(
		development,
		closure,
		closureSHA256,
	); err != nil {
		return err
	}
	if freezeSHA256 != closure.CandidateFreezeManifestSHA256 ||
		freeze.Status != ProductionCorpusFreezeStatus ||
		!freeze.ProductionSourceExporterVerified ||
		freeze.PlanID != closure.PlanID ||
		freeze.PlanSHA256 != closure.PlanSHA256 ||
		freeze.CandidateCorpusSHA256 != closure.CandidateCorpusSHA256 ||
		freeze.DevelopmentCorpusSHA256 !=
			closure.CandidateDevelopmentSHA256 ||
		freeze.PrivacySidecarSHA256 != sidecarSHA256 ||
		sidecar.PlanID != closure.PlanID ||
		sidecar.PlanSHA256 != closure.PlanSHA256 ||
		sidecar.CandidateCorpusSHA256 != closure.CandidateCorpusSHA256 {
		return fmt.Errorf(
			"development privacy artifacts do not match the exact closure/freeze chain",
		)
	}
	entries := make(map[string]ProductionPrivacySidecarEntry, len(sidecar.Entries))
	for _, entry := range sidecar.Entries {
		entries[entry.CaseID] = entry
	}
	for _, record := range development.Records {
		entry, ok := entries[record.CaseID]
		if !ok || entry.Task != record.Task {
			return fmt.Errorf(
				"production privacy sidecar does not cover every development case",
			)
		}
	}
	return nil
}

func validateMatcherSpecialistCalibrationInput(
	input MatcherSpecialistCalibrationInput,
) (Corpus, map[string]ResultRecord, string, error) {
	if err := ValidateMatcherSpecialistDevelopmentClosure(
		input.Development,
		input.DevelopmentClosure,
		input.DevelopmentClosureSHA256,
	); err != nil {
		return Corpus{}, nil, "", err
	}
	if err := input.System.Validate(); err != nil {
		return Corpus{}, nil, "", err
	}
	if input.System.APIKind != APIKindEmbedding ||
		input.System.PromptVersion != MatcherSpecialistAlgorithmID {
		return Corpus{}, nil, "", fmt.Errorf(
			"calibration requires the hosted matcher embedding specialist",
		)
	}
	for name, value := range map[string]string{
		"system_manifest_sha256":      input.SystemManifestSHA256,
		"route_snapshot_sha256":       input.RouteSnapshotSHA256,
		"evaluator_build_sha256":      input.EvaluatorBuildSHA256,
		"raw_results_artifact_sha256": input.RawResultsArtifactSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return Corpus{}, nil, "", err
		}
	}
	matcher, err := FilterCorpus(
		input.Development,
		TaskMatcherRerank,
		0,
	)
	if err != nil {
		return Corpus{}, nil, "", err
	}
	zeroThresholds, err := MatcherSpecialistThresholds(
		ProductionThresholds(),
		0,
	)
	if err != nil {
		return Corpus{}, nil, "", err
	}
	if len(input.RawResults) != len(matcher.Records) {
		return Corpus{}, nil, "", fmt.Errorf(
			"raw results must cover every matcher development case exactly once",
		)
	}
	if err := ValidateResultsWithThresholds(
		input.RawResults,
		zeroThresholds,
	); err != nil {
		return Corpus{}, nil, "", err
	}
	canonicalResultsSHA, err := ResultsIdentity(input.RawResults)
	if err != nil {
		return Corpus{}, nil, "", err
	}
	caseByID := make(map[string]CorpusRecord, len(matcher.Records))
	for _, record := range matcher.Records {
		caseByID[record.CaseID] = record
	}
	resultByCase := make(map[string]ResultRecord, len(input.RawResults))
	for _, result := range input.RawResults {
		record, ok := caseByID[result.CaseID]
		if !ok || result.Status != ResultStatusOK ||
			result.CorpusSHA256 != matcher.SHA256 ||
			result.System != input.System.Descriptor() ||
			result.EvaluatorBuildSHA256 != input.EvaluatorBuildSHA256 ||
			result.ExecutionAudit.ManifestSHA256 !=
				input.SystemManifestSHA256 ||
			result.ExecutionAudit.Route.SnapshotSHA256 !=
				input.RouteSnapshotSHA256 ||
			result.MatcherRerank == nil ||
			result.MatcherRerank.SpecialistAudit == nil {
			return Corpus{}, nil, "", fmt.Errorf(
				"raw result %q is not bound to the exact development execution",
				result.CaseID,
			)
		}
		marker := result.ExecutionAudit.MatcherSpecialistDevelopment
		if marker == nil ||
			marker.Mode != MatcherSpecialistDevelopmentRaw ||
			marker.ThresholdPPB != 0 ||
			marker.DevelopmentClosureManifestSHA256 !=
				input.DevelopmentClosureSHA256 {
			return Corpus{}, nil, "", fmt.Errorf(
				"raw result %q lacks the exact calibration-only execution marker",
				result.CaseID,
			)
		}
		expectedContract, err := RequestContractSHA256(
			record,
			input.System,
			zeroThresholds,
		)
		if err != nil ||
			result.RequestContractSHA256 != expectedContract {
			return Corpus{}, nil, "", fmt.Errorf(
				"raw result %q has a different threshold-zero request contract",
				result.CaseID,
			)
		}
		audit := result.MatcherRerank.SpecialistAudit
		if err := validateMatcherSpecialistCompletion(
			record.MatcherRerank,
			input.System,
			audit.SelectedTMDBID,
			result.MatcherRerank.Confidence,
			*audit,
		); err != nil {
			return Corpus{}, nil, "", fmt.Errorf(
				"raw result %q specialist audit: %w",
				result.CaseID,
				err,
			)
		}
		if _, duplicate := resultByCase[result.CaseID]; duplicate {
			return Corpus{}, nil, "", fmt.Errorf(
				"raw results contain duplicate case %q",
				result.CaseID,
			)
		}
		resultByCase[result.CaseID] = result
	}
	return matcher, resultByCase, canonicalResultsSHA, nil
}

func matcherSpecialistCalibrationMetrics(
	matcher Corpus,
	resultByCase map[string]ResultRecord,
	thresholdPPB int64,
) (
	MatcherSpecialistCalibrationMetrics,
	map[string]MatcherSpecialistCalibrationMetrics,
	error,
) {
	totalGroups := make(map[string]struct{})
	suiteGroups := make(map[string]map[string]struct{})
	suiteMetrics := make(map[string]MatcherSpecialistCalibrationMetrics)
	total := MatcherSpecialistCalibrationMetrics{
		ThresholdPPB: thresholdPPB,
		Cases:        len(matcher.Records),
	}
	for _, record := range matcher.Records {
		result, ok := resultByCase[record.CaseID]
		if !ok {
			return MatcherSpecialistCalibrationMetrics{}, nil,
				fmt.Errorf("missing raw result for case %q", record.CaseID)
		}
		suite, err := scoringPrimarySuite(record)
		if err != nil {
			return MatcherSpecialistCalibrationMetrics{}, nil, err
		}
		metrics := suiteMetrics[suite]
		metrics.ThresholdPPB = thresholdPPB
		metrics.Cases++
		totalGroups[record.GroupID] = struct{}{}
		if suiteGroups[suite] == nil {
			suiteGroups[suite] = make(map[string]struct{})
		}
		suiteGroups[suite][record.GroupID] = struct{}{}
		audit := result.MatcherRerank.SpecialistAudit
		attach := audit.SelectedTMDBID > 0 &&
			audit.ScorePPB >= thresholdPPB &&
			!matcherCandidatePolicyVeto(result.MatcherRerank.PolicyReason)
		if !attach {
			total.Abstentions++
			metrics.Abstentions++
		} else {
			total.AttachmentAttempts++
			metrics.AttachmentAttempts++
			if matcherSpecialistSelectionCorrect(
				record,
				audit.SelectedTMDBID,
			) {
				total.CorrectAttachments++
				metrics.CorrectAttachments++
			} else {
				total.WrongAttachments++
				metrics.WrongAttachments++
			}
		}
		suiteMetrics[suite] = metrics
	}
	total.Groups = len(totalGroups)
	for suite, metrics := range suiteMetrics {
		metrics.Groups = len(suiteGroups[suite])
		suiteMetrics[suite] = metrics
	}
	return total, suiteMetrics, nil
}

func matcherSpecialistSelectionCorrect(
	record CorpusRecord,
	selectedTMDBID int64,
) bool {
	if record.MatcherRerank == nil || selectedTMDBID <= 0 {
		return false
	}
	for _, acceptable := range record.MatcherRerank.Expected.AcceptableTMDBIDs {
		if selectedTMDBID == acceptable {
			return true
		}
	}
	return false
}

func validateMatcherSpecialistMetric(
	name string,
	metric MatcherSpecialistCalibrationMetrics,
	cases int,
	groups int,
	thresholdPPB int64,
) error {
	if metric.ThresholdPPB != thresholdPPB ||
		metric.Cases != cases ||
		metric.Groups != groups ||
		cases <= 0 || groups <= 0 || groups > cases ||
		metric.AttachmentAttempts < 0 ||
		metric.CorrectAttachments < 0 ||
		metric.WrongAttachments < 0 ||
		metric.Abstentions < 0 ||
		metric.CorrectAttachments+metric.WrongAttachments !=
			metric.AttachmentAttempts ||
		metric.AttachmentAttempts+metric.Abstentions != metric.Cases {
		return fmt.Errorf("%s calibration metrics are inconsistent", name)
	}
	return nil
}
