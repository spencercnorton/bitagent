package llmeval

import (
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxIDLength       = 192
	maxTextLength     = 4096
	maxOverviewLength = 8192
	maxSlices         = 64
	maxCandidates     = 100
	maxPromptFiles    = 5

	// ProtocolSmokeCorpusSHA256 is the only synthetic corpus permitted to use
	// the hosted-run privacy/gold exemption. It binds that narrow compatibility
	// path to the checked-in eight-case, source-free protocol fixture.
	ProtocolSmokeCorpusSHA256 = "99629ff5cc024e9203fb79df8e9ab68a48f13f87f8f32763c6191d7aff1c0acb"
)

var (
	identifierRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
	tagRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+/-]*$`)
	secretREs    = []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`glpat-[A-Za-z0-9_.-]{16,}`),
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{12,}`),
		regexp.MustCompile(`(?i)\b(?:api[_-]?key|token|secret|password)\s*[:=]\s*["']?[A-Za-z0-9._~+/-]{8,}`),
	}
)

// ValidateCorpus validates every record and corpus-wide identity constraint.
func ValidateCorpus(records []CorpusRecord) error {
	if len(records) == 0 {
		return fmt.Errorf("corpus is empty")
	}

	seen := make(map[string]struct{}, len(records))
	for i := range records {
		if err := records[i].Validate(); err != nil {
			return fmt.Errorf("record %d: %w", i+1, err)
		}
		if _, exists := seen[records[i].CaseID]; exists {
			return fmt.Errorf("record %d: duplicate case_id %q", i+1, records[i].CaseID)
		}
		seen[records[i].CaseID] = struct{}{}
	}

	return nil
}

// Validate checks one self-contained corpus record.
func (r CorpusRecord) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version: got %d, want %d", r.SchemaVersion, SchemaVersion)
	}
	if err := validateIdentifier("case_id", r.CaseID); err != nil {
		return err
	}
	if !r.Task.valid() {
		return fmt.Errorf("task: unsupported value %q", r.Task)
	}
	if err := validateIdentifier("group_id", r.GroupID); err != nil {
		return err
	}
	if len(r.SliceIDs) == 0 {
		return fmt.Errorf("slice_ids: at least one slice is required")
	}
	if len(r.SliceIDs) > maxSlices {
		return fmt.Errorf("slice_ids: %d exceeds maximum %d", len(r.SliceIDs), maxSlices)
	}

	seenSlices := make(map[string]struct{}, len(r.SliceIDs))
	for i, id := range r.SliceIDs {
		if err := validateIdentifier(fmt.Sprintf("slice_ids[%d]", i), id); err != nil {
			return err
		}
		if _, exists := seenSlices[id]; exists {
			return fmt.Errorf("slice_ids[%d]: duplicate %q", i, id)
		}
		seenSlices[id] = struct{}{}
	}

	if err := r.Label.Validate(); err != nil {
		return fmt.Errorf("label: %w", err)
	}
	if r.Label.HumanReviewProof != nil {
		expectedWorkflow := ReviewWorkflowContentJunk
		if r.Task == TaskMatcherExtract || r.Task == TaskMatcherRerank {
			expectedWorkflow = ReviewWorkflowMatcher
		}
		if r.Label.HumanReviewProof.Workflow != expectedWorkflow {
			return fmt.Errorf(
				"label.human_review_proof.workflow: got %q, want %q for task %q",
				r.Label.HumanReviewProof.Workflow,
				expectedWorkflow,
				r.Task,
			)
		}
	}
	if r.PrivacyAttestation == nil {
		if r.Label.Provenance != LabelProvenanceSynthetic {
			return fmt.Errorf(
				"privacy_attestation: required for non-synthetic provenance %q",
				r.Label.Provenance,
			)
		}
	} else if err := r.PrivacyAttestation.Validate(); err != nil {
		return fmt.Errorf("privacy_attestation: %w", err)
	}

	payloads := boolInt(r.MatcherExtract != nil) +
		boolInt(r.MatcherRerank != nil) +
		boolInt(r.ContentFilter != nil) +
		boolInt(r.JunkPurge != nil)
	if payloads != 1 {
		return fmt.Errorf("task payload: exactly one payload is required, got %d", payloads)
	}
	if err := r.validateSamplingCandidatePlaceholder(); err != nil {
		return fmt.Errorf("label: %w", err)
	}

	switch r.Task {
	case TaskMatcherExtract:
		if r.MatcherExtract == nil {
			return fmt.Errorf("task payload: matcher_extract is required")
		}
		return r.MatcherExtract.Validate()
	case TaskMatcherRerank:
		if r.MatcherRerank == nil {
			return fmt.Errorf("task payload: matcher_rerank is required")
		}
		return r.MatcherRerank.Validate()
	case TaskContentFilter:
		if r.ContentFilter == nil {
			return fmt.Errorf("task payload: contentfilter is required")
		}
		return r.ContentFilter.Validate()
	case TaskJunkPurge:
		if r.JunkPurge == nil {
			return fmt.Errorf("task payload: junkpurge is required")
		}
		return r.JunkPurge.Validate(r.Tier)
	default:
		return fmt.Errorf("task: unsupported value %q", r.Task)
	}
}

func (m LabelMetadata) Validate() error {
	switch m.Provenance {
	case LabelProvenanceHumanReview,
		LabelProvenanceOperatorOutcome,
		LabelProvenanceArrEvidence,
		LabelProvenanceProductionTeacher,
		LabelProvenanceConsensus,
		LabelProvenanceSynthetic,
		LabelProvenanceSamplingCandidate:
	default:
		return fmt.Errorf("provenance: unsupported value %q", m.Provenance)
	}

	switch m.Strength {
	case LabelStrengthGold,
		LabelStrengthStrong,
		LabelStrengthWeak,
		LabelStrengthTeacher,
		LabelStrengthUnreviewed:
	default:
		return fmt.Errorf("strength: unsupported value %q", m.Strength)
	}

	if err := validateTag("policy_version", m.PolicyVersion); err != nil {
		return err
	}
	if m.SourceRef != "" {
		if err := validateIdentifier("source_ref", m.SourceRef); err != nil {
			return err
		}
	}
	if m.ReviewerCount < 0 {
		return fmt.Errorf("reviewer_count: must be non-negative")
	}
	if m.Provenance == LabelProvenanceHumanReview && m.ReviewerCount < 1 {
		return fmt.Errorf("reviewer_count: human_review requires at least one reviewer")
	}
	if m.Provenance == LabelProvenanceHumanReview {
		if m.HumanReviewProof == nil {
			return fmt.Errorf("human_review_proof: human_review requires immutable proof")
		}
		if err := m.HumanReviewProof.Validate(); err != nil {
			return fmt.Errorf("human_review_proof: %w", err)
		}
		if m.PolicyVersion != m.HumanReviewProof.PolicyVersion {
			return fmt.Errorf(
				"policy_version: %q does not match human_review_proof %q",
				m.PolicyVersion,
				m.HumanReviewProof.PolicyVersion,
			)
		}
	} else if m.HumanReviewProof != nil {
		return fmt.Errorf("human_review_proof: allowed only for human_review provenance")
	}
	if m.Provenance == LabelProvenanceSamplingCandidate {
		if m.Strength != LabelStrengthUnreviewed {
			return fmt.Errorf(
				"strength: sampling_candidate requires %q",
				LabelStrengthUnreviewed,
			)
		}
		if m.PolicyVersion != CandidatePlaceholderPolicyVersion {
			return fmt.Errorf(
				"policy_version: sampling_candidate requires %q",
				CandidatePlaceholderPolicyVersion,
			)
		}
		if m.ReviewerCount != 0 {
			return fmt.Errorf(
				"reviewer_count: sampling_candidate must be unreviewed",
			)
		}
	} else if m.Strength == LabelStrengthUnreviewed {
		return fmt.Errorf(
			"strength: %q is allowed only for sampling_candidate",
			LabelStrengthUnreviewed,
		)
	}

	return nil
}

// ValidateGoldCorpus is the shared hosted/scoring/comparison provenance gate.
// Synthetic protocol fixtures remain valid, but a production-derived corpus
// must consist exclusively of independently reviewed human_review/gold rows.
// Mixed synthetic/production corpora and every weaker production label fail.
// CLI entry points additionally bind production gold to its exact closure
// manifest before invoking these package-level operations.
func ValidateGoldCorpus(corpus Corpus) error {
	synthetic := 0
	for i, record := range corpus.Records {
		if record.Label.Provenance == LabelProvenanceSamplingCandidate ||
			record.Label.Strength == LabelStrengthUnreviewed {
			return fmt.Errorf(
				"record %d (%q): unreviewed sampling candidate cannot be executed or scored",
				i+1,
				record.CaseID,
			)
		}
		if record.Label.Provenance == LabelProvenanceSynthetic {
			synthetic++
			continue
		}
		if record.Label.Provenance != LabelProvenanceHumanReview ||
			record.Label.Strength != LabelStrengthGold ||
			record.Label.HumanReviewProof == nil ||
			(record.Label.ReviewerCount != 2 &&
				record.Label.ReviewerCount != 3) {
			return fmt.Errorf(
				"record %d (%q): production-derived execution and scoring require independently reviewed human_review/gold with two or three reviewers",
				i+1,
				record.CaseID,
			)
		}
	}
	if synthetic != 0 && synthetic != len(corpus.Records) {
		return fmt.Errorf(
			"corpus mixes synthetic and production-derived records",
		)
	}
	return nil
}

// ValidateSyntheticProtocolCorpus prevents arbitrary production text from
// bypassing privacy and closure gates merely by self-declaring synthetic
// provenance.
func ValidateSyntheticProtocolCorpus(corpus Corpus) error {
	canonical, err := NewCorpus(corpus.Records)
	if err != nil {
		return err
	}
	if corpus.SHA256 != "" && corpus.SHA256 != canonical.SHA256 {
		return fmt.Errorf("synthetic protocol corpus identity mismatch")
	}
	for i, record := range canonical.Records {
		if record.Label.Provenance != LabelProvenanceSynthetic {
			return fmt.Errorf(
				"synthetic protocol corpus record %d (%q) is not synthetic",
				i+1,
				record.CaseID,
			)
		}
	}
	if canonical.SHA256 != ProtocolSmokeCorpusSHA256 {
		return fmt.Errorf(
			"synthetic hosted execution is restricted to checked-in protocol smoke corpus %s",
			ProtocolSmokeCorpusSHA256,
		)
	}
	return nil
}

func (r CorpusRecord) validateSamplingCandidatePlaceholder() error {
	if r.Label.Provenance != LabelProvenanceSamplingCandidate {
		return nil
	}
	switch r.Task {
	case TaskMatcherExtract:
		if r.MatcherExtract == nil ||
			len(r.MatcherExtract.Expected.Acceptable) != 0 ||
			!r.MatcherExtract.Expected.AllowAbstain {
			return fmt.Errorf(
				"sampling_candidate matcher_extract expected must be the abstain-only placeholder",
			)
		}
	case TaskMatcherRerank:
		if r.MatcherRerank == nil ||
			len(r.MatcherRerank.Expected.AcceptableTMDBIDs) != 0 ||
			!r.MatcherRerank.Expected.AllowAbstain {
			return fmt.Errorf(
				"sampling_candidate matcher_rerank expected must be the abstain-only placeholder",
			)
		}
	case TaskContentFilter:
		if r.ContentFilter == nil ||
			r.ContentFilter.Expected.Language != "" ||
			!r.ContentFilter.Expected.AllowAbstain {
			return fmt.Errorf(
				"sampling_candidate contentfilter expected must be the abstain-only placeholder",
			)
		}
	case TaskJunkPurge:
		// The placeholder is `ambiguous` deliberately: it is the one content
		// class that can never be gold, so a sampling candidate that leaks into
		// a gold tier fails JunkPurgeExpected.Validate immediately instead of
		// being scored as an abstain.
		if r.JunkPurge == nil ||
			r.JunkPurge.Expected.ContentClass != JunkClassAmbiguous ||
			r.JunkPurge.Expected.Disposition != JunkDispositionAbstain {
			return fmt.Errorf(
				"sampling_candidate junkpurge expected must be the ambiguous/abstain placeholder",
			)
		}
		if r.Tier != GoldTierSamplingCandidate {
			return fmt.Errorf(
				"sampling_candidate junkpurge must carry tier %q, got %q",
				GoldTierSamplingCandidate, r.Tier,
			)
		}
	default:
		return fmt.Errorf("sampling_candidate task %q is unsupported", r.Task)
	}
	return nil
}

// Validate checks that a production-source privacy attestation carries the
// exact bounded status and immutable source snapshot identity required by
// hosted evaluation.
func (a SourcePrivacyAttestation) Validate() error {
	if err := validateIdentifier("plan_id", a.PlanID); err != nil {
		return err
	}
	if err := validateSHA256("source_snapshot_sha256", a.SourceSnapshotSHA256); err != nil {
		return err
	}
	if a.Status != PrivacyVerifiedPostRulePublic {
		return fmt.Errorf("status: unsupported value %q", a.Status)
	}
	return nil
}

// ValidateHostedCorpusPrivacy is an explicit hosted-run preflight. Corpus
// validation already enforces the same rule; this named check makes the
// fail-closed boundary available to callers before any provider request.
func ValidateHostedCorpusPrivacy(corpus Corpus) error {
	for i, record := range corpus.Records {
		if record.Label.Provenance == LabelProvenanceSynthetic {
			continue
		}
		if record.PrivacyAttestation == nil {
			return fmt.Errorf(
				"hosted corpus record %d (%q): privacy_attestation is required",
				i+1,
				record.CaseID,
			)
		}
		if err := record.PrivacyAttestation.Validate(); err != nil {
			return fmt.Errorf(
				"hosted corpus record %d (%q): privacy_attestation: %w",
				i+1,
				record.CaseID,
				err,
			)
		}
	}
	return nil
}

func (c MatcherExtractCase) Validate() error {
	if err := validateText("matcher_extract.input.release_name", c.Input.ReleaseName, maxTextLength); err != nil {
		return err
	}
	if len(c.Input.FilePaths) > maxPromptFiles {
		return fmt.Errorf(
			"matcher_extract.input.file_paths: %d exceeds prompt maximum %d",
			len(c.Input.FilePaths),
			maxPromptFiles,
		)
	}
	for i, path := range c.Input.FilePaths {
		if err := validateText(
			fmt.Sprintf("matcher_extract.input.file_paths[%d]", i),
			path,
			maxTextLength,
		); err != nil {
			return err
		}
	}

	if len(c.Expected.Acceptable) == 0 && !c.Expected.AllowAbstain {
		return fmt.Errorf("matcher_extract.expected: acceptable output or allow_abstain is required")
	}
	seenExpected := make(map[MatcherExtraction]struct{}, len(c.Expected.Acceptable))
	for i := range c.Expected.Acceptable {
		if err := c.Expected.Acceptable[i].Validate(); err != nil {
			return fmt.Errorf("matcher_extract.expected.acceptable[%d]: %w", i, err)
		}
		if _, exists := seenExpected[c.Expected.Acceptable[i]]; exists {
			return fmt.Errorf("matcher_extract.expected.acceptable[%d]: duplicate output", i)
		}
		seenExpected[c.Expected.Acceptable[i]] = struct{}{}
	}

	return nil
}

func (e MatcherExtraction) Validate() error {
	if err := validateText("title", e.Title, 1024); err != nil {
		return err
	}
	if e.Year != 0 && (e.Year < 1800 || e.Year > 2200) {
		return fmt.Errorf("year: %d is outside 1800..2200 or 0", e.Year)
	}
	if !e.Type.valid() {
		return fmt.Errorf("type: unsupported value %q", e.Type)
	}
	if e.Season < 0 || e.Season > 100000 {
		return fmt.Errorf("season: %d is outside 0..100000", e.Season)
	}
	if e.Episode < 0 || e.Episode > 1000000 {
		return fmt.Errorf("episode: %d is outside 0..1000000", e.Episode)
	}
	if !e.English.valid() {
		return fmt.Errorf("english: unsupported value %q", e.English)
	}

	return nil
}

func (c MatcherRerankCase) Validate() error {
	if err := validateText("matcher_rerank.input.release_name", c.Input.ReleaseName, maxTextLength); err != nil {
		return err
	}
	if c.Input.ParsedTitle != "" {
		if err := validateText("matcher_rerank.input.parsed_title", c.Input.ParsedTitle, 1024); err != nil {
			return err
		}
	}
	if err := c.Input.Extraction.Validate(); err != nil {
		return fmt.Errorf("matcher_rerank.input.extraction: %w", err)
	}
	if c.Input.EffectiveMediaType != "" &&
		!c.Input.EffectiveMediaType.valid() {
		return fmt.Errorf(
			"matcher_rerank.input.effective_media_type: unsupported value %q",
			c.Input.EffectiveMediaType,
		)
	}
	if len(c.Input.Candidates) == 0 {
		return fmt.Errorf("matcher_rerank.input.candidates: at least one candidate is required")
	}
	if len(c.Input.Candidates) > maxCandidates {
		return fmt.Errorf(
			"matcher_rerank.input.candidates: %d exceeds maximum %d",
			len(c.Input.Candidates),
			maxCandidates,
		)
	}
	if c.Input.CandidateSource != "" &&
		c.Input.CandidateSource != "local" &&
		c.Input.CandidateSource != "api" {
		return fmt.Errorf(
			"matcher_rerank.input.candidate_source: got %q, want local or api",
			c.Input.CandidateSource,
		)
	}

	candidateIDs := make(map[int64]struct{}, len(c.Input.Candidates))
	for i, candidate := range c.Input.Candidates {
		if candidate.TMDBID <= 0 {
			return fmt.Errorf("matcher_rerank.input.candidates[%d].tmdb_id: must be positive", i)
		}
		if _, exists := candidateIDs[candidate.TMDBID]; exists {
			return fmt.Errorf(
				"matcher_rerank.input.candidates[%d].tmdb_id: duplicate %d",
				i,
				candidate.TMDBID,
			)
		}
		candidateIDs[candidate.TMDBID] = struct{}{}
		if !candidate.Type.valid() {
			return fmt.Errorf(
				"matcher_rerank.input.candidates[%d].type: unsupported value %q",
				i,
				candidate.Type,
			)
		}
		if err := validateText(
			fmt.Sprintf("matcher_rerank.input.candidates[%d].title", i),
			candidate.Title,
			1024,
		); err != nil {
			return err
		}
		if candidate.OriginalTitle != "" {
			if err := validateText(
				fmt.Sprintf("matcher_rerank.input.candidates[%d].original_title", i),
				candidate.OriginalTitle,
				1024,
			); err != nil {
				return err
			}
		}
		for j, altTitle := range candidate.AltTitles {
			if err := validateText(
				fmt.Sprintf("matcher_rerank.input.candidates[%d].alt_titles[%d]", i, j),
				altTitle,
				1024,
			); err != nil {
				return err
			}
		}
		if candidate.Year != 0 && (candidate.Year < 1800 || candidate.Year > 2200) {
			return fmt.Errorf(
				"matcher_rerank.input.candidates[%d].year: %d is outside 1800..2200 or 0",
				i,
				candidate.Year,
			)
		}
		if candidate.Overview != "" {
			if err := validateText(
				fmt.Sprintf("matcher_rerank.input.candidates[%d].overview", i),
				candidate.Overview,
				maxOverviewLength,
			); err != nil {
				return err
			}
		}
	}

	if len(c.Expected.AcceptableTMDBIDs) == 0 && !c.Expected.AllowAbstain {
		return fmt.Errorf("matcher_rerank.expected: acceptable id or allow_abstain is required")
	}

	expectedIDs := make(map[int64]struct{}, len(c.Expected.AcceptableTMDBIDs))
	for i, id := range c.Expected.AcceptableTMDBIDs {
		if id <= 0 {
			return fmt.Errorf("matcher_rerank.expected.acceptable_tmdb_ids[%d]: must be positive", i)
		}
		if _, exists := candidateIDs[id]; !exists {
			return fmt.Errorf(
				"matcher_rerank.expected.acceptable_tmdb_ids[%d]: %d is not in candidates",
				i,
				id,
			)
		}
		if _, exists := expectedIDs[id]; exists {
			return fmt.Errorf(
				"matcher_rerank.expected.acceptable_tmdb_ids[%d]: duplicate %d",
				i,
				id,
			)
		}
		expectedIDs[id] = struct{}{}
	}

	return nil
}

func (c ContentFilterCase) Validate() error {
	if err := validateText("contentfilter.input.title", c.Input.Title, maxTextLength); err != nil {
		return err
	}
	if c.Expected.AllowAbstain {
		if c.Expected.Language != "" {
			return fmt.Errorf(
				"contentfilter.expected: allow_abstain must omit language",
			)
		}
		return nil
	}
	switch c.Expected.Language {
	case LanguageEnglish, LanguageNonEnglish:
		return nil
	default:
		return fmt.Errorf(
			"contentfilter.expected.language: unsupported value %q",
			c.Expected.Language,
		)
	}
}

func (c JunkPurgeCase) Validate(tier GoldTier) error {
	if err := validateText("junkpurge.input.torrent_name", c.Input.TorrentName, maxTextLength); err != nil {
		return err
	}
	return c.Expected.Validate(tier)
}

// ValidateResults checks independent result-record contracts and duplicate
// system/case keys. Corpus cross-checks are performed by Score.
func ValidateResults(results []ResultRecord) error {
	seen := make(map[string]struct{}, len(results))
	type executionIdentity struct {
		manifest         string
		campaignBound    bool
		campaignSHA256   string
		campaignRunID    string
		snapshot         string
		snapshotFetched  string
		outputAllowance  int64
		returnedModel    string
		returnedProvider string
		batchID          string
		batchInput       string
		batchEndpoint    string
		batchMultiplier  int64
	}
	executionBySystem := make(map[string]executionIdentity)
	for i := range results {
		if err := results[i].Validate(); err != nil {
			return fmt.Errorf("result %d: %w", i+1, err)
		}
		key := results[i].System.SystemID + "\x00" + results[i].CaseID
		if _, exists := seen[key]; exists {
			return fmt.Errorf(
				"result %d: duplicate system/case key %q/%q",
				i+1,
				results[i].System.SystemID,
				results[i].CaseID,
			)
		}
		seen[key] = struct{}{}

		audit := results[i].ExecutionAudit
		current := executionIdentity{
			manifest:        audit.ManifestSHA256,
			snapshot:        audit.Route.SnapshotSHA256,
			snapshotFetched: audit.Route.SnapshotFetchedAt,
			outputAllowance: audit.UncappedOutputTokenAllowance,
		}
		if audit.Campaign != nil {
			current.campaignBound = true
			current.campaignSHA256 = audit.Campaign.CampaignSHA256
			current.campaignRunID = audit.Campaign.RunID
		}
		if audit.OpenAIBatch != nil {
			current.batchID = audit.OpenAIBatch.BatchID
			current.batchInput = audit.OpenAIBatch.InputFileSHA256
			current.batchEndpoint = audit.OpenAIBatch.Endpoint
			current.batchMultiplier = audit.OpenAIBatch.PricingMultiplierPPM
		}
		if audit.Route.Verified {
			current.returnedModel = normalizeRouteModel(
				audit.Route.ReturnedModel,
			)
			current.returnedProvider = normalizeRouteProvider(
				audit.Route.ReturnedProvider,
			)
		}
		prior, exists := executionBySystem[results[i].System.SystemID]
		if !exists {
			executionBySystem[results[i].System.SystemID] = current
			continue
		}
		if current.manifest != prior.manifest ||
			current.campaignBound != prior.campaignBound ||
			current.campaignSHA256 != prior.campaignSHA256 ||
			current.campaignRunID != prior.campaignRunID ||
			current.snapshot != prior.snapshot ||
			current.snapshotFetched != prior.snapshotFetched ||
			current.outputAllowance != prior.outputAllowance ||
			current.batchID != prior.batchID ||
			current.batchInput != prior.batchInput ||
			current.batchEndpoint != prior.batchEndpoint ||
			current.batchMultiplier != prior.batchMultiplier {
			return fmt.Errorf(
				"result %d: mixed execution artifact identities for system %q",
				i+1,
				results[i].System.SystemID,
			)
		}
		if current.returnedModel == "" {
			continue
		}
		if prior.returnedModel == "" {
			prior.returnedModel = current.returnedModel
			prior.returnedProvider = current.returnedProvider
			executionBySystem[results[i].System.SystemID] = prior
			continue
		}
		if current.returnedModel != prior.returnedModel ||
			current.returnedProvider != prior.returnedProvider {
			return fmt.Errorf(
				"result %d: mixed concrete routes for system %q",
				i+1,
				results[i].System.SystemID,
			)
		}
	}

	return nil
}

// ValidateResultsWithThresholds applies the exact decision thresholds bound to
// a run after validating the threshold-independent artifact structure. Result
// JSON intentionally stores normalized actions, so callers that possess the
// run/roster contract must use this function to reject threshold tampering.
func ValidateResultsWithThresholds(
	results []ResultRecord,
	thresholds DecisionThresholds,
) error {
	if err := thresholds.Validate(); err != nil {
		return fmt.Errorf("decision thresholds: %w", err)
	}
	if err := ValidateResults(results); err != nil {
		return err
	}
	for i := range results {
		if err := results[i].ValidateWithThresholds(thresholds); err != nil {
			return fmt.Errorf("result %d: %w", i+1, err)
		}
	}
	return nil
}

// Validate checks one normalized provider-independent result.
func (r ResultRecord) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version: got %d, want %d", r.SchemaVersion, SchemaVersion)
	}
	if err := validateSHA256("corpus_sha256", r.CorpusSHA256); err != nil {
		return err
	}
	if err := validateSHA256("request_contract_sha256", r.RequestContractSHA256); err != nil {
		return err
	}
	if err := validateSHA256("evaluator_build_sha256", r.EvaluatorBuildSHA256); err != nil {
		return err
	}
	if err := validateIdentifier("case_id", r.CaseID); err != nil {
		return err
	}
	if !r.Task.valid() {
		return fmt.Errorf("task: unsupported value %q", r.Task)
	}
	if err := r.System.Validate(); err != nil {
		return fmt.Errorf("system: %w", err)
	}
	if err := r.ExecutionAudit.Validate(r.System.Provider, r.Status); err != nil {
		return fmt.Errorf("execution_audit: %w", err)
	}
	if campaign := r.ExecutionAudit.Campaign; campaign != nil {
		if campaign.SystemID != r.System.SystemID {
			return fmt.Errorf(
				"execution_audit.campaign: system_id differs from result system",
			)
		}
		if campaign.Task != r.Task {
			return fmt.Errorf(
				"execution_audit.campaign: task differs from result task",
			)
		}
		if campaign.Corpus.SHA256 != r.CorpusSHA256 {
			return fmt.Errorf(
				"execution_audit.campaign: corpus identity differs from result",
			)
		}
		if campaign.SystemManifestSHA256 !=
			r.ExecutionAudit.ManifestSHA256 {
			return fmt.Errorf(
				"execution_audit.campaign: manifest identity differs from execution audit",
			)
		}
		if r.System.Provider == "openrouter" {
			if campaign.RouteSnapshot == nil ||
				campaign.RouteSnapshot.SHA256 !=
					r.ExecutionAudit.Route.SnapshotSHA256 {
				return fmt.Errorf(
					"execution_audit.campaign: route snapshot identity differs from execution audit",
				)
			}
		} else if campaign.RouteSnapshot != nil {
			return fmt.Errorf(
				"execution_audit.campaign: route snapshot is valid only for OpenRouter",
			)
		}
	}
	if err := r.Usage.Validate(); err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	if r.ExecutionAudit.CostAccountingVersion == CurrentCostAccountingVersion &&
		r.Usage.CostSource == "" {
		return fmt.Errorf("usage: cost_source is required for new formal results")
	}
	if r.RequestTiming != nil {
		if err := r.RequestTiming.Validate(); err != nil {
			return fmt.Errorf("request_timing: %w", err)
		}
	}

	payloads := boolInt(r.MatcherExtract != nil) +
		boolInt(r.MatcherRerank != nil) +
		boolInt(r.ContentFilter != nil) +
		boolInt(r.JunkPurge != nil)

	switch r.Status {
	case ResultStatusOK:
		if r.ErrorCode != "" {
			return fmt.Errorf("error_code: must be empty when status is ok")
		}
		if payloads != 1 {
			return fmt.Errorf("task result: status ok requires exactly one payload, got %d", payloads)
		}
	case ResultStatusSchemaError, ResultStatusError:
		if payloads != 0 {
			return fmt.Errorf("task result: error status must not carry a task payload")
		}
		if err := validateTag("error_code", r.ErrorCode); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("status: unsupported value %q", r.Status)
	}
	if marker := r.ExecutionAudit.MatcherSpecialistDevelopment; marker != nil {
		if r.Task != TaskMatcherRerank {
			return fmt.Errorf(
				"execution_audit.matcher_specialist_development: valid only for matcher_rerank",
			)
		}
		if r.MatcherRerank == nil || r.MatcherRerank.SpecialistAudit == nil {
			return fmt.Errorf(
				"execution_audit.matcher_specialist_development: ok rows require specialist audit",
			)
		}
	}

	switch r.Task {
	case TaskMatcherExtract:
		if r.MatcherExtract == nil {
			return fmt.Errorf("task result: matcher_extract is required")
		}
		return r.MatcherExtract.Validate()
	case TaskMatcherRerank:
		if r.MatcherRerank == nil {
			return fmt.Errorf("task result: matcher_rerank is required")
		}
		if r.MatcherRerank.SpecialistAudit != nil &&
			r.System.PromptVersion !=
				r.MatcherRerank.SpecialistAudit.AlgorithmID {
			return fmt.Errorf(
				"matcher_rerank.specialist_audit: algorithm differs from system prompt_version",
			)
		}
		return r.MatcherRerank.Validate()
	case TaskContentFilter:
		if r.ContentFilter == nil {
			return fmt.Errorf("task result: contentfilter is required")
		}
		return r.ContentFilter.Validate()
	case TaskJunkPurge:
		if r.JunkPurge == nil {
			return fmt.Errorf("task result: junkpurge is required")
		}
		return r.JunkPurge.Validate()
	default:
		return fmt.Errorf("task: unsupported value %q", r.Task)
	}
}

// ValidateWithThresholds verifies that a structurally valid normalized action
// is reachable under the exact thresholds bound to its request contract.
func (r ResultRecord) ValidateWithThresholds(
	thresholds DecisionThresholds,
) error {
	if err := thresholds.Validate(); err != nil {
		return fmt.Errorf("decision thresholds: %w", err)
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Status != ResultStatusOK {
		return nil
	}
	if marker := r.ExecutionAudit.MatcherSpecialistDevelopment; marker != nil {
		thresholdPPB, err := MatcherSpecialistScorePPB(
			thresholds.MatcherAttachConfidence,
		)
		if err != nil {
			return err
		}
		if marker.ThresholdPPB != thresholdPPB {
			return fmt.Errorf(
				"execution_audit.matcher_specialist_development: threshold differs from run thresholds",
			)
		}
	}
	switch r.Task {
	case TaskMatcherExtract:
		return nil
	case TaskMatcherRerank:
		return r.MatcherRerank.validateWithThresholds(thresholds)
	case TaskContentFilter:
		return r.ContentFilter.validateWithThresholds(thresholds)
	case TaskJunkPurge:
		return r.JunkPurge.validateWithThresholds(thresholds)
	default:
		return fmt.Errorf("task: unsupported value %q", r.Task)
	}
}

func (a ExecutionAudit) Validate(provider string, status ResultStatus) error {
	if err := validateSHA256("manifest_sha256", a.ManifestSHA256); err != nil {
		return err
	}
	if a.Campaign != nil {
		if err := a.Campaign.Validate(); err != nil {
			return fmt.Errorf("campaign: %w", err)
		}
	}
	if a.CostAccountingVersion != 0 &&
		a.CostAccountingVersion != CurrentCostAccountingVersion {
		return fmt.Errorf(
			"cost_accounting_version: got %d, want 0 (legacy) or %d",
			a.CostAccountingVersion,
			CurrentCostAccountingVersion,
		)
	}
	if a.UncappedOutputTokenAllowance < 0 {
		return fmt.Errorf(
			"uncapped_output_token_allowance: must be non-negative",
		)
	}
	route := a.Route
	isOpenRouter := provider == "openrouter"
	if isOpenRouter {
		if err := validateSHA256("route.snapshot_sha256", route.SnapshotSHA256); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339Nano, route.SnapshotFetchedAt); err != nil {
			return fmt.Errorf("route.snapshot_fetched_at: must be RFC3339")
		}
	} else if route.SnapshotSHA256 != "" || route.SnapshotFetchedAt != "" {
		return fmt.Errorf("route: snapshot fields are valid only for OpenRouter")
	}

	switch route.Proof {
	case RouteProofUnverified:
		if route.Verified {
			return fmt.Errorf("route.verified: proof is required")
		}
	case RouteProofDirectResponseModel:
		if isOpenRouter {
			return fmt.Errorf("route.proof: direct proof is invalid for OpenRouter")
		}
		if strings.TrimSpace(route.ReturnedModel) == "" {
			return fmt.Errorf("route.returned_model: required by direct proof")
		}
	case RouteProofRouterMetadata, RouteProofGenerationMetadata:
		if !isOpenRouter {
			return fmt.Errorf("route.proof: router proof is valid only for OpenRouter")
		}
		if strings.TrimSpace(route.ReturnedModel) == "" {
			return fmt.Errorf("route.returned_model: required by router proof")
		}
		if strings.TrimSpace(route.ReturnedProvider) == "" {
			return fmt.Errorf("route.returned_provider: required by router proof")
		}
	default:
		return fmt.Errorf("route.proof: unsupported value %q", route.Proof)
	}
	if route.Verified && route.Proof == RouteProofUnverified {
		return fmt.Errorf("route.verified: cannot be true without proof")
	}
	if !route.Verified && status != ResultStatusError {
		return fmt.Errorf("route.verified: non-error results require verified routing")
	}
	if !isOpenRouter && route.ReturnedProvider != "" {
		return fmt.Errorf("route.returned_provider: invalid for a direct provider")
	}
	if a.OpenAIBatch != nil {
		if provider != "openai" {
			return fmt.Errorf("openai_batch: valid only for direct OpenAI")
		}
		if err := validateIdentifier(
			"openai_batch.batch_id",
			a.OpenAIBatch.BatchID,
		); err != nil {
			return err
		}
		if err := validateSHA256(
			"openai_batch.input_file_sha256",
			a.OpenAIBatch.InputFileSHA256,
		); err != nil {
			return err
		}
		switch a.OpenAIBatch.Endpoint {
		case "/v1/chat/completions", "/v1/responses":
		default:
			return fmt.Errorf("openai_batch.endpoint: unsupported value")
		}
		if err := validateIdentifier(
			"openai_batch.custom_id",
			a.OpenAIBatch.CustomID,
		); err != nil {
			return err
		}
		if a.OpenAIBatch.PricingMultiplierPPM != 500_000 {
			return fmt.Errorf(
				"openai_batch.pricing_multiplier_ppm: must be 500000",
			)
		}
	}
	if a.MatcherSpecialistCalibrationSHA256 != "" {
		if provider != "openrouter" {
			return fmt.Errorf(
				"matcher_specialist_calibration_sha256: valid only for OpenRouter",
			)
		}
		if err := validateSHA256(
			"matcher_specialist_calibration_sha256",
			a.MatcherSpecialistCalibrationSHA256,
		); err != nil {
			return err
		}
	}
	if a.MatcherSpecialistDevelopment != nil {
		if provider != "openrouter" {
			return fmt.Errorf(
				"matcher_specialist_development: valid only for OpenRouter",
			)
		}
		if err := a.MatcherSpecialistDevelopment.Validate(); err != nil {
			return fmt.Errorf("matcher_specialist_development: %w", err)
		}
		if a.MatcherSpecialistCalibrationSHA256 != "" {
			return fmt.Errorf(
				"matcher specialist development evidence cannot bind a holdout calibration artifact",
			)
		}
	}
	for name, value := range map[string]string{
		"route.returned_model":    route.ReturnedModel,
		"route.returned_provider": route.ReturnedProvider,
	} {
		if len(value) > maxIDLength || !utf8.ValidString(value) ||
			strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s: invalid value", name)
		}
		if err := rejectSecret(name, value); err != nil {
			return err
		}
	}
	return nil
}

func (s SystemDescriptor) Validate() error {
	if err := validateIdentifier("system_id", s.SystemID); err != nil {
		return err
	}
	if err := validateIdentifier("provider", s.Provider); err != nil {
		return err
	}
	if err := validateIdentifier("model", s.Model); err != nil {
		return err
	}
	if s.Variant != "" {
		if err := validateIdentifier("variant", s.Variant); err != nil {
			return err
		}
	}
	if err := validateTag("prompt_version", s.PromptVersion); err != nil {
		return err
	}

	return nil
}

func (r MatcherExtractResult) Validate() error {
	switch r.Action {
	case MatcherExtractActionExtract:
		if r.Extraction == nil {
			return fmt.Errorf("matcher_extract.extraction: required for extract action")
		}
		if err := r.Extraction.Validate(); err != nil {
			return fmt.Errorf("matcher_extract.extraction: %w", err)
		}
	case MatcherExtractActionAbstain:
		if r.Extraction != nil {
			return fmt.Errorf("matcher_extract.extraction: must be absent for abstain action")
		}
	default:
		return fmt.Errorf("matcher_extract.action: unsupported value %q", r.Action)
	}

	return nil
}

func (r MatcherRerankResult) Validate() error {
	if err := validateConfidence("matcher_rerank.confidence", r.Confidence); err != nil {
		return err
	}
	switch r.Action {
	case MatcherRerankActionAttach:
		if r.TMDBID <= 0 {
			return fmt.Errorf("matcher_rerank.tmdb_id: attach action requires a positive id")
		}
		if r.PolicyReason != "" {
			return fmt.Errorf("matcher_rerank.policy_reason: attach action requires an empty reason")
		}
		if r.SpecialistAudit != nil &&
			r.TMDBID != r.SpecialistAudit.SelectedTMDBID {
			return fmt.Errorf(
				"matcher_rerank.tmdb_id: specialist attach must use selected_tmdb_id",
			)
		}
	case MatcherRerankActionAbstain:
		if r.TMDBID != 0 {
			return fmt.Errorf("matcher_rerank.tmdb_id: abstain action requires zero")
		}
		switch r.PolicyReason {
		case "", "model_declined", "below_confidence", "candidate_year",
			"candidate_title", "candidate_ambiguous":
		default:
			return fmt.Errorf(
				"matcher_rerank.policy_reason: unsupported value %q",
				r.PolicyReason,
			)
		}
	default:
		return fmt.Errorf("matcher_rerank.action: unsupported value %q", r.Action)
	}
	if r.SpecialistAudit != nil {
		if err := r.SpecialistAudit.Validate(); err != nil {
			return fmt.Errorf("matcher_rerank.specialist_audit: %w", err)
		}
		confidence, err := MatcherSpecialistConfidence(
			r.SpecialistAudit.ScorePPB,
		)
		if err != nil || confidence != r.Confidence {
			return fmt.Errorf(
				"matcher_rerank.confidence: does not equal specialist score_ppb",
			)
		}
	}

	return nil
}

func (r MatcherRerankResult) validateWithThresholds(
	thresholds DecisionThresholds,
) error {
	if err := r.Validate(); err != nil {
		return err
	}
	switch r.Action {
	case MatcherRerankActionAttach:
		if r.Confidence < thresholds.MatcherAttachConfidence {
			return fmt.Errorf(
				"matcher_rerank.confidence: attach action requires at least %.2f",
				thresholds.MatcherAttachConfidence,
			)
		}
	}
	if r.SpecialistAudit != nil {
		thresholdPPB, err := MatcherSpecialistScorePPB(
			thresholds.MatcherAttachConfidence,
		)
		if err != nil {
			return err
		}
		shouldAttach := r.SpecialistAudit.SelectedTMDBID > 0 &&
			r.SpecialistAudit.ScorePPB >= thresholdPPB &&
			!matcherCandidatePolicyVeto(r.PolicyReason)
		if shouldAttach != (r.Action == MatcherRerankActionAttach) {
			return fmt.Errorf(
				"matcher_rerank.action: inconsistent with specialist score and threshold",
			)
		}
	}
	return nil
}

func matcherCandidatePolicyVeto(reason string) bool {
	switch reason {
	case "candidate_year", "candidate_title", "candidate_ambiguous":
		return true
	default:
		return false
	}
}

func (a MatcherSpecialistResultAudit) Validate() error {
	if a.AlgorithmID != MatcherSpecialistAlgorithmID {
		return fmt.Errorf("algorithm_id: unsupported value %q", a.AlgorithmID)
	}
	if a.EligibleCandidates < 0 || a.EligibleCandidates > maxCandidates {
		return fmt.Errorf("eligible_candidates: out of range")
	}
	if a.ScorePPB < 0 || a.ScorePPB > MatcherSpecialistScoreScalePPB {
		return fmt.Errorf("score_ppb: out of range")
	}
	if a.EligibleCandidates == 0 {
		if a.SelectedTMDBID != 0 || a.ScorePPB != 0 {
			return fmt.Errorf(
				"no eligible candidates requires zero selected_tmdb_id and score_ppb",
			)
		}
	} else if a.SelectedTMDBID <= 0 {
		return fmt.Errorf(
			"eligible candidates require a positive selected_tmdb_id",
		)
	}
	return nil
}

func (a MatcherSpecialistDevelopmentExecutionAudit) Validate() error {
	if err := validateSHA256(
		"development_closure_manifest_sha256",
		a.DevelopmentClosureManifestSHA256,
	); err != nil {
		return err
	}
	if a.ThresholdPPB < 0 ||
		a.ThresholdPPB > MatcherSpecialistScoreScalePPB {
		return fmt.Errorf("threshold_ppb: out of range")
	}
	switch a.Mode {
	case MatcherSpecialistDevelopmentRaw:
		if a.ThresholdPPB != 0 {
			return fmt.Errorf("calibration_raw requires threshold_ppb 0")
		}
		if a.DerivedFromResultsSHA256 != "" {
			return fmt.Errorf(
				"calibration_raw must not carry derived_from_results_sha256",
			)
		}
	case MatcherSpecialistDevelopmentCalibrated:
		if err := validateSHA256(
			"derived_from_results_sha256",
			a.DerivedFromResultsSHA256,
		); err != nil {
			return err
		}
	default:
		return fmt.Errorf("mode: unsupported value %q", a.Mode)
	}
	return nil
}

func (r ContentFilterResult) Validate() error {
	if err := validateConfidence("contentfilter.confidence", r.Confidence); err != nil {
		return err
	}
	if r.ReasonTag != "" {
		if err := validateTag("contentfilter.reason_tag", r.ReasonTag); err != nil {
			return err
		}
	}

	switch r.Action {
	case ContentFilterActionDrop:
		if r.IsEnglish == nil || *r.IsEnglish {
			return fmt.Errorf("contentfilter.is_english: drop action requires false")
		}
	case ContentFilterActionKeep:
		if r.IsEnglish == nil || !*r.IsEnglish {
			return fmt.Errorf("contentfilter.is_english: keep action requires true")
		}
	case ContentFilterActionAbstain:
		if r.IsEnglish != nil {
			return fmt.Errorf("contentfilter.is_english: abstain action must omit model verdict")
		}
	default:
		return fmt.Errorf("contentfilter.action: unsupported value %q", r.Action)
	}

	return nil
}

func (r ContentFilterResult) validateWithThresholds(
	thresholds DecisionThresholds,
) error {
	if err := r.Validate(); err != nil {
		return err
	}
	switch r.Action {
	case ContentFilterActionDrop:
		if r.Confidence < thresholds.ContentDropConfidence {
			return fmt.Errorf(
				"contentfilter.confidence: drop action requires at least %.2f",
				thresholds.ContentDropConfidence,
			)
		}
	case ContentFilterActionAbstain:
		if r.Confidence >= thresholds.ContentDropConfidence {
			return fmt.Errorf(
				"contentfilter.confidence: abstain action must be below %.2f",
				thresholds.ContentDropConfidence,
			)
		}
	}
	return nil
}

func (r JunkPurgeResult) Validate() error {
	if err := validateConfidence("junkpurge.confidence", r.Confidence); err != nil {
		return err
	}
	switch r.Verdict {
	case JunkVerdictJunk, JunkVerdictRealMangled, JunkVerdictRealAbsent, JunkVerdictUnsure:
	default:
		return fmt.Errorf("junkpurge.verdict: unsupported value %q", r.Verdict)
	}

	switch r.Action {
	case JunkPurgeActionJunk:
		if r.Verdict != JunkVerdictJunk {
			return fmt.Errorf("junkpurge.verdict: junk action requires junk verdict")
		}
	case JunkPurgeActionKeep:
		if r.Verdict != JunkVerdictRealMangled && r.Verdict != JunkVerdictRealAbsent {
			return fmt.Errorf(
				"junkpurge.verdict: keep action requires a real_mangled or real_absent verdict",
			)
		}
	case JunkPurgeActionAbstain:
		if r.Verdict != JunkVerdictUnsure &&
			r.Verdict != JunkVerdictJunk {
			return fmt.Errorf(
				"junkpurge.verdict: abstain action requires unsure or a junk verdict",
			)
		}
	default:
		return fmt.Errorf("junkpurge.action: unsupported value %q", r.Action)
	}

	return nil
}

func (r JunkPurgeResult) validateWithThresholds(
	thresholds DecisionThresholds,
) error {
	if err := r.Validate(); err != nil {
		return err
	}
	switch r.Action {
	case JunkPurgeActionJunk:
		if r.Confidence < thresholds.JunkConfidence {
			return fmt.Errorf(
				"junkpurge.confidence: junk action requires at least %.2f",
				thresholds.JunkConfidence,
			)
		}
	case JunkPurgeActionAbstain:
		if r.Verdict == JunkVerdictJunk &&
			r.Confidence >= thresholds.JunkConfidence {
			return fmt.Errorf(
				"junkpurge.confidence: abstain junk action must be below %.2f",
				thresholds.JunkConfidence,
			)
		}
	}
	return nil
}

func (u Usage) Validate() error {
	if u.RequestID != "" {
		if err := validateIdentifier("request_id", u.RequestID); err != nil {
			return err
		}
	}

	values := []struct {
		name  string
		value int64
	}{
		{"input_tokens", u.InputTokens},
		{"cached_input_tokens", u.CachedInputTokens},
		{"cache_write_tokens", u.CacheWriteTokens},
		{"output_tokens", u.OutputTokens},
		{"reasoning_tokens", u.ReasoningTokens},
		{"cost_micro_usd", u.CostMicroUSD},
	}
	for _, item := range values {
		if item.value < 0 {
			return fmt.Errorf("%s: must be non-negative", item.name)
		}
	}
	if u.CachedInputTokens > u.InputTokens {
		return fmt.Errorf(
			"cached_input_tokens: %d exceeds input_tokens %d",
			u.CachedInputTokens,
			u.InputTokens,
		)
	}
	if u.CacheWriteTokens > u.InputTokens-u.CachedInputTokens {
		return fmt.Errorf(
			"cached_input_tokens + cache_write_tokens: %d + %d exceeds input_tokens %d",
			u.CachedInputTokens,
			u.CacheWriteTokens,
			u.InputTokens,
		)
	}
	if u.ReasoningTokens > u.OutputTokens {
		return fmt.Errorf(
			"reasoning_tokens: %d exceeds output_tokens %d",
			u.ReasoningTokens,
			u.OutputTokens,
		)
	}

	switch u.CostSource {
	case "":
		// Empty is accepted only for backward-compatible validation of legacy
		// result rows. New formal record validators require an explicit source.
	case CostSourceProviderReported, CostSourceManifestEstimate:
	case CostSourceUnavailable:
		if u.CostMicroUSD != 0 {
			return fmt.Errorf(
				"cost_source: unavailable cannot carry cost_micro_usd",
			)
		}
		if u.AccountingComplete {
			return fmt.Errorf(
				"cost_source: unavailable cannot claim complete accounting",
			)
		}
	default:
		return fmt.Errorf("cost_source: unsupported value %q", u.CostSource)
	}

	return nil
}

func (t Task) valid() bool {
	switch t {
	case TaskMatcherExtract, TaskMatcherRerank, TaskContentFilter, TaskJunkPurge:
		return true
	default:
		return false
	}
}

func (t MediaType) valid() bool {
	return t == MediaTypeMovie || t == MediaTypeTV
}

func (t EnglishTrack) valid() bool {
	switch t {
	case EnglishTrackDub, EnglishTrackSub, EnglishTrackNone, EnglishTrackUnknown:
		return true
	default:
		return false
	}
}

func validateConfidence(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return fmt.Errorf("%s: %v is outside 0..1", name, value)
	}

	return nil
}

func validateIdentifier(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	if len(value) > maxIDLength {
		return fmt.Errorf("%s: length %d exceeds maximum %d", name, len(value), maxIDLength)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s: invalid UTF-8", name)
	}
	if !identifierRE.MatchString(value) {
		return fmt.Errorf("%s: contains unsupported characters", name)
	}
	if err := rejectSecret(name, value); err != nil {
		return err
	}

	return nil
}

func validateTag(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	if len(value) > maxIDLength {
		return fmt.Errorf("%s: length %d exceeds maximum %d", name, len(value), maxIDLength)
	}
	if !tagRE.MatchString(value) {
		return fmt.Errorf("%s: contains unsupported characters", name)
	}
	if err := rejectSecret(name, value); err != nil {
		return err
	}

	return nil
}

func validateText(name, value string, maxLength int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	if len(value) > maxLength {
		return fmt.Errorf("%s: length %d exceeds maximum %d", name, len(value), maxLength)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s: invalid UTF-8", name)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s: contains NUL", name)
	}
	if err := rejectSecret(name, value); err != nil {
		return err
	}

	return nil
}

func rejectSecret(name, value string) error {
	for _, re := range secretREs {
		if re.MatchString(value) {
			return fmt.Errorf("%s: contains secret-like material", name)
		}
	}

	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != 64 || value != strings.ToLower(value) {
		return fmt.Errorf("%s: must be 64 lowercase hexadecimal characters", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s: invalid SHA-256: %w", name, err)
	}

	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
