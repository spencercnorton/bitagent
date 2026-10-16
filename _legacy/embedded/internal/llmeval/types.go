// Package llmeval contains the provider-independent, offline evaluation
// contracts for BitAgent's LLM-assisted classification stages.
package llmeval

import "fmt"

// SchemaVersion is the current corpus and result record schema version.
const SchemaVersion = 1

// Task identifies one independently evaluated LLM use case.
type Task string

const (
	TaskMatcherExtract Task = "matcher_extract"
	TaskMatcherRerank  Task = "matcher_rerank"
	TaskContentFilter  Task = "contentfilter"
	TaskJunkPurge      Task = "junkpurge"
)

var orderedTasks = []Task{
	TaskMatcherExtract,
	TaskMatcherRerank,
	TaskContentFilter,
	TaskJunkPurge,
}

// MediaType is the matcher prompt's normalized media type.
type MediaType string

const (
	MediaTypeMovie MediaType = "movie"
	MediaTypeTV    MediaType = "tv"
)

// EnglishTrack is the matcher extraction prompt's English-availability field.
type EnglishTrack string

const (
	EnglishTrackDub     EnglishTrack = "dub"
	EnglishTrackSub     EnglishTrack = "sub"
	EnglishTrackNone    EnglishTrack = "none"
	EnglishTrackUnknown EnglishTrack = "unknown"
)

// LabelProvenance records where a corpus label came from. It deliberately
// names evidence classes rather than people or credentials.
type LabelProvenance string

const (
	LabelProvenanceHumanReview       LabelProvenance = "human_review"
	LabelProvenanceOperatorOutcome   LabelProvenance = "operator_outcome"
	LabelProvenanceArrEvidence       LabelProvenance = "arr_evidence"
	LabelProvenanceProductionTeacher LabelProvenance = "production_teacher"
	LabelProvenanceConsensus         LabelProvenance = "consensus"
	LabelProvenanceSynthetic         LabelProvenance = "synthetic"
	// LabelProvenanceSamplingCandidate is an explicitly unscored source
	// candidate awaiting independent review. Its expected payload is a closed,
	// task-specific placeholder required only by the v1 union schema. Hosted
	// execution and every scoring/comparison path reject this provenance.
	LabelProvenanceSamplingCandidate LabelProvenance = "sampling_candidate"
)

// LabelStrength separates verified ground truth from weaker teacher labels.
type LabelStrength string

const (
	LabelStrengthGold    LabelStrength = "gold"
	LabelStrengthStrong  LabelStrength = "strong"
	LabelStrengthWeak    LabelStrength = "weak"
	LabelStrengthTeacher LabelStrength = "teacher"
	// LabelStrengthUnreviewed is valid only with sampling_candidate.
	LabelStrengthUnreviewed LabelStrength = "unreviewed"
)

// CandidatePlaceholderPolicyVersion is the only policy marker allowed on an
// unreviewed source candidate. It is not a label policy and cannot survive
// gold promotion.
const CandidatePlaceholderPolicyVersion = "candidate-not-gold-v1"

// LabelMetadata makes label quality and origin part of the immutable corpus.
// SourceRef must be an opaque non-secret identifier, not a URL, token, prompt,
// provider response, or free-form note.
type LabelMetadata struct {
	Provenance       LabelProvenance   `json:"provenance"`
	Strength         LabelStrength     `json:"strength"`
	SourceRef        string            `json:"source_ref,omitempty"`
	PolicyVersion    string            `json:"policy_version"`
	ReviewerCount    int               `json:"reviewer_count,omitempty"`
	HumanReviewProof *HumanReviewProof `json:"human_review_proof,omitempty"`
}

// PrivacyVerificationStatus is the bounded result of applying the production
// source privacy rule before a record is exported. It is an attestation about
// the post-rule source snapshot, not a claim that arbitrary source data is
// safe to publish.
type PrivacyVerificationStatus string

const (
	PrivacyVerifiedPostRulePublic PrivacyVerificationStatus = "verified_post_rule_public"
)

// SourcePrivacyAttestation binds a non-synthetic record to the sampling plan
// and exact production-source snapshot that passed the public-data rule.
// Synthetic protocol fixtures may omit it; every other record must carry it.
type SourcePrivacyAttestation struct {
	PlanID               string                    `json:"plan_id"`
	SourceSnapshotSHA256 string                    `json:"source_snapshot_sha256"`
	Status               PrivacyVerificationStatus `json:"status"`
}

// CorpusRecord is one fully self-contained evaluation case. Exactly one task
// payload must be present and must match Task. SliceIDs support overlapping
// analytical cohorts; GroupID keeps related variants together during splits.
type CorpusRecord struct {
	SchemaVersion int           `json:"schema_version"`
	CaseID        string        `json:"case_id"`
	Task          Task          `json:"task"`
	SliceIDs      []string      `json:"slice_ids"`
	GroupID       string        `json:"group_id"`
	Label         LabelMetadata `json:"label"`
	// Tier separates populations that must never be blended into one score.
	// Added with the disposition contract rather than after adjudication
	// starts, because a tier tag added later is a second migration.
	Tier               GoldTier                  `json:"tier,omitempty"`
	PrivacyAttestation *SourcePrivacyAttestation `json:"privacy_attestation,omitempty"`
	MatcherExtract     *MatcherExtractCase       `json:"matcher_extract,omitempty"`
	MatcherRerank      *MatcherRerankCase        `json:"matcher_rerank,omitempty"`
	ContentFilter      *ContentFilterCase        `json:"contentfilter,omitempty"`
	JunkPurge          *JunkPurgeCase            `json:"junkpurge,omitempty"`
}

// Corpus is a validated, canonically ordered corpus and its SHA-256 identity.
type Corpus struct {
	SHA256  string
	Records []CorpusRecord
}

// MatcherExtractCase freezes every field visible to the extraction model.
type MatcherExtractCase struct {
	Input    MatcherExtractInput    `json:"input"`
	Expected MatcherExtractExpected `json:"expected"`
}

type MatcherExtractInput struct {
	ReleaseName string   `json:"release_name"`
	FilePaths   []string `json:"file_paths,omitempty"`
}

// MatcherExtractExpected accepts any explicitly enumerated equivalent output.
// AllowAbstain is useful for titles whose correct behavior is to invent
// nothing. At least one acceptable output or AllowAbstain must be set.
type MatcherExtractExpected struct {
	Acceptable   []MatcherExtraction `json:"acceptable,omitempty"`
	AllowAbstain bool                `json:"allow_abstain,omitempty"`
}

// MatcherExtraction mirrors the normalized production extraction contract.
type MatcherExtraction struct {
	Title   string       `json:"title"`
	Year    int          `json:"year"`
	Type    MediaType    `json:"type"`
	Season  int          `json:"season"`
	Episode int          `json:"episode"`
	IsAnime bool         `json:"is_anime"`
	English EnglishTrack `json:"english"`
	IsPack  bool         `json:"is_pack"`
	IsAdult bool         `json:"is_adult"`
}

// MatcherRerankCase freezes the extraction and ordered candidate list passed
// to the reranker, so later TMDB/mirror changes cannot alter an A/B run.
type MatcherRerankCase struct {
	Input    MatcherRerankInput    `json:"input"`
	Expected MatcherRerankExpected `json:"expected"`
}

type MatcherRerankInput struct {
	ReleaseName string `json:"release_name"`
	// ParsedTitle is the deterministic parser's BaseTitle at the live matcher
	// boundary. It is frozen policy evidence and is never rendered into the
	// model prompt; without it an offline run cannot exercise the source-title
	// gate that protects live attachments from a model-invented identity.
	ParsedTitle        string             `json:"parsed_title,omitempty"`
	Extraction         MatcherExtraction  `json:"extraction"`
	EffectiveMediaType MediaType          `json:"effective_media_type,omitempty"`
	Candidates         []MatcherCandidate `json:"candidates"`
	// CandidateSource preserves whether production supplied this exact
	// ordered list from the local mirror or the API fallback. It is frozen
	// provenance, not an additional model-visible field.
	CandidateSource string `json:"candidate_source,omitempty"`
}

type MatcherCandidate struct {
	TMDBID        int64     `json:"tmdb_id"`
	Type          MediaType `json:"type"`
	Title         string    `json:"title"`
	OriginalTitle string    `json:"original_title,omitempty"`
	// AltTitles are independently catalogued aliases used only by the final
	// identity gate. BuildPrompt deliberately omits them from model-visible
	// candidates, matching llmmatch.Candidate's json:"-" contract.
	AltTitles []string `json:"alt_titles,omitempty"`
	Year      int      `json:"year"`
	Overview  string   `json:"overview,omitempty"`
}

// MatcherRerankExpected permits multiple equivalent TMDB identities when the
// gold set explicitly establishes them.
type MatcherRerankExpected struct {
	AcceptableTMDBIDs []int64 `json:"acceptable_tmdb_ids,omitempty"`
	AllowAbstain      bool    `json:"allow_abstain,omitempty"`
}

// ContentFilterCase freezes the title and its human/evidence-backed language
// label. The live LLM receives only the title.
type ContentFilterCase struct {
	Input    ContentFilterInput    `json:"input"`
	Expected ContentFilterExpected `json:"expected"`
}

type ContentFilterInput struct {
	Title string `json:"title"`
}

type LanguageClass string

const (
	LanguageEnglish    LanguageClass = "english"
	LanguageNonEnglish LanguageClass = "non_english"
)

type ContentFilterExpected struct {
	Language     LanguageClass `json:"language,omitempty"`
	AllowAbstain bool          `json:"allow_abstain,omitempty"`
}

// JunkPurgeCase freezes the torrent name and its policy DISPOSITION.
//
// It deliberately carries no cataloguedness field. "Is this title in a
// catalogue?" is not the question the deletion decision asks, and a field for
// it would be re-used as one. TestJunkPurgeGoldCarriesNoCatalogueField pins
// that.
type JunkPurgeCase struct {
	Input    JunkPurgeInput    `json:"input"`
	Expected JunkPurgeExpected `json:"expected"`
}

type JunkPurgeInput struct {
	TorrentName string `json:"torrent_name"`
}

// JunkVerdict is the MODEL's reply class, parsed from the deployed judge
// prompt (internal/junkpurge/llm.go, judgePromptVersion "v2-2026-06-26"). It
// lives on JunkPurgeResult only. It is emphatically NOT a gold label: it
// describes provenance, and all 278,451 production judgments are untouched by
// the gold contract.
type JunkVerdict string

const (
	JunkVerdictJunk        JunkVerdict = "junk"
	JunkVerdictRealMangled JunkVerdict = "real_mangled"
	JunkVerdictRealAbsent  JunkVerdict = "real_absent"
	JunkVerdictUnsure      JunkVerdict = "unsure"
)

// JunkPurgeExpected is the gold label. ContentClass is the durable human
// judgement; Disposition is DERIVED from it by DispositionPolicy and is never
// authored by hand. See internal/llmeval/junk_disposition.go.
type JunkPurgeExpected struct {
	Disposition       JunkDisposition  `json:"disposition"`
	ContentClass      JunkContentClass `json:"content_class"`
	DispositionPolicy string           `json:"disposition_policy"`
}

// SystemDescriptor identifies the exact evaluated route without carrying an
// endpoint, request headers, API keys, or raw provider configuration.
type SystemDescriptor struct {
	SystemID      string `json:"system_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Variant       string `json:"variant,omitempty"`
	PromptVersion string `json:"prompt_version"`
}

type ResultStatus string

const (
	ResultStatusOK          ResultStatus = "ok"
	ResultStatusSchemaError ResultStatus = "schema_error"
	ResultStatusError       ResultStatus = "error"
)

// ExecutionAudit binds a result row to the exact non-secret execution inputs
// and records how the concrete provider route was established.
type ExecutionAudit struct {
	ManifestSHA256        string     `json:"manifest_sha256"`
	Route                 RouteAudit `json:"route"`
	CostAccountingVersion int        `json:"cost_accounting_version,omitempty"`
	// Campaign is present on every campaign-bound synchronous result. Legacy
	// and explicitly diagnostic artifacts may omit it, but promotion v2
	// requires exact equality with a binding re-derived from the immutable
	// campaign-plan bytes.
	Campaign *CampaignRunBinding `json:"campaign,omitempty"`
	// UncappedOutputTokenAllowance binds the conservative local fuse ceiling
	// used when a generative request deliberately omits its provider-side
	// output cap. Persisting it prevents a resume from releasing previously
	// reserved spend by supplying a smaller allowance.
	UncappedOutputTokenAllowance       int64                                       `json:"uncapped_output_token_allowance,omitempty"`
	OpenAIBatch                        *OpenAIBatchExecutionAudit                  `json:"openai_batch,omitempty"`
	MatcherSpecialistCalibrationSHA256 string                                      `json:"matcher_specialist_calibration_sha256,omitempty"`
	MatcherSpecialistDevelopment       *MatcherSpecialistDevelopmentExecutionAudit `json:"matcher_specialist_development,omitempty"`
}

type MatcherSpecialistDevelopmentExecutionMode string

const (
	MatcherSpecialistDevelopmentRaw        MatcherSpecialistDevelopmentExecutionMode = "calibration_raw"
	MatcherSpecialistDevelopmentCalibrated MatcherSpecialistDevelopmentExecutionMode = "calibration_rethresholded"
)

// MatcherSpecialistDevelopmentExecutionAudit prevents calibration-only raw
// rows from being mistaken for ordinary evaluation evidence. Rethresholded
// rows retain the original provider usage and route, and additionally bind the
// exact raw results artifact from which their decision was derived.
type MatcherSpecialistDevelopmentExecutionAudit struct {
	Mode                             MatcherSpecialistDevelopmentExecutionMode `json:"mode"`
	DevelopmentClosureManifestSHA256 string                                    `json:"development_closure_manifest_sha256"`
	ThresholdPPB                     int64                                     `json:"threshold_ppb"`
	DerivedFromResultsSHA256         string                                    `json:"derived_from_results_sha256,omitempty"`
}

// OpenAIBatchExecutionAudit makes the discounted asynchronous transport part
// of every reconciled result's immutable evidence. CustomID is case-specific;
// all other fields must be identical across one system result set.
type OpenAIBatchExecutionAudit struct {
	BatchID              string `json:"batch_id"`
	InputFileSHA256      string `json:"input_file_sha256"`
	Endpoint             string `json:"endpoint"`
	CustomID             string `json:"custom_id"`
	PricingMultiplierPPM int64  `json:"pricing_multiplier_ppm"`
}

type RouteAudit struct {
	SnapshotSHA256    string     `json:"snapshot_sha256,omitempty"`
	SnapshotFetchedAt string     `json:"snapshot_fetched_at,omitempty"`
	ReturnedModel     string     `json:"returned_model,omitempty"`
	ReturnedProvider  string     `json:"returned_provider,omitempty"`
	Proof             RouteProof `json:"proof,omitempty"`
	Verified          bool       `json:"verified"`
}

// ResultRecord is the normalized, provider-independent result for one case.
// ErrorCode is a bounded machine tag; raw errors and provider responses are
// intentionally excluded because they can contain secrets.
type ResultRecord struct {
	SchemaVersion         int                   `json:"schema_version"`
	CorpusSHA256          string                `json:"corpus_sha256"`
	RequestContractSHA256 string                `json:"request_contract_sha256"`
	EvaluatorBuildSHA256  string                `json:"evaluator_build_sha256"`
	ExecutionAudit        ExecutionAudit        `json:"execution_audit"`
	CaseID                string                `json:"case_id"`
	Task                  Task                  `json:"task"`
	System                SystemDescriptor      `json:"system"`
	Status                ResultStatus          `json:"status"`
	ErrorCode             string                `json:"error_code,omitempty"`
	MatcherExtract        *MatcherExtractResult `json:"matcher_extract,omitempty"`
	MatcherRerank         *MatcherRerankResult  `json:"matcher_rerank,omitempty"`
	ContentFilter         *ContentFilterResult  `json:"contentfilter,omitempty"`
	JunkPurge             *JunkPurgeResult      `json:"junkpurge,omitempty"`
	Usage                 Usage                 `json:"usage"`
	// RequestTiming separates the primary provider/model operation from optional
	// post-response route-audit overhead. A nil value is retained for legacy and
	// asynchronous artifacts; every newly executed RunCorpus request writes it.
	RequestTiming *RequestTiming `json:"request_timing,omitempty"`
}

const (
	// Keep persisted millisecond values portable through signed 32-bit
	// consumers. A synchronous hosted request lasting more than about 24 days
	// is not credible evaluation evidence.
	maxRequestElapsedMS int64 = 1<<31 - 1
	// SystemConfig applies the same ten-minute upper bound to the manifest-bound
	// synchronous request deadline.
	maxRequestDeadlineMS int64 = 10 * 60 * 1000
	// OpenRouterGenerationAuditDeadlineMS is the independent metadata-only
	// reconciliation budget. It does not extend or redefine model latency.
	OpenRouterGenerationAuditDeadlineMS int64 = 4_000
)

// RequestTiming binds observed synchronous request latency to the exact
// deadline selected by the evaluated system manifest. ElapsedMS is the
// primary provider/model operation only; optional route-audit and total timing
// keep post-response verification overhead from distorting model latency.
// Milliseconds are integral so canonical reports remain deterministic.
type RequestTiming struct {
	ElapsedMS      int64             `json:"elapsed_ms"`
	DeadlineMS     int64             `json:"deadline_ms"`
	TotalElapsedMS int64             `json:"total_elapsed_ms,omitempty"`
	RouteAudit     *RouteAuditTiming `json:"route_audit,omitempty"`
}

// ExceededDeadline reports provider/model work that completed outside the
// manifest-bound synchronous budget. Validation deliberately retains such
// timing on timeout/error rows so billed usage is not discarded; scoring and
// promotion must count it as deadline-failure evidence even when a provider or
// test double omitted the bounded "timeout" error code.
func (t RequestTiming) ExceededDeadline() bool {
	return t.DeadlineMS > 0 && t.ElapsedMS > t.DeadlineMS
}

// RouteAuditTiming records the bounded, metadata-only OpenRouter audit that
// may follow a primary response. It never represents a repeated inference.
type RouteAuditTiming struct {
	ElapsedMS  int64  `json:"elapsed_ms"`
	DeadlineMS int64  `json:"deadline_ms"`
	Attempts   int    `json:"attempts"`
	Succeeded  bool   `json:"succeeded"`
	ErrorCode  string `json:"error_code,omitempty"`
}

// Validate rejects malformed timing evidence. Absence is represented by a nil
// ResultRecord.RequestTiming, never a zero-valued RequestTiming object.
func (t RequestTiming) Validate() error {
	if t.ElapsedMS < 0 {
		return fmt.Errorf("elapsed_ms: must be non-negative")
	}
	if t.ElapsedMS > maxRequestElapsedMS {
		return fmt.Errorf(
			"elapsed_ms: %d exceeds maximum %d",
			t.ElapsedMS,
			maxRequestElapsedMS,
		)
	}
	if t.DeadlineMS < 0 {
		return fmt.Errorf("deadline_ms: must be non-negative")
	}
	if t.DeadlineMS > maxRequestDeadlineMS {
		return fmt.Errorf(
			"deadline_ms: %d exceeds maximum %d",
			t.DeadlineMS,
			maxRequestDeadlineMS,
		)
	}
	if t.ElapsedMS == 0 {
		return fmt.Errorf("elapsed_ms: timing evidence must be positive")
	}
	if t.DeadlineMS == 0 {
		return fmt.Errorf(
			"deadline_ms: positive elapsed timing requires a positive deadline",
		)
	}
	if t.TotalElapsedMS < 0 {
		return fmt.Errorf("total_elapsed_ms: must be non-negative")
	}
	if t.TotalElapsedMS > maxRequestElapsedMS {
		return fmt.Errorf(
			"total_elapsed_ms: %d exceeds maximum %d",
			t.TotalElapsedMS,
			maxRequestElapsedMS,
		)
	}
	if t.TotalElapsedMS != 0 && t.TotalElapsedMS < t.ElapsedMS {
		return fmt.Errorf(
			"total_elapsed_ms: must be at least elapsed_ms",
		)
	}
	if t.RouteAudit == nil {
		return nil
	}
	if err := t.RouteAudit.Validate(); err != nil {
		return fmt.Errorf("route_audit: %w", err)
	}
	if t.TotalElapsedMS == 0 {
		return fmt.Errorf(
			"total_elapsed_ms: route audit evidence requires total timing",
		)
	}
	if t.TotalElapsedMS < t.RouteAudit.ElapsedMS {
		return fmt.Errorf(
			"total_elapsed_ms: must be at least route_audit.elapsed_ms",
		)
	}
	return nil
}

// Validate rejects malformed or ambiguous route-audit evidence.
func (t RouteAuditTiming) Validate() error {
	if t.ElapsedMS <= 0 {
		return fmt.Errorf("elapsed_ms: timing evidence must be positive")
	}
	if t.ElapsedMS > maxRequestElapsedMS {
		return fmt.Errorf(
			"elapsed_ms: %d exceeds maximum %d",
			t.ElapsedMS,
			maxRequestElapsedMS,
		)
	}
	if t.DeadlineMS <= 0 {
		return fmt.Errorf("deadline_ms: must be positive")
	}
	if t.DeadlineMS > maxRequestDeadlineMS {
		return fmt.Errorf(
			"deadline_ms: %d exceeds maximum %d",
			t.DeadlineMS,
			maxRequestDeadlineMS,
		)
	}
	if t.Attempts < 1 || t.Attempts > 16 {
		return fmt.Errorf("attempts: must be in 1..16")
	}
	if t.Succeeded {
		if t.ErrorCode != "" {
			return fmt.Errorf("error_code: successful audit cannot carry an error")
		}
		return nil
	}
	if !validTimingErrorCode(t.ErrorCode) {
		return fmt.Errorf("error_code: failed audit requires a bounded machine code")
	}
	return nil
}

func validTimingErrorCode(code string) bool {
	if len(code) < 1 || len(code) > 64 {
		return false
	}
	for _, character := range code {
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

type MatcherExtractAction string

const (
	MatcherExtractActionExtract MatcherExtractAction = "extract"
	MatcherExtractActionAbstain MatcherExtractAction = "abstain"
)

type MatcherExtractResult struct {
	Action     MatcherExtractAction `json:"action"`
	Extraction *MatcherExtraction   `json:"extraction,omitempty"`
}

type MatcherRerankAction string

const (
	MatcherRerankActionAttach  MatcherRerankAction = "attach"
	MatcherRerankActionAbstain MatcherRerankAction = "abstain"
)

type MatcherRerankResult struct {
	Action          MatcherRerankAction           `json:"action"`
	TMDBID          int64                         `json:"tmdb_id,omitempty"`
	Confidence      float64                       `json:"confidence"`
	PolicyReason    string                        `json:"policy_reason,omitempty"`
	SpecialistAudit *MatcherSpecialistResultAudit `json:"specialist_audit,omitempty"`
}

// MatcherSpecialistResultAudit exposes the deterministic local decision made
// from provider-returned embedding vectors. It preserves the selected identity
// even when the calibrated action abstains.
type MatcherSpecialistResultAudit struct {
	AlgorithmID        string `json:"algorithm_id"`
	EligibleCandidates int    `json:"eligible_candidates"`
	SelectedTMDBID     int64  `json:"selected_tmdb_id"`
	ScorePPB           int64  `json:"score_ppb"`
}

type ContentFilterAction string

// Content-filter actions describe intervention evidence, not storage state.
// Abstain preserves the item in production while recording that a destructive
// drop was not supported at the configured confidence threshold.
const (
	ContentFilterActionKeep    ContentFilterAction = "keep"
	ContentFilterActionDrop    ContentFilterAction = "drop"
	ContentFilterActionAbstain ContentFilterAction = "abstain"
)

type ContentFilterResult struct {
	Action     ContentFilterAction `json:"action"`
	IsEnglish  *bool               `json:"is_english,omitempty"`
	Confidence float64             `json:"confidence"`
	ReasonTag  string              `json:"reason_tag,omitempty"`
}

type JunkPurgeAction string

// Junk-purge actions describe intervention evidence, not storage state.
// Abstain preserves the item in production while recording an unsure verdict
// or a junk verdict below the configured destructive-action threshold.
const (
	JunkPurgeActionKeep    JunkPurgeAction = "keep"
	JunkPurgeActionJunk    JunkPurgeAction = "junk"
	JunkPurgeActionAbstain JunkPurgeAction = "abstain"
)

type JunkPurgeResult struct {
	Action     JunkPurgeAction `json:"action"`
	Verdict    JunkVerdict     `json:"verdict"`
	Confidence float64         `json:"confidence"`
}

// CostSource records where CostMicroUSD came from. ProviderReported is an
// authenticated provider billing value, ManifestEstimate is calculated from
// returned token counts and the immutable system manifest, and Unavailable
// means no defensible cost value was available.
type CostSource string

const (
	CostSourceProviderReported CostSource = "provider_reported"
	CostSourceManifestEstimate CostSource = "manifest_estimate"
	CostSourceUnavailable      CostSource = "unavailable"
	// CurrentCostAccountingVersion marks result rows created after explicit
	// cost provenance became part of the formal execution contract. Version
	// zero is reserved for reading intentional legacy fixtures/artifacts.
	CurrentCostAccountingVersion = 1
)

// Usage stores provider token counts plus explicit cost provenance.
// CostMicroUSD is integer millionths of one US dollar to keep canonical JSON
// and aggregation free of floating-point rounding drift.
type Usage struct {
	// RequestID deduplicates usage shared by multiple outputs from one packed
	// provider request. When empty, the usage is treated as case-local.
	RequestID         string `json:"request_id,omitempty"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int64  `json:"cache_write_tokens,omitempty"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens,omitempty"`
	CostMicroUSD      int64  `json:"cost_micro_usd"`
	// CostSource is omitted only on intentionally supported legacy result rows.
	// Every newly executed synchronous or OpenAI Batch result must set it.
	CostSource CostSource `json:"cost_source,omitempty"`
	// AccountingComplete proves the transport observed a complete accounting
	// token envelope. CostSource independently distinguishes exact provider
	// billing from a manifest-derived estimate. Missing/partial usage remains
	// useful action evidence but cannot replace a spend ceiling.
	AccountingComplete bool `json:"accounting_complete,omitempty"`
}

// withExplicitUnavailableCostSource upgrades a zero-cost, incomplete legacy
// zero value at the boundary where a new result is created. It deliberately
// does not guess provenance for a positive cost or a complete accounting
// claim supplied by an alternate Completer.
func withExplicitUnavailableCostSource(usage Usage) Usage {
	if usage.CostSource == "" && usage.CostMicroUSD == 0 &&
		!usage.AccountingComplete {
		usage.CostSource = CostSourceUnavailable
	}
	return usage
}
