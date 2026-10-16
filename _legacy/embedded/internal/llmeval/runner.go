package llmeval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type Completer interface {
	Complete(
		ctx context.Context,
		system SystemConfig,
		apiKey string,
		prompt PromptRequest,
	) (Completion, error)
}

type RunOptions struct {
	// MaxCostMicroUSD is a local pre-request fuse evaluated from the checked-in
	// pricing configuration. It is not a provider-enforced billing limit.
	MaxCostMicroUSD int64
	// UncappedOutputTokenAllowance is required when a Responses request omits
	// its provider-side output cap. It becomes the conservative per-call output
	// ceiling in both fresh and resumed local fuse accounting.
	UncappedOutputTokenAllowance int64
	Thresholds                   DecisionThresholds
	// EvaluatorBuildSHA256 is the SHA-256 of the executable performing the
	// transport and parse. It prevents a resumed run from mixing rows emitted
	// by different evaluator implementations.
	EvaluatorBuildSHA256 string
	ExecutionBinding     ExecutionBinding
	// MatcherSpecialistCalibrationSHA256 is required for ordinary hosted
	// embedding evaluation and absent from calibration-only development rows.
	MatcherSpecialistCalibrationSHA256 string
	// MatcherSpecialistDevelopment is set only by the isolated development
	// collection command. It stamps calibration-only threshold-zero evidence.
	MatcherSpecialistDevelopment *MatcherSpecialistDevelopmentExecutionAudit
	Existing                     []ResultRecord
	OnResult                     func(ResultRecord) error
}

// ExecutionBinding contains the immutable artifact identities and expected
// route used to stamp and validate every result in a run. ExpectedModel and
// ExpectedProvider are deliberately not persisted separately; the verified
// route returned by OpenRouter is stored in each row and compared to these
// snapshot-derived values.
type ExecutionBinding struct {
	ManifestSHA256         string
	RouteSnapshotSHA256    string
	RouteSnapshotFetchedAt string
	ExpectedModel          string
	ExpectedProvider       string
	// Campaign is re-derived from exact preregistered plan bytes by the CLI.
	// A nil value preserves explicitly non-campaign legacy/diagnostic runs.
	Campaign *CampaignRunBinding
}

type RunSummary struct {
	Results               []ResultRecord
	Completed             int
	Skipped               int
	Requests              int
	CostMicroUSD          int64
	AccountedCostMicroUSD int64
	// StoppedByCostCap reports that the local estimated-spend fuse prevented
	// another request; it does not attest to an external invoice ceiling.
	StoppedByCostCap bool
}

// RunCorpus evaluates cases sequentially. There is deliberately no concurrency
// knob: these workloads have no latency requirement, and one-at-a-time requests
// make the spend fuse and provider audit trail deterministic.
func RunCorpus(
	ctx context.Context,
	corpus Corpus,
	system SystemConfig,
	apiKey string,
	client Completer,
	options RunOptions,
) (RunSummary, error) {
	canonical, err := NewCorpus(corpus.Records)
	if err != nil {
		return RunSummary{}, err
	}
	if corpus.SHA256 != "" && corpus.SHA256 != canonical.SHA256 {
		return RunSummary{}, fmt.Errorf("corpus identity mismatch")
	}
	if err := ValidateGoldCorpus(canonical); err != nil {
		return RunSummary{}, err
	}
	if err := ValidateHostedCorpusPrivacy(canonical); err != nil {
		return RunSummary{}, err
	}
	if err := system.Validate(); err != nil {
		return RunSummary{}, fmt.Errorf("system: %w", err)
	}
	if err := validateRunOutputAllowance(
		canonical,
		system,
		options.UncappedOutputTokenAllowance,
	); err != nil {
		return RunSummary{}, err
	}
	if err := validateRunCorpusSystemContract(canonical, system); err != nil {
		return RunSummary{}, err
	}
	if err := options.Thresholds.Validate(); err != nil {
		return RunSummary{}, err
	}
	if err := validateSHA256(
		"evaluator_build_sha256",
		options.EvaluatorBuildSHA256,
	); err != nil {
		return RunSummary{}, err
	}
	if err := options.ExecutionBinding.Validate(system); err != nil {
		return RunSummary{}, fmt.Errorf("execution binding: %w", err)
	}
	if err := validateRunCampaignBinding(canonical, system, options); err != nil {
		return RunSummary{}, fmt.Errorf("campaign execution binding: %w", err)
	}
	if options.MatcherSpecialistDevelopment != nil {
		if err := options.MatcherSpecialistDevelopment.Validate(); err != nil {
			return RunSummary{}, fmt.Errorf(
				"matcher specialist development execution: %w",
				err,
			)
		}
		if options.MatcherSpecialistDevelopment.Mode !=
			MatcherSpecialistDevelopmentRaw ||
			system.APIKind != APIKindEmbedding ||
			len(canonical.Records) == 0 {
			return RunSummary{}, fmt.Errorf(
				"matcher specialist development execution requires a threshold-zero embedding matcher corpus",
			)
		}
		thresholdPPB, thresholdErr := MatcherSpecialistScorePPB(
			options.Thresholds.MatcherAttachConfidence,
		)
		if thresholdErr != nil || thresholdPPB != 0 {
			return RunSummary{}, fmt.Errorf(
				"matcher specialist development execution requires threshold zero",
			)
		}
		for _, record := range canonical.Records {
			if record.Task != TaskMatcherRerank {
				return RunSummary{}, fmt.Errorf(
					"matcher specialist development execution contains a non-rerank case",
				)
			}
		}
	}
	if system.APIKind == APIKindEmbedding {
		if options.MatcherSpecialistDevelopment == nil {
			if err := validateSHA256(
				"matcher_specialist_calibration_sha256",
				options.MatcherSpecialistCalibrationSHA256,
			); err != nil {
				return RunSummary{}, err
			}
		} else if options.MatcherSpecialistCalibrationSHA256 != "" {
			return RunSummary{}, fmt.Errorf(
				"development collection cannot bind a fitted calibration",
			)
		}
	} else if options.MatcherSpecialistCalibrationSHA256 != "" ||
		options.MatcherSpecialistDevelopment != nil {
		return RunSummary{}, fmt.Errorf(
			"matcher specialist run bindings require an embedding system",
		)
	}
	if client == nil {
		return RunSummary{}, fmt.Errorf("completer is nil")
	}

	summary := RunSummary{}
	existing := make(map[string]ResultRecord, len(options.Existing))
	recordsByID := make(map[string]CorpusRecord, len(canonical.Records))
	for _, record := range canonical.Records {
		recordsByID[record.CaseID] = record
	}
	var directReturnedModel string
	for _, result := range options.Existing {
		if err := result.ValidateWithThresholds(options.Thresholds); err != nil {
			return RunSummary{}, fmt.Errorf("existing result %q: %w", result.CaseID, err)
		}
		if err := validateResultRequestTiming(result); err != nil {
			return RunSummary{}, fmt.Errorf(
				"existing result %q request timing: %w",
				result.CaseID,
				err,
			)
		}
		if result.RequestTiming != nil &&
			result.RequestTiming.DeadlineMS != system.RequestTimeoutMS {
			return RunSummary{}, fmt.Errorf(
				"existing result %q request timing deadline %d does not match manifest-bound request timeout %d",
				result.CaseID,
				result.RequestTiming.DeadlineMS,
				system.RequestTimeoutMS,
			)
		}
		existingMarker := result.ExecutionAudit.MatcherSpecialistDevelopment
		switch {
		case options.MatcherSpecialistDevelopment == nil &&
			existingMarker != nil:
			return RunSummary{}, fmt.Errorf(
				"existing result %q is calibration-only development evidence",
				result.CaseID,
			)
		case options.MatcherSpecialistDevelopment != nil &&
			(existingMarker == nil ||
				*existingMarker != *options.MatcherSpecialistDevelopment):
			return RunSummary{}, fmt.Errorf(
				"existing result %q has a different matcher specialist development binding",
				result.CaseID,
			)
		}
		if result.ExecutionAudit.MatcherSpecialistCalibrationSHA256 !=
			options.MatcherSpecialistCalibrationSHA256 {
			return RunSummary{}, fmt.Errorf(
				"existing result %q has a different matcher specialist calibration binding",
				result.CaseID,
			)
		}
		if result.CorpusSHA256 != canonical.SHA256 {
			return RunSummary{}, fmt.Errorf("existing result %q has a different corpus identity", result.CaseID)
		}
		if result.System != system.Descriptor() {
			return RunSummary{}, fmt.Errorf("existing result %q belongs to a different system", result.CaseID)
		}
		if result.EvaluatorBuildSHA256 != options.EvaluatorBuildSHA256 {
			return RunSummary{}, fmt.Errorf(
				"existing result %q belongs to a different evaluator build",
				result.CaseID,
			)
		}
		if err := validateExistingExecutionAudit(
			result,
			system,
			options.ExecutionBinding,
			options.UncappedOutputTokenAllowance,
			&directReturnedModel,
		); err != nil {
			return RunSummary{}, fmt.Errorf(
				"existing result %q execution audit: %w",
				result.CaseID,
				err,
			)
		}
		record, ok := recordsByID[result.CaseID]
		if !ok {
			return RunSummary{}, fmt.Errorf("existing result %q is not in the corpus", result.CaseID)
		}
		if result.Task != record.Task {
			return RunSummary{}, fmt.Errorf("existing result %q has a different task", result.CaseID)
		}
		if isUncappedGenerativeSystem(system) &&
			result.Usage.OutputTokens > options.UncappedOutputTokenAllowance {
			return RunSummary{}, fmt.Errorf(
				"existing result %q output_tokens %d exceeds its bound uncapped output-token allowance %d",
				result.CaseID,
				result.Usage.OutputTokens,
				options.UncappedOutputTokenAllowance,
			)
		}
		expectedContract, err := RequestContractSHA256(record, system, options.Thresholds)
		if err != nil {
			return RunSummary{}, fmt.Errorf(
				"existing result %q request contract: %w",
				result.CaseID,
				err,
			)
		}
		if result.RequestContractSHA256 != expectedContract {
			return RunSummary{}, fmt.Errorf(
				"existing result %q has a different request contract",
				result.CaseID,
			)
		}
		if _, duplicate := existing[result.CaseID]; duplicate {
			return RunSummary{}, fmt.Errorf("duplicate existing result %q", result.CaseID)
		}
		existing[result.CaseID] = result
		nextCost, err := checkedAddInt64(
			summary.CostMicroUSD,
			result.Usage.CostMicroUSD,
		)
		if err != nil {
			return RunSummary{}, fmt.Errorf(
				"existing result %q cost accounting: %w",
				result.CaseID,
				err,
			)
		}
		summary.CostMicroUSD = nextCost
		if options.MaxCostMicroUSD > 0 {
			prompt, err := BuildPrompt(record)
			if err != nil {
				return RunSummary{}, fmt.Errorf(
					"existing result %q cost fuse prompt: %w",
					result.CaseID,
					err,
				)
			}
			accountedCost := costFuseCharge(
				system,
				result.Usage,
				estimateRequestCeiling(
					system,
					prompt,
					options.UncappedOutputTokenAllowance,
				),
			)
			nextAccounted, err := checkedAddInt64(
				summary.AccountedCostMicroUSD,
				accountedCost,
			)
			if err != nil {
				return RunSummary{}, fmt.Errorf(
					"existing result %q cost fuse accounting: %w",
					result.CaseID,
					err,
				)
			}
			summary.AccountedCostMicroUSD = nextAccounted
		}
	}

	for _, record := range canonical.Records {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if !system.SupportsTask(record.Task) {
			return summary, fmt.Errorf(
				"system %q does not support case %q task %q",
				system.SystemID,
				record.CaseID,
				record.Task,
			)
		}
		if result, ok := existing[record.CaseID]; ok {
			summary.Results = append(summary.Results, result)
			summary.Skipped++
			continue
		}

		prompt, err := BuildPrompt(record)
		if err != nil {
			return summary, err
		}
		requestCeiling := int64(0)
		if options.MaxCostMicroUSD > 0 {
			requestCeiling = estimateRequestCeiling(
				system,
				prompt,
				options.UncappedOutputTokenAllowance,
			)
			projectedCost, err := checkedAddInt64(
				summary.AccountedCostMicroUSD,
				requestCeiling,
			)
			if err != nil {
				return summary, fmt.Errorf(
					"case %q cost ceiling accounting: %w",
					record.CaseID,
					err,
				)
			}
			if projectedCost > options.MaxCostMicroUSD {
				summary.StoppedByCostCap = true
				break
			}
		}

		if system.RequestTimeoutMS <= 0 {
			return summary, fmt.Errorf(
				"case %q synchronous request requires a positive manifest-bound request_timeout_ms",
				record.CaseID,
			)
		}
		started := time.Now()
		completion, callErr := client.Complete(ctx, system, apiKey, prompt)
		completion.Usage = withExplicitUnavailableCostSource(completion.Usage)
		totalElapsedMS := elapsedMillisecondsCeil(time.Since(started))
		summary.Requests++
		requestTiming := &RequestTiming{
			ElapsedMS:      totalElapsedMS,
			DeadlineMS:     system.RequestTimeoutMS,
			TotalElapsedMS: totalElapsedMS,
		}
		if completion.RequestTiming != nil {
			// The client owns the primary/route-audit split, but the runner
			// owns both the manifest binding and end-to-end wall clock.
			if completion.RequestTiming.DeadlineMS != system.RequestTimeoutMS {
				return summary, fmt.Errorf(
					"case %q client request timing deadline %d does not match manifest-bound request timeout %d",
					record.CaseID,
					completion.RequestTiming.DeadlineMS,
					system.RequestTimeoutMS,
				)
			}
			requestTiming.ElapsedMS = completion.RequestTiming.ElapsedMS
			if completion.RequestTiming.RouteAudit != nil {
				routeAudit := *completion.RequestTiming.RouteAudit
				requestTiming.RouteAudit = &routeAudit
			}
		}
		executionAudit, routeErr := auditCompletionRoute(
			system,
			options.ExecutionBinding,
			completion,
			&directReturnedModel,
		)
		if routeErr != nil &&
			(callErr == nil || completionHasRouteEvidence(completion)) {
			callErr = routeErr
		}
		var result ResultRecord
		if callErr != nil {
			result = ErrorResult(
				canonical.SHA256,
				record,
				system,
				options.Thresholds,
				callErr,
			)
			// A provider can return billable usage with a refusal, empty
			// completion, or otherwise unusable envelope. Preserve it so
			// reliability failures cannot disappear from spend accounting.
			result.Usage = completion.Usage
		} else {
			result = ParseCompletion(
				canonical.SHA256,
				record,
				system,
				completion,
				options.Thresholds,
			)
		}
		result.EvaluatorBuildSHA256 = options.EvaluatorBuildSHA256
		result.ExecutionAudit = executionAudit
		result.RequestTiming = requestTiming
		result.ExecutionAudit.UncappedOutputTokenAllowance =
			options.UncappedOutputTokenAllowance
		result.ExecutionAudit.MatcherSpecialistCalibrationSHA256 =
			options.MatcherSpecialistCalibrationSHA256
		if options.MatcherSpecialistDevelopment != nil {
			marker := *options.MatcherSpecialistDevelopment
			result.ExecutionAudit.MatcherSpecialistDevelopment = &marker
		}
		if err := result.ValidateWithThresholds(options.Thresholds); err != nil {
			return summary, fmt.Errorf(
				"case %q normalized result: %w",
				record.CaseID,
				err,
			)
		}
		if err := validateResultRequestTiming(result); err != nil {
			return summary, fmt.Errorf(
				"case %q normalized result request timing: %w",
				record.CaseID,
				err,
			)
		}
		nextCost, err := checkedAddInt64(
			summary.CostMicroUSD,
			result.Usage.CostMicroUSD,
		)
		if err != nil {
			return summary, fmt.Errorf(
				"case %q cost accounting: %w",
				record.CaseID,
				err,
			)
		}
		summary.CostMicroUSD = nextCost
		if options.MaxCostMicroUSD > 0 {
			accountedCost := costFuseCharge(
				system,
				result.Usage,
				requestCeiling,
			)
			nextAccounted, err := checkedAddInt64(
				summary.AccountedCostMicroUSD,
				accountedCost,
			)
			if err != nil {
				return summary, fmt.Errorf(
					"case %q cost fuse accounting: %w",
					record.CaseID,
					err,
				)
			}
			summary.AccountedCostMicroUSD = nextAccounted
		}
		summary.Results = append(summary.Results, result)
		summary.Completed++
		outputAllowanceExceeded := isUncappedGenerativeSystem(system) &&
			result.Usage.OutputTokens > options.UncappedOutputTokenAllowance
		if options.OnResult != nil {
			if err := options.OnResult(result); err != nil {
				return summary, fmt.Errorf("persist result %q: %w", record.CaseID, err)
			}
		}
		if outputAllowanceExceeded {
			return summary, fmt.Errorf(
				"case %q output_tokens %d exceeded the bound uncapped output-token allowance %d; result was retained and no further request was sent",
				record.CaseID,
				result.Usage.OutputTokens,
				options.UncappedOutputTokenAllowance,
			)
		}
	}
	return summary, nil
}

func elapsedMillisecondsCeil(elapsed time.Duration) int64 {
	if elapsed <= 0 {
		return 1
	}
	milliseconds := elapsed / time.Millisecond
	if elapsed%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds < 1 {
		return 1
	}
	return int64(milliseconds)
}

func validateResultRequestTiming(result ResultRecord) error {
	if result.RequestTiming == nil {
		return nil
	}
	return result.RequestTiming.Validate()
}

// costFuseCharge keeps the pre-call ceiling reserved unless the result carries
// usable observed accounting. This prevents a timeout, unreadable response, or
// live-valid response with missing/malformed usage from resetting the budget
// after every sequential call. Reported Usage remains untouched; this value is
// only the conservative local fuse ledger.
func costFuseCharge(
	system SystemConfig,
	usage Usage,
	requestCeiling int64,
) int64 {
	if usageAccountingCompleteForSystem(
		system,
		usage.InputTokens,
		usage.OutputTokens,
	) && usage.AccountingComplete {
		if usage.CostMicroUSD > 0 {
			return usage.CostMicroUSD
		}
		if system.isZeroPriced() {
			return 0
		}
		outputTokens := max(usage.OutputTokens, usage.ReasoningTokens)
		estimated := system.EstimateCostWithCacheWriteMicroUSD(
			usage.InputTokens,
			usage.CachedInputTokens,
			usage.CacheWriteTokens,
			outputTokens,
		)
		if estimated > 0 {
			return estimated
		}
		// Micro-USD rounding can turn a complete sub-micro-dollar request into zero.
		// Retain one unit so repeated tiny calls cannot bypass the fuse.
		return 1
	}
	// Partial accounting may still contain an authenticated exact charge. It
	// cannot release the reservation, but it can prove that the reservation was
	// too low (including when a stale manifest says the route is free).
	if usage.CostMicroUSD > requestCeiling {
		return usage.CostMicroUSD
	}
	if requestCeiling > 0 {
		return requestCeiling
	}
	if usage.CostMicroUSD > 0 {
		return usage.CostMicroUSD
	}
	if system.isZeroPriced() {
		return 0
	}
	return 1
}

// validateRunCorpusSystemContract proves every case is executable before the
// first paid request. In particular, rerank-v1 captures do not contain the
// effective movie/TV state used by the live final identity gate, so they may be
// retained as historical evidence but cannot start any deployed-wire-contract
// run (direct production fidelity or pinned OpenRouter shadow fidelity).
func validateRunCorpusSystemContract(corpus Corpus, system SystemConfig) error {
	for _, record := range corpus.Records {
		if !system.SupportsTask(record.Task) {
			return fmt.Errorf(
				"system %q does not support case %q task %q",
				system.SystemID,
				record.CaseID,
				record.Task,
			)
		}
		if !system.UsesDeployedWireContract() ||
			record.Task != TaskMatcherRerank {
			continue
		}
		if record.MatcherRerank == nil ||
			(record.MatcherRerank.Input.EffectiveMediaType != MediaTypeMovie &&
				record.MatcherRerank.Input.EffectiveMediaType != MediaTypeTV) {
			return fmt.Errorf(
				"deployed-wire-contract matcher rerank case %q requires capture contract llmmatch-chat-rerank-v2 effective_media_type",
				record.CaseID,
			)
		}
		if system.RequireSourceTitle &&
			strings.TrimSpace(record.MatcherRerank.Input.ParsedTitle) == "" {
			return fmt.Errorf(
				"deployed-wire-contract matcher rerank case %q requires parsed_title for the configured source-title gate",
				record.CaseID,
			)
		}
	}
	return nil
}

func validateRunOutputAllowance(
	corpus Corpus,
	system SystemConfig,
	allowance int64,
) error {
	if !isUncappedGenerativeSystem(system) {
		if allowance != 0 {
			return fmt.Errorf(
				"uncapped output allowance is valid only for an uncapped generative system",
			)
		}
		return nil
	}
	if allowance <= 0 {
		return fmt.Errorf(
			"uncapped generative execution requires a positive output-token allowance",
		)
	}
	for _, record := range corpus.Records {
		prompt, err := BuildPrompt(record)
		if err != nil {
			return err
		}
		if allowance < int64(prompt.MaxCompletionTokens) {
			return fmt.Errorf(
				"uncapped output-token allowance %d is below case %q nominal %d-token ceiling",
				allowance,
				record.CaseID,
				prompt.MaxCompletionTokens,
			)
		}
	}
	return nil
}

func isUncappedGenerativeSystem(system SystemConfig) bool {
	return system.OmitMaxCompletionTokens &&
		(system.APIKind == APIKindChat || system.APIKind == APIKindResponses)
}

func validateRunCampaignBinding(
	corpus Corpus,
	system SystemConfig,
	options RunOptions,
) error {
	binding := options.ExecutionBinding.Campaign
	if binding == nil {
		return nil
	}
	if binding.Corpus.SHA256 != corpus.SHA256 {
		return fmt.Errorf("campaign corpus identity differs from selected corpus")
	}
	if options.MaxCostMicroUSD != binding.CostCapMicroUSD {
		return fmt.Errorf(
			"max cost fuse %d differs from campaign run cap %d",
			options.MaxCostMicroUSD,
			binding.CostCapMicroUSD,
		)
	}
	for _, record := range corpus.Records {
		if record.Task != binding.Task {
			return fmt.Errorf(
				"case %q task %q differs from campaign task %q",
				record.CaseID,
				record.Task,
				binding.Task,
			)
		}
	}
	if system.Provider == "openrouter" {
		if binding.RouteSnapshot == nil {
			return fmt.Errorf("OpenRouter campaign run requires a route snapshot")
		}
		if binding.RouteSnapshot.SHA256 !=
			options.ExecutionBinding.RouteSnapshotSHA256 {
			return fmt.Errorf(
				"campaign route snapshot identity differs from execution binding",
			)
		}
	} else if binding.RouteSnapshot != nil {
		return fmt.Errorf(
			"campaign route snapshot is valid only for OpenRouter",
		)
	}
	return nil
}

func cloneCampaignRunBinding(
	binding *CampaignRunBinding,
) *CampaignRunBinding {
	if binding == nil {
		return nil
	}
	cloned := *binding
	if binding.GoldClosure != nil {
		value := *binding.GoldClosure
		cloned.GoldClosure = &value
	}
	if binding.PrivacySidecar != nil {
		value := *binding.PrivacySidecar
		cloned.PrivacySidecar = &value
	}
	if binding.RouteSnapshot != nil {
		value := *binding.RouteSnapshot
		cloned.RouteSnapshot = &value
	}
	return &cloned
}

func (b ExecutionBinding) Validate(system SystemConfig) error {
	if err := validateSHA256("manifest_sha256", b.ManifestSHA256); err != nil {
		return err
	}
	if b.Campaign != nil {
		if err := b.Campaign.Validate(); err != nil {
			return fmt.Errorf("campaign: %w", err)
		}
		if b.Campaign.SystemManifestSHA256 != b.ManifestSHA256 {
			return fmt.Errorf("campaign manifest identity differs from execution binding")
		}
		if b.Campaign.SystemID != system.SystemID {
			return fmt.Errorf("campaign system_id differs from selected system")
		}
	}
	if system.Provider == "openrouter" {
		if err := validateSHA256(
			"route_snapshot_sha256",
			b.RouteSnapshotSHA256,
		); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339Nano, b.RouteSnapshotFetchedAt); err != nil {
			return fmt.Errorf("route_snapshot_fetched_at must be RFC3339")
		}
		if err := validateIdentifier(
			"expected OpenRouter model",
			b.ExpectedModel,
		); err != nil {
			return err
		}
		if strings.TrimSpace(b.ExpectedProvider) == "" {
			return fmt.Errorf("expected OpenRouter provider is required")
		}
		return nil
	}
	if b.RouteSnapshotSHA256 != "" || b.RouteSnapshotFetchedAt != "" ||
		b.ExpectedModel != "" || b.ExpectedProvider != "" {
		return fmt.Errorf("route snapshot binding is valid only for OpenRouter")
	}
	return nil
}

func validateExistingExecutionAudit(
	result ResultRecord,
	system SystemConfig,
	binding ExecutionBinding,
	uncappedOutputTokenAllowance int64,
	directReturnedModel *string,
) error {
	audit := result.ExecutionAudit
	if audit.ManifestSHA256 != binding.ManifestSHA256 {
		return fmt.Errorf("different manifest identity")
	}
	if audit.Route.SnapshotSHA256 != binding.RouteSnapshotSHA256 {
		return fmt.Errorf("different route snapshot identity")
	}
	if audit.Route.SnapshotFetchedAt != binding.RouteSnapshotFetchedAt {
		return fmt.Errorf("different route snapshot fetched_at")
	}
	if audit.UncappedOutputTokenAllowance != uncappedOutputTokenAllowance {
		return fmt.Errorf("different uncapped output-token allowance")
	}
	if !reflect.DeepEqual(audit.Campaign, binding.Campaign) {
		return fmt.Errorf("different campaign run binding")
	}
	if !audit.Route.Verified {
		return nil
	}
	if system.Provider == "openrouter" {
		if normalizeRouteModel(audit.Route.ReturnedModel) !=
			normalizeRouteModel(binding.ExpectedModel) {
			return fmt.Errorf("returned a different model")
		}
		if normalizeRouteProvider(audit.Route.ReturnedProvider) !=
			normalizeRouteProvider(binding.ExpectedProvider) {
			return fmt.Errorf("returned a different provider")
		}
		return nil
	}
	concrete := strings.TrimSpace(audit.Route.ReturnedModel)
	if !directModelMatches(system.Model, concrete) {
		return fmt.Errorf(
			"returned concrete model %q does not resolve configured model %q",
			concrete,
			system.Model,
		)
	}
	if *directReturnedModel == "" {
		*directReturnedModel = concrete
		return nil
	}
	if concrete != *directReturnedModel {
		return fmt.Errorf(
			"returned concrete model %q differs from prior %q",
			concrete,
			*directReturnedModel,
		)
	}
	return nil
}

func auditCompletionRoute(
	system SystemConfig,
	binding ExecutionBinding,
	completion Completion,
	directReturnedModel *string,
) (ExecutionAudit, error) {
	route := RouteAudit{
		SnapshotSHA256:    binding.RouteSnapshotSHA256,
		SnapshotFetchedAt: binding.RouteSnapshotFetchedAt,
		ReturnedModel:     strings.TrimSpace(completion.ReturnedModel),
		ReturnedProvider:  strings.TrimSpace(completion.ReturnedProvider),
		Proof:             completion.RouteProof,
	}
	audit := ExecutionAudit{
		ManifestSHA256:        binding.ManifestSHA256,
		Route:                 route,
		CostAccountingVersion: CurrentCostAccountingVersion,
		Campaign:              cloneCampaignRunBinding(binding.Campaign),
	}
	if system.Provider == "openrouter" {
		switch completion.RouteProof {
		case RouteProofRouterMetadata, RouteProofGenerationMetadata:
		default:
			return audit, &CallError{Code: "route_unverifiable"}
		}
		if route.ReturnedModel == "" || route.ReturnedProvider == "" {
			return audit, &CallError{Code: "route_unverifiable"}
		}
		if normalizeRouteModel(route.ReturnedModel) !=
			normalizeRouteModel(binding.ExpectedModel) {
			return audit, &CallError{Code: "route_model_mismatch"}
		}
		if normalizeRouteProvider(route.ReturnedProvider) !=
			normalizeRouteProvider(binding.ExpectedProvider) {
			return audit, &CallError{Code: "route_provider_mismatch"}
		}
		audit.Route.Verified = true
		return audit, nil
	}

	// Direct APIs can resolve a stable alias to a dated concrete model. Bind
	// the run to the first concrete model returned and reject any drift across
	// both new and resumed rows.
	audit.Route.ReturnedProvider = ""
	if completion.RouteProof != RouteProofDirectResponseModel ||
		route.ReturnedModel == "" {
		return audit, &CallError{Code: "route_unverifiable"}
	}
	if !directModelMatches(system.Model, route.ReturnedModel) {
		return audit, &CallError{Code: "route_model_mismatch"}
	}
	if *directReturnedModel != "" && route.ReturnedModel != *directReturnedModel {
		return audit, &CallError{Code: "route_model_mismatch"}
	}
	if *directReturnedModel == "" {
		*directReturnedModel = route.ReturnedModel
	}
	audit.Route.Verified = true
	return audit, nil
}

func completionHasRouteEvidence(completion Completion) bool {
	return strings.TrimSpace(completion.ReturnedModel) != "" ||
		strings.TrimSpace(completion.ReturnedProvider) != "" ||
		completion.RouteProof != RouteProofUnverified
}

func normalizeRouteProvider(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func normalizeRouteModel(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// directModelMatches accepts either the exact configured OpenAI model or its
// dated concrete snapshot. It rejects a first response from a different model
// family instead of blessing whichever model happens to appear first.
func directModelMatches(configured, returned string) bool {
	configured = normalizeRouteModel(configured)
	returned = normalizeRouteModel(returned)
	if configured == "" || returned == "" {
		return false
	}
	if returned == configured {
		return true
	}
	if !strings.HasPrefix(returned, configured+"-") {
		return false
	}
	suffix := strings.TrimPrefix(returned, configured+"-")
	if len(suffix) != len("2006-01-02") {
		return false
	}
	for index, value := range suffix {
		switch index {
		case 4, 7:
			if value != '-' {
				return false
			}
		default:
			if value < '0' || value > '9' {
				return false
			}
		}
	}
	_, err := time.Parse("2006-01-02", suffix)
	return err == nil
}

// FilterCorpus derives a new immutable corpus containing only task. The
// derived SHA prevents results over a subset from being presented as results
// over the original full corpus.
func FilterCorpus(corpus Corpus, task Task, limit int) (Corpus, error) {
	if !validTask(task) {
		return Corpus{}, fmt.Errorf("unsupported task %q", task)
	}
	var records []CorpusRecord
	for _, record := range corpus.Records {
		if record.Task != task {
			continue
		}
		if limit > 0 && len(records) >= limit {
			break
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		return Corpus{}, fmt.Errorf("corpus has no %s cases", task)
	}
	return NewCorpus(records)
}

func estimateRequestCeiling(
	system SystemConfig,
	prompt PromptRequest,
	uncappedOutputTokenAllowance int64,
) int64 {
	schema, _ := json.Marshal(prompt.Schema)
	// One token per UTF-8 byte is deliberately conservative. It can stop early,
	// but cannot authorize an over-cap request based on an optimistic tokenizer
	// estimate.
	inputTokenCeiling := int64(len(prompt.System) + len(prompt.User) + len(schema) + 256)
	if specialized := prompt.SpecializedRerank; specialized != nil &&
		(system.APIKind == APIKindEmbedding || system.APIKind == APIKindRerank) {
		inputTokenCeiling = int64(len(specialized.Query) + 256)
		for _, document := range specialized.Documents {
			inputTokenCeiling += int64(len(document.Text))
		}
	}
	outputTokenCeiling := int64(completionTokenLimit(system, prompt))
	if isUncappedGenerativeSystem(system) {
		// The provider has no output cap. The caller's explicit conservative
		// allowance, validated before any request, is the only safe ceiling.
		outputTokenCeiling = uncappedOutputTokenAllowance
	}
	// The pre-call fuse cannot know whether the provider will classify input as
	// regular, cache-read, or cache-write tokens. Use the most expensive of the
	// three configured input tiers so a high cache-write price cannot make the
	// local ceiling optimistic.
	regularCost := system.EstimateCostWithCacheWriteMicroUSD(
		inputTokenCeiling,
		0,
		0,
		outputTokenCeiling,
	)
	cachedCost := system.EstimateCostWithCacheWriteMicroUSD(
		inputTokenCeiling,
		inputTokenCeiling,
		0,
		outputTokenCeiling,
	)
	cacheWriteCost := system.EstimateCostWithCacheWriteMicroUSD(
		inputTokenCeiling,
		0,
		inputTokenCeiling,
		outputTokenCeiling,
	)
	return max(regularCost, cachedCost, cacheWriteCost)
}
