package llmeval

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

const (
	OpenAIBatchCompletionWindow = "24h"
	OpenAIBatchPricingPPM       = int64(500_000)
	OpenAIBatchMaxRequests      = 50_000
	OpenAIBatchMaxInputBytes    = 200 << 20
	maxOpenAIBatchStateBytes    = 16 << 20
	maxOpenAIBatchResultBytes   = 1 << 30
)

type OpenAIBatchStateStatus string

const (
	OpenAIBatchStatePrepared        OpenAIBatchStateStatus = "prepared"
	OpenAIBatchStateUploadUncertain OpenAIBatchStateStatus = "upload_uncertain"
	OpenAIBatchStateUploaded        OpenAIBatchStateStatus = "uploaded"
	OpenAIBatchStateSubmitUncertain OpenAIBatchStateStatus = "submission_uncertain"
	OpenAIBatchStateSubmitted       OpenAIBatchStateStatus = "submitted"
	OpenAIBatchStateReconcileRetry  OpenAIBatchStateStatus = "reconciliation_retry_required"
	OpenAIBatchStateCompleted       OpenAIBatchStateStatus = "completed"
	OpenAIBatchStateProviderFailed  OpenAIBatchStateStatus = "provider_failed"
	OpenAIBatchStatePrivacyBlocked  OpenAIBatchStateStatus = "privacy_blocked"
	OpenAIBatchStateCleanupRequired OpenAIBatchStateStatus = "cleanup_required"
)

type OpenAIBatchTerminalErrorCode string

const (
	OpenAIBatchTerminalErrorUnsupportedStatus  OpenAIBatchTerminalErrorCode = "unsupported_provider_status"
	OpenAIBatchTerminalErrorIncompleteCounts   OpenAIBatchTerminalErrorCode = "incomplete_request_counts"
	OpenAIBatchTerminalErrorOutputDownload     OpenAIBatchTerminalErrorCode = "output_download_failed"
	OpenAIBatchTerminalErrorErrorDownload      OpenAIBatchTerminalErrorCode = "error_download_failed"
	OpenAIBatchTerminalErrorNoArtifacts        OpenAIBatchTerminalErrorCode = "no_accounting_artifacts"
	OpenAIBatchTerminalErrorUnverifiable       OpenAIBatchTerminalErrorCode = "accounting_artifacts_unverifiable"
	OpenAIBatchTerminalErrorResultCanonical    OpenAIBatchTerminalErrorCode = "result_canonicalization_failed"
	OpenAIBatchTerminalErrorResultWrite        OpenAIBatchTerminalErrorCode = "result_persistence_failed"
	OpenAIBatchTerminalErrorLegacyUnverifiable OpenAIBatchTerminalErrorCode = "legacy_unverifiable_terminal"
)

type OpenAIBatchCaseBinding struct {
	CustomID              string `json:"custom_id"`
	CaseID                string `json:"case_id"`
	RequestContractSHA256 string `json:"request_contract_sha256"`
}

// OpenAIBatchState is the resume journal. PlanSHA256 covers every immutable
// field that authorizes spend. Provider IDs, lifecycle status, and terminal
// accounting fields are mutable and are persisted atomically by the CLI after
// each provider mutation.
type OpenAIBatchState struct {
	SchemaVersion            int                          `json:"schema_version"`
	PlanSHA256               string                       `json:"plan_sha256"`
	Status                   OpenAIBatchStateStatus       `json:"status"`
	CleanupFinalStatus       OpenAIBatchStateStatus       `json:"cleanup_final_status,omitempty"`
	CorpusSHA256             string                       `json:"corpus_sha256"`
	ManifestSHA256           string                       `json:"manifest_sha256"`
	GoldClosureSHA256        string                       `json:"gold_closure_sha256,omitempty"`
	PrivacySidecarSHA256     string                       `json:"privacy_sidecar_sha256,omitempty"`
	EvaluatorBuildSHA256     string                       `json:"evaluator_build_sha256"`
	System                   SystemDescriptor             `json:"system"`
	Task                     Task                         `json:"task"`
	Endpoint                 string                       `json:"endpoint"`
	InputFileSHA256          string                       `json:"input_file_sha256"`
	InputFilename            string                       `json:"input_filename"`
	RequestCount             int                          `json:"request_count"`
	CostFuseMicroUSD         int64                        `json:"cost_fuse_micro_usd"`
	EstimatedCeilingMicroUSD int64                        `json:"estimated_ceiling_micro_usd"`
	PricingMultiplierPPM     int64                        `json:"pricing_multiplier_ppm"`
	UncappedOutputAllowance  int64                        `json:"uncapped_output_allowance,omitempty"`
	Cases                    []OpenAIBatchCaseBinding     `json:"cases"`
	InputFileID              string                       `json:"input_file_id,omitempty"`
	DeletedInputFileID       string                       `json:"deleted_input_file_id,omitempty"`
	InputFileDeleted         bool                         `json:"input_file_deleted,omitempty"`
	BatchID                  string                       `json:"batch_id,omitempty"`
	ProviderStatus           string                       `json:"provider_status,omitempty"`
	TerminalErrorCode        OpenAIBatchTerminalErrorCode `json:"terminal_error_code,omitempty"`
	OutputFileID             string                       `json:"output_file_id,omitempty"`
	ErrorFileID              string                       `json:"error_file_id,omitempty"`
	DeletedOutputFileID      string                       `json:"deleted_output_file_id,omitempty"`
	DeletedErrorFileID       string                       `json:"deleted_error_file_id,omitempty"`
	OutputFileDeleted        bool                         `json:"output_file_deleted,omitempty"`
	ErrorFileDeleted         bool                         `json:"error_file_deleted,omitempty"`
	ResultsSHA256            string                       `json:"results_sha256,omitempty"`
	AccountedCostMicroUSD    int64                        `json:"accounted_cost_micro_usd,omitempty"`
	ActualCostMicroUSD       int64                        `json:"actual_cost_micro_usd,omitempty"`
	CostCoverageRequests     int                          `json:"cost_coverage_requests,omitempty"`
	ActualCostUnknown        bool                         `json:"actual_cost_unknown,omitempty"`
}

type openAIBatchPlanIdentity struct {
	Domain                   string                   `json:"domain"`
	CorpusSHA256             string                   `json:"corpus_sha256"`
	ManifestSHA256           string                   `json:"manifest_sha256"`
	GoldClosureSHA256        string                   `json:"gold_closure_sha256,omitempty"`
	PrivacySidecarSHA256     string                   `json:"privacy_sidecar_sha256,omitempty"`
	EvaluatorBuildSHA256     string                   `json:"evaluator_build_sha256"`
	System                   SystemDescriptor         `json:"system"`
	Task                     Task                     `json:"task"`
	Endpoint                 string                   `json:"endpoint"`
	InputFileSHA256          string                   `json:"input_file_sha256"`
	RequestCount             int                      `json:"request_count"`
	CostFuseMicroUSD         int64                    `json:"cost_fuse_micro_usd"`
	EstimatedCeilingMicroUSD int64                    `json:"estimated_ceiling_micro_usd"`
	PricingMultiplierPPM     int64                    `json:"pricing_multiplier_ppm"`
	UncappedOutputAllowance  int64                    `json:"uncapped_output_allowance,omitempty"`
	Cases                    []OpenAIBatchCaseBinding `json:"cases"`
}

type openAIBatchInputLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

type OpenAIBatchBuildOptions struct {
	ManifestSHA256          string
	GoldClosureSHA256       string
	PrivacySidecarSHA256    string
	EvaluatorBuildSHA256    string
	MaxCostMicroUSD         int64
	UncappedOutputAllowance int64
	Thresholds              DecisionThresholds
}

// BuildOpenAIBatchState builds exact canonical Batch JSONL for one complete
// task corpus. It never truncates or samples: staged subsets must first be
// frozen as their own immutable corpus.
func BuildOpenAIBatchState(
	corpus Corpus,
	system SystemConfig,
	options OpenAIBatchBuildOptions,
) (OpenAIBatchState, []byte, error) {
	return buildOpenAIBatchState(corpus, system, options, true)
}

func buildOpenAIBatchState(
	corpus Corpus,
	system SystemConfig,
	options OpenAIBatchBuildOptions,
	enforceNewPlanEligibility bool,
) (OpenAIBatchState, []byte, error) {
	canonical, err := NewCorpus(corpus.Records)
	if err != nil {
		return OpenAIBatchState{}, nil, err
	}
	if corpus.SHA256 != "" && corpus.SHA256 != canonical.SHA256 {
		return OpenAIBatchState{}, nil, fmt.Errorf("corpus identity mismatch")
	}
	if err := ValidateGoldCorpus(canonical); err != nil {
		return OpenAIBatchState{}, nil, err
	}
	if err := ValidateHostedCorpusPrivacy(canonical); err != nil {
		return OpenAIBatchState{}, nil, err
	}
	if err := system.Validate(); err != nil {
		return OpenAIBatchState{}, nil, fmt.Errorf("system: %w", err)
	}
	if system.Provider != "openai" {
		return OpenAIBatchState{}, nil, fmt.Errorf(
			"OpenAI Batch controls require provider=openai",
		)
	}
	if enforceNewPlanEligibility {
		if err := ValidateOpenAIBatchNewSubmissionSystem(system); err != nil {
			return OpenAIBatchState{}, nil, err
		}
	}
	if system.APIKind != APIKindChat && system.APIKind != APIKindResponses {
		return OpenAIBatchState{}, nil, fmt.Errorf(
			"OpenAI Batch controls require Chat or Responses",
		)
	}
	if len(canonical.Records) > OpenAIBatchMaxRequests {
		return OpenAIBatchState{}, nil, fmt.Errorf(
			"batch has %d requests, maximum is %d",
			len(canonical.Records),
			OpenAIBatchMaxRequests,
		)
	}
	if options.MaxCostMicroUSD <= 0 {
		return OpenAIBatchState{}, nil, fmt.Errorf(
			"positive max cost fuse is required",
		)
	}
	if err := options.Thresholds.Validate(); err != nil {
		return OpenAIBatchState{}, nil, err
	}
	for name, value := range map[string]string{
		"manifest_sha256":        options.ManifestSHA256,
		"evaluator_build_sha256": options.EvaluatorBuildSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return OpenAIBatchState{}, nil, err
		}
	}
	if options.GoldClosureSHA256 != "" {
		if err := validateSHA256(
			"gold_closure_sha256",
			options.GoldClosureSHA256,
		); err != nil {
			return OpenAIBatchState{}, nil, err
		}
	}
	if options.PrivacySidecarSHA256 != "" {
		if err := validateSHA256(
			"privacy_sidecar_sha256",
			options.PrivacySidecarSHA256,
		); err != nil {
			return OpenAIBatchState{}, nil, err
		}
	}

	task := canonical.Records[0].Task
	for _, record := range canonical.Records {
		if record.Task != task {
			return OpenAIBatchState{}, nil, fmt.Errorf(
				"OpenAI Batch state requires one complete task corpus",
			)
		}
		if !system.SupportsTask(record.Task) {
			return OpenAIBatchState{}, nil, fmt.Errorf(
				"system %q does not support task %q",
				system.SystemID,
				record.Task,
			)
		}
	}

	var (
		input     bytes.Buffer
		bindings  = make([]OpenAIBatchCaseBinding, 0, len(canonical.Records))
		estimated int64
		endpoint  string
	)
	for _, record := range canonical.Records {
		prompt, err := BuildPrompt(record)
		if err != nil {
			return OpenAIBatchState{}, nil, err
		}
		lineEndpoint, body, err := GenerativeRequestBody(system, prompt)
		if err != nil {
			return OpenAIBatchState{}, nil, err
		}
		if endpoint == "" {
			endpoint = lineEndpoint
		} else if endpoint != lineEndpoint {
			return OpenAIBatchState{}, nil, fmt.Errorf(
				"batch input contains mixed endpoints",
			)
		}
		contract, err := RequestContractSHA256(
			record,
			system,
			options.Thresholds,
		)
		if err != nil {
			return OpenAIBatchState{}, nil, err
		}
		customID := openAIBatchCustomID(canonical.SHA256, record.CaseID, contract)
		bodyJSON, err := json.Marshal(body)
		if err != nil {
			return OpenAIBatchState{}, nil, fmt.Errorf(
				"case %q request body: %w",
				record.CaseID,
				err,
			)
		}
		lineJSON, err := json.Marshal(openAIBatchInputLine{
			CustomID: customID,
			Method:   "POST",
			URL:      endpoint,
			Body:     bodyJSON,
		})
		if err != nil {
			return OpenAIBatchState{}, nil, fmt.Errorf(
				"case %q batch line: %w",
				record.CaseID,
				err,
			)
		}
		input.Write(lineJSON)
		input.WriteByte('\n')
		bindings = append(bindings, OpenAIBatchCaseBinding{
			CustomID:              customID,
			CaseID:                record.CaseID,
			RequestContractSHA256: contract,
		})

		ceiling, err := openAIBatchRequestCeiling(
			system,
			prompt,
			options.UncappedOutputAllowance,
			enforceNewPlanEligibility,
		)
		if err != nil {
			return OpenAIBatchState{}, nil, fmt.Errorf(
				"case %q cost fuse: %w",
				record.CaseID,
				err,
			)
		}
		if estimated > math.MaxInt64-ceiling {
			return OpenAIBatchState{}, nil, fmt.Errorf(
				"batch cost ceiling overflows",
			)
		}
		estimated += ceiling
	}
	if input.Len() > OpenAIBatchMaxInputBytes {
		return OpenAIBatchState{}, nil, fmt.Errorf(
			"batch input is %d bytes, maximum is %d",
			input.Len(),
			OpenAIBatchMaxInputBytes,
		)
	}
	if estimated > options.MaxCostMicroUSD {
		return OpenAIBatchState{}, nil, fmt.Errorf(
			"conservative Batch ceiling $%.6f exceeds explicit cost fuse $%.6f",
			float64(estimated)/1_000_000,
			float64(options.MaxCostMicroUSD)/1_000_000,
		)
	}

	inputBytes := input.Bytes()
	inputSHA := sha256Hex(inputBytes)
	state := OpenAIBatchState{
		SchemaVersion:            SchemaVersion,
		Status:                   OpenAIBatchStatePrepared,
		CorpusSHA256:             canonical.SHA256,
		ManifestSHA256:           options.ManifestSHA256,
		GoldClosureSHA256:        options.GoldClosureSHA256,
		PrivacySidecarSHA256:     options.PrivacySidecarSHA256,
		EvaluatorBuildSHA256:     options.EvaluatorBuildSHA256,
		System:                   system.Descriptor(),
		Task:                     task,
		Endpoint:                 endpoint,
		InputFileSHA256:          inputSHA,
		RequestCount:             len(bindings),
		CostFuseMicroUSD:         options.MaxCostMicroUSD,
		EstimatedCeilingMicroUSD: estimated,
		PricingMultiplierPPM:     OpenAIBatchPricingPPM,
		UncappedOutputAllowance:  options.UncappedOutputAllowance,
		Cases:                    bindings,
	}
	state.PlanSHA256, err = openAIBatchPlanSHA256(state)
	if err != nil {
		return OpenAIBatchState{}, nil, err
	}
	state.InputFilename = "bitagent-llmeval-" + state.PlanSHA256 + ".jsonl"
	if err := state.Validate(); err != nil {
		return OpenAIBatchState{}, nil, err
	}
	return state, append([]byte(nil), inputBytes...), nil
}

// ValidateOpenAIBatchNewSubmissionSystem rejects systems whose evidence would
// misrepresent the live synchronous action contract. Historical state/input
// verification deliberately bypasses this gate, but every provider mutation
// that could create a new Batch must enforce it.
func ValidateOpenAIBatchNewSubmissionSystem(system SystemConfig) error {
	if system.EvaluationLane == EvaluationLaneProductionFidelity {
		return fmt.Errorf(
			"OpenAI Batch cannot produce production_fidelity evidence; use synchronous run evidence",
		)
	}
	if system.RequestTimeoutMS > 0 {
		return fmt.Errorf(
			"OpenAI Batch cannot reproduce a manifest-bound request timeout; use synchronous run evidence",
		)
	}
	return nil
}

// ValidateOpenAIBatchNewSubmissionState applies the mutation-only policy to a
// persisted prepared/uploaded plan. Historical verification accepts legacy
// zero-allowance artifacts, but they must never be used to upload a new file or
// create a new uncapped provider Batch.
func ValidateOpenAIBatchNewSubmissionState(
	state OpenAIBatchState,
	system SystemConfig,
) error {
	if err := ValidateOpenAIBatchNewSubmissionSystem(system); err != nil {
		return err
	}
	if isUncappedGenerativeSystem(system) {
		if state.UncappedOutputAllowance <= 0 {
			return fmt.Errorf(
				"uncapped generative Batch submission requires a bound positive output allowance",
			)
		}
	} else if state.UncappedOutputAllowance != 0 {
		return fmt.Errorf(
			"capped Batch submission cannot carry an uncapped output allowance",
		)
	}
	return nil
}

func openAIBatchRequestCeiling(
	system SystemConfig,
	prompt PromptRequest,
	uncappedOutputAllowance int64,
	requireUncappedOutputAllowance bool,
) (int64, error) {
	if isUncappedGenerativeSystem(system) {
		if requireUncappedOutputAllowance &&
			uncappedOutputAllowance < int64(prompt.MaxCompletionTokens) {
			return 0, fmt.Errorf(
				"uncapped generative request requires output allowance >= %d tokens",
				prompt.MaxCompletionTokens,
			)
		} else if uncappedOutputAllowance != 0 &&
			uncappedOutputAllowance < int64(prompt.MaxCompletionTokens) {
			return 0, fmt.Errorf(
				"uncapped output allowance must be zero for legacy verification or >= %d tokens",
				prompt.MaxCompletionTokens,
			)
		}
	} else if uncappedOutputAllowance != 0 {
		return 0, fmt.Errorf(
			"uncapped output allowance is valid only when a generative request omits its cap",
		)
	}
	schema, _ := json.Marshal(prompt.Schema)
	inputCeiling := int64(
		len(prompt.System) + len(prompt.User) + len(schema) + 256,
	)
	outputCeiling := int64(completionTokenLimit(system, prompt))
	if isUncappedGenerativeSystem(system) && uncappedOutputAllowance > 0 {
		outputCeiling = uncappedOutputAllowance
	}
	batchSystem := openAIBatchPricedSystem(system)
	regular := batchSystem.EstimateCostWithCacheWriteMicroUSD(
		inputCeiling,
		0,
		0,
		outputCeiling,
	)
	cached := batchSystem.EstimateCostWithCacheWriteMicroUSD(
		inputCeiling,
		inputCeiling,
		0,
		outputCeiling,
	)
	cacheWrite := batchSystem.EstimateCostWithCacheWriteMicroUSD(
		inputCeiling,
		0,
		inputCeiling,
		outputCeiling,
	)
	return max(regular, cached, cacheWrite), nil
}

func openAIBatchPricedSystem(system SystemConfig) SystemConfig {
	system.InputUSDPerMillion *= 0.5
	system.CachedInputUSDPerMillion *= 0.5
	system.CacheWriteUSDPerMillion *= 0.5
	system.OutputUSDPerMillion *= 0.5
	system.USDPerRequest *= 0.5
	return system
}

func openAIBatchCustomID(corpusSHA, caseID, contractSHA string) string {
	payload := "bitagent-llmeval-openai-batch-custom-id-v1\x00" +
		corpusSHA + "\x00" + caseID + "\x00" + contractSHA
	sum := sha256.Sum256([]byte(payload))
	return "bae_" + hex.EncodeToString(sum[:])
}

func openAIBatchPlanSHA256(state OpenAIBatchState) (string, error) {
	raw, err := json.Marshal(openAIBatchPlanIdentity{
		Domain:                   "bitagent-llmeval-openai-batch-plan-v1",
		CorpusSHA256:             state.CorpusSHA256,
		ManifestSHA256:           state.ManifestSHA256,
		GoldClosureSHA256:        state.GoldClosureSHA256,
		PrivacySidecarSHA256:     state.PrivacySidecarSHA256,
		EvaluatorBuildSHA256:     state.EvaluatorBuildSHA256,
		System:                   state.System,
		Task:                     state.Task,
		Endpoint:                 state.Endpoint,
		InputFileSHA256:          state.InputFileSHA256,
		RequestCount:             state.RequestCount,
		CostFuseMicroUSD:         state.CostFuseMicroUSD,
		EstimatedCeilingMicroUSD: state.EstimatedCeilingMicroUSD,
		PricingMultiplierPPM:     state.PricingMultiplierPPM,
		UncappedOutputAllowance:  state.UncappedOutputAllowance,
		Cases:                    state.Cases,
	})
	if err != nil {
		return "", fmt.Errorf("encode Batch plan identity: %w", err)
	}
	return sha256Hex(raw), nil
}

func (state OpenAIBatchState) Validate() error {
	if state.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"Batch state schema_version got %d, want %d",
			state.SchemaVersion,
			SchemaVersion,
		)
	}
	for name, value := range map[string]string{
		"plan_sha256":            state.PlanSHA256,
		"corpus_sha256":          state.CorpusSHA256,
		"manifest_sha256":        state.ManifestSHA256,
		"evaluator_build_sha256": state.EvaluatorBuildSHA256,
		"input_file_sha256":      state.InputFileSHA256,
	} {
		if err := validateSHA256("Batch state."+name, value); err != nil {
			return err
		}
	}
	if state.GoldClosureSHA256 != "" {
		if err := validateSHA256(
			"Batch state.gold_closure_sha256",
			state.GoldClosureSHA256,
		); err != nil {
			return err
		}
	}
	if state.PrivacySidecarSHA256 != "" {
		if err := validateSHA256(
			"Batch state.privacy_sidecar_sha256",
			state.PrivacySidecarSHA256,
		); err != nil {
			return err
		}
	}
	if err := state.System.Validate(); err != nil {
		return fmt.Errorf("Batch state.system: %w", err)
	}
	if state.System.Provider != "openai" {
		return fmt.Errorf("Batch state is not a direct OpenAI system")
	}
	if !validTask(state.Task) {
		return fmt.Errorf("Batch state has unsupported task %q", state.Task)
	}
	switch state.Endpoint {
	case "/v1/chat/completions", "/v1/responses":
	default:
		return fmt.Errorf("Batch state has unsupported endpoint %q", state.Endpoint)
	}
	if state.PricingMultiplierPPM != OpenAIBatchPricingPPM {
		return fmt.Errorf("Batch state pricing multiplier must be 500000 ppm")
	}
	if state.CostFuseMicroUSD <= 0 ||
		state.EstimatedCeilingMicroUSD < 0 ||
		state.EstimatedCeilingMicroUSD > state.CostFuseMicroUSD {
		return fmt.Errorf("Batch state has invalid cost fuse")
	}
	if state.RequestCount <= 0 ||
		state.RequestCount != len(state.Cases) ||
		state.RequestCount > OpenAIBatchMaxRequests {
		return fmt.Errorf("Batch state has invalid request count")
	}
	if state.InputFilename != "bitagent-llmeval-"+state.PlanSHA256+".jsonl" {
		return fmt.Errorf("Batch state input filename is not plan-derived")
	}
	switch state.Status {
	case OpenAIBatchStatePrepared,
		OpenAIBatchStateUploadUncertain,
		OpenAIBatchStateUploaded,
		OpenAIBatchStateSubmitUncertain,
		OpenAIBatchStateSubmitted,
		OpenAIBatchStateReconcileRetry,
		OpenAIBatchStateCompleted,
		OpenAIBatchStateProviderFailed,
		OpenAIBatchStatePrivacyBlocked,
		OpenAIBatchStateCleanupRequired:
	default:
		return fmt.Errorf("Batch state has unsupported status %q", state.Status)
	}
	switch state.CleanupFinalStatus {
	case "":
		if state.Status == OpenAIBatchStateCleanupRequired {
			return fmt.Errorf(
				"Batch cleanup_required state must name its final status",
			)
		}
	case OpenAIBatchStateCompleted,
		OpenAIBatchStateProviderFailed,
		OpenAIBatchStatePrivacyBlocked:
		if state.Status != OpenAIBatchStateCleanupRequired {
			return fmt.Errorf(
				"Batch cleanup_final_status is valid only while cleanup is required",
			)
		}
	default:
		return fmt.Errorf(
			"Batch state has unsupported cleanup_final_status %q",
			state.CleanupFinalStatus,
		)
	}
	switch state.TerminalErrorCode {
	case "",
		OpenAIBatchTerminalErrorUnsupportedStatus,
		OpenAIBatchTerminalErrorIncompleteCounts,
		OpenAIBatchTerminalErrorOutputDownload,
		OpenAIBatchTerminalErrorErrorDownload,
		OpenAIBatchTerminalErrorNoArtifacts,
		OpenAIBatchTerminalErrorUnverifiable,
		OpenAIBatchTerminalErrorResultCanonical,
		OpenAIBatchTerminalErrorResultWrite,
		OpenAIBatchTerminalErrorLegacyUnverifiable:
	default:
		return fmt.Errorf(
			"Batch state has unsupported terminal_error_code %q",
			state.TerminalErrorCode,
		)
	}
	seenCustom := make(map[string]struct{}, len(state.Cases))
	seenCase := make(map[string]struct{}, len(state.Cases))
	for index, binding := range state.Cases {
		if err := validateIdentifier(
			fmt.Sprintf("Batch state.cases[%d].custom_id", index),
			binding.CustomID,
		); err != nil {
			return err
		}
		if err := validateIdentifier(
			fmt.Sprintf("Batch state.cases[%d].case_id", index),
			binding.CaseID,
		); err != nil {
			return err
		}
		if err := validateSHA256(
			fmt.Sprintf("Batch state.cases[%d].request_contract_sha256", index),
			binding.RequestContractSHA256,
		); err != nil {
			return err
		}
		if _, duplicate := seenCustom[binding.CustomID]; duplicate {
			return fmt.Errorf("Batch state contains duplicate custom_id")
		}
		if _, duplicate := seenCase[binding.CaseID]; duplicate {
			return fmt.Errorf("Batch state contains duplicate case_id")
		}
		seenCustom[binding.CustomID] = struct{}{}
		seenCase[binding.CaseID] = struct{}{}
	}
	for name, value := range map[string]string{
		"input_file_id":          state.InputFileID,
		"deleted_input_file_id":  state.DeletedInputFileID,
		"batch_id":               state.BatchID,
		"output_file_id":         state.OutputFileID,
		"error_file_id":          state.ErrorFileID,
		"deleted_output_file_id": state.DeletedOutputFileID,
		"deleted_error_file_id":  state.DeletedErrorFileID,
		"provider_status":        state.ProviderStatus,
	} {
		if value != "" {
			if err := validateIdentifier("Batch state."+name, value); err != nil {
				return err
			}
		}
	}
	if state.InputFileDeleted {
		if state.DeletedInputFileID == "" || state.InputFileID != "" {
			return fmt.Errorf(
				"Batch state deleted input requires only deleted_input_file_id",
			)
		}
	} else if state.DeletedInputFileID != "" {
		return fmt.Errorf(
			"Batch state deleted_input_file_id requires input_file_deleted",
		)
	}
	if err := validateOpenAIBatchDeletedFileState(
		"output",
		state.OutputFileID,
		state.DeletedOutputFileID,
		state.OutputFileDeleted,
	); err != nil {
		return err
	}
	if err := validateOpenAIBatchDeletedFileState(
		"error",
		state.ErrorFileID,
		state.DeletedErrorFileID,
		state.ErrorFileDeleted,
	); err != nil {
		return err
	}
	if state.ResultsSHA256 != "" {
		if err := validateSHA256(
			"Batch state.results_sha256",
			state.ResultsSHA256,
		); err != nil {
			return err
		}
	}
	if state.AccountedCostMicroUSD < 0 || state.ActualCostMicroUSD < 0 {
		return fmt.Errorf("Batch state costs cannot be negative")
	}
	if state.CostCoverageRequests < 0 ||
		state.CostCoverageRequests > state.RequestCount {
		return fmt.Errorf("Batch state cost coverage is invalid")
	}
	if (state.CostCoverageRequests == 0) !=
		(state.ResultsSHA256 == "") {
		return fmt.Errorf(
			"Batch state result identity and cost coverage must be present together",
		)
	}
	if state.CostCoverageRequests == 0 &&
		state.AccountedCostMicroUSD != 0 {
		return fmt.Errorf(
			"Batch state cannot account cost without covered requests",
		)
	}
	terminalStatus := state.Status
	if state.Status == OpenAIBatchStateCleanupRequired {
		terminalStatus = state.CleanupFinalStatus
	}
	retryError := state.TerminalErrorCode ==
		OpenAIBatchTerminalErrorUnsupportedStatus ||
		state.TerminalErrorCode ==
			OpenAIBatchTerminalErrorIncompleteCounts ||
		state.TerminalErrorCode ==
			OpenAIBatchTerminalErrorOutputDownload ||
		state.TerminalErrorCode ==
			OpenAIBatchTerminalErrorErrorDownload ||
		state.TerminalErrorCode ==
			OpenAIBatchTerminalErrorResultCanonical ||
		state.TerminalErrorCode ==
			OpenAIBatchTerminalErrorResultWrite
	if retryError &&
		(state.Status != OpenAIBatchStateReconcileRetry ||
			!state.ActualCostUnknown) {
		return fmt.Errorf(
			"retryable terminal_error_code requires reconciliation retry with unknown actual cost",
		)
	}
	if !retryError &&
		state.TerminalErrorCode != "" &&
		(terminalStatus != OpenAIBatchStateProviderFailed ||
			!state.ActualCostUnknown) {
		return fmt.Errorf(
			"Batch terminal_error_code requires provider_failed with unknown actual cost",
		)
	}
	if state.Status == OpenAIBatchStateReconcileRetry &&
		state.TerminalErrorCode == "" {
		return fmt.Errorf(
			"Batch reconciliation retry requires terminal_error_code",
		)
	}
	if terminalStatus == OpenAIBatchStateCompleted &&
		(state.ActualCostUnknown ||
			state.CostCoverageRequests != state.RequestCount ||
			state.ResultsSHA256 == "" ||
			state.ActualCostMicroUSD != state.AccountedCostMicroUSD) {
		return fmt.Errorf(
			"completed Batch state requires exact full cost and result coverage",
		)
	}
	if (state.Status == OpenAIBatchStateCompleted ||
		(state.Status == OpenAIBatchStateProviderFailed &&
			state.ResultsSHA256 != "") ||
		state.Status == OpenAIBatchStatePrivacyBlocked) &&
		(state.InputFileID != "" ||
			state.OutputFileID != "" ||
			state.ErrorFileID != "") {
		return fmt.Errorf(
			"terminal Batch state cannot retain active provider file IDs",
		)
	}
	if state.ActualCostMicroUSD != 0 &&
		(state.ActualCostUnknown ||
			state.CostCoverageRequests != state.RequestCount ||
			state.ActualCostMicroUSD != state.AccountedCostMicroUSD) {
		return fmt.Errorf("Batch state exact actual cost is not fully covered")
	}
	wantPlan, err := openAIBatchPlanSHA256(state)
	if err != nil {
		return err
	}
	if state.PlanSHA256 != wantPlan {
		return fmt.Errorf("Batch state immutable plan identity mismatch")
	}
	return nil
}

func validateOpenAIBatchDeletedFileState(
	kind,
	activeID,
	deletedID string,
	deleted bool,
) error {
	if deleted {
		if deletedID == "" || activeID != "" {
			return fmt.Errorf(
				"Batch state deleted %s requires only deleted_%s_file_id",
				kind,
				kind,
			)
		}
		return nil
	}
	if deletedID != "" {
		return fmt.Errorf(
			"Batch state deleted_%s_file_id requires %s_file_deleted",
			kind,
			kind,
		)
	}
	return nil
}

func MarshalOpenAIBatchState(state OpenAIBatchState) ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode Batch state: %w", err)
	}
	return append(raw, '\n'), nil
}

func ReadOpenAIBatchState(r io.Reader) (OpenAIBatchState, string, error) {
	if r == nil {
		return OpenAIBatchState{}, "", fmt.Errorf("Batch state reader is nil")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxOpenAIBatchStateBytes+1))
	if err != nil {
		return OpenAIBatchState{}, "", fmt.Errorf("read Batch state: %w", err)
	}
	if len(raw) == 0 || len(raw) > maxOpenAIBatchStateBytes {
		return OpenAIBatchState{}, "", fmt.Errorf("Batch state size is invalid")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return OpenAIBatchState{}, "", fmt.Errorf("decode Batch state: %w", err)
	}
	var state OpenAIBatchState
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return OpenAIBatchState{}, "", fmt.Errorf("decode Batch state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return OpenAIBatchState{}, "", fmt.Errorf("decode Batch state: trailing data")
	}
	upgradeLegacyOpenAIBatchCleanupState(&state)
	if err := state.Validate(); err != nil {
		return OpenAIBatchState{}, "", err
	}
	return state, sha256Hex(raw), nil
}

// upgradeLegacyOpenAIBatchCleanupState keeps state files written before
// terminal provider-file cleanup was generalized resumable. The old
// cleanup_required status was used only for privacy revocation and had no
// explicit target. Old reconciled terminal states also retained provider file
// IDs. Neither transition changes the immutable plan identity.
func upgradeLegacyOpenAIBatchCleanupState(state *OpenAIBatchState) {
	if state == nil || state.CleanupFinalStatus != "" {
		return
	}
	if state.Status == OpenAIBatchStateCleanupRequired {
		state.CleanupFinalStatus = OpenAIBatchStatePrivacyBlocked
		return
	}
	hasActiveFiles := state.InputFileID != "" ||
		state.OutputFileID != "" ||
		state.ErrorFileID != ""
	if !hasActiveFiles {
		return
	}
	switch state.Status {
	case OpenAIBatchStateCompleted, OpenAIBatchStatePrivacyBlocked:
		state.CleanupFinalStatus = state.Status
		state.Status = OpenAIBatchStateCleanupRequired
	case OpenAIBatchStateProviderFailed:
		if state.ResultsSHA256 != "" ||
			openAIBatchStateProviderTerminal(state.ProviderStatus) {
			if state.ResultsSHA256 == "" &&
				state.TerminalErrorCode == "" {
				state.TerminalErrorCode =
					OpenAIBatchTerminalErrorLegacyUnverifiable
				state.ActualCostUnknown = true
			}
			state.CleanupFinalStatus = state.Status
			state.Status = OpenAIBatchStateCleanupRequired
		}
	}
}

func openAIBatchStateProviderTerminal(status string) bool {
	switch status {
	case "completed", "failed", "expired", "cancelled":
		return true
	default:
		return false
	}
}

func ValidateOpenAIBatchInput(
	state OpenAIBatchState,
	input []byte,
	corpus Corpus,
	system SystemConfig,
	thresholds DecisionThresholds,
) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if len(input) > OpenAIBatchMaxInputBytes ||
		sha256Hex(input) != state.InputFileSHA256 {
		return fmt.Errorf("Batch input bytes do not match state")
	}
	rebuilt, expected, err := buildOpenAIBatchState(
		corpus,
		system,
		OpenAIBatchBuildOptions{
			ManifestSHA256:          state.ManifestSHA256,
			GoldClosureSHA256:       state.GoldClosureSHA256,
			PrivacySidecarSHA256:    state.PrivacySidecarSHA256,
			EvaluatorBuildSHA256:    state.EvaluatorBuildSHA256,
			MaxCostMicroUSD:         state.CostFuseMicroUSD,
			UncappedOutputAllowance: state.UncappedOutputAllowance,
			Thresholds:              thresholds,
		},
		false,
	)
	if err != nil {
		return err
	}
	if rebuilt.PlanSHA256 != state.PlanSHA256 || !bytes.Equal(input, expected) {
		return fmt.Errorf("Batch input is not the canonical request plan")
	}
	return nil
}

type openAIBatchOutputLine struct {
	ID       string `json:"id"`
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int             `json:"status_code"`
		RequestID  string          `json:"request_id"`
		Body       json.RawMessage `json:"body"`
	} `json:"response"`
	Error json.RawMessage `json:"error"`
}

type OpenAIBatchReconcileSummary struct {
	Results      []ResultRecord
	CostMicroUSD int64
	Covered      int
	Completed    int
	Failed       int
}

// ReconcileOpenAIBatchOutput joins unordered output/error JSONL by custom_id,
// requires exact one-to-one coverage, verifies each returned concrete model,
// and prices provider usage at the documented 50% Batch multiplier.
func ReconcileOpenAIBatchOutput(
	state OpenAIBatchState,
	corpus Corpus,
	system SystemConfig,
	batchID string,
	outputJSONL []byte,
	errorJSONL []byte,
	thresholds DecisionThresholds,
) (OpenAIBatchReconcileSummary, error) {
	return reconcileOpenAIBatchOutput(
		state,
		corpus,
		system,
		batchID,
		outputJSONL,
		errorJSONL,
		thresholds,
		true,
	)
}

// ReconcileOpenAIBatchPartialOutput accounts every trustworthy line available
// from a non-success terminal Batch. Missing custom IDs remain explicit through
// Covered and must never be presented as exact actual cost.
func ReconcileOpenAIBatchPartialOutput(
	state OpenAIBatchState,
	corpus Corpus,
	system SystemConfig,
	batchID string,
	outputJSONL []byte,
	errorJSONL []byte,
	thresholds DecisionThresholds,
) (OpenAIBatchReconcileSummary, error) {
	return reconcileOpenAIBatchOutput(
		state,
		corpus,
		system,
		batchID,
		outputJSONL,
		errorJSONL,
		thresholds,
		false,
	)
}

func reconcileOpenAIBatchOutput(
	state OpenAIBatchState,
	corpus Corpus,
	system SystemConfig,
	batchID string,
	outputJSONL []byte,
	errorJSONL []byte,
	thresholds DecisionThresholds,
	requireFullCoverage bool,
) (OpenAIBatchReconcileSummary, error) {
	if err := state.Validate(); err != nil {
		return OpenAIBatchReconcileSummary{}, err
	}
	if batchID == "" || (state.BatchID != "" && batchID != state.BatchID) {
		return OpenAIBatchReconcileSummary{}, fmt.Errorf(
			"provider batch ID does not match state",
		)
	}
	if system.Descriptor() != state.System {
		return OpenAIBatchReconcileSummary{}, fmt.Errorf(
			"system does not match Batch state",
		)
	}
	canonical, err := NewCorpus(corpus.Records)
	if err != nil {
		return OpenAIBatchReconcileSummary{}, err
	}
	if canonical.SHA256 != state.CorpusSHA256 {
		return OpenAIBatchReconcileSummary{}, fmt.Errorf(
			"corpus does not match Batch state",
		)
	}
	if err := thresholds.Validate(); err != nil {
		return OpenAIBatchReconcileSummary{}, err
	}

	lines := make(map[string]openAIBatchOutputLine, state.RequestCount)
	for _, artifact := range []struct {
		name string
		raw  []byte
	}{
		{name: "output", raw: outputJSONL},
		{name: "error", raw: errorJSONL},
	} {
		parsed, err := readOpenAIBatchOutputLines(artifact.raw, artifact.name)
		if err != nil {
			return OpenAIBatchReconcileSummary{}, err
		}
		for _, line := range parsed {
			if _, duplicate := lines[line.CustomID]; duplicate {
				return OpenAIBatchReconcileSummary{}, fmt.Errorf(
					"Batch output duplicates custom_id %q",
					line.CustomID,
				)
			}
			lines[line.CustomID] = line
		}
	}
	if requireFullCoverage && len(lines) != state.RequestCount {
		return OpenAIBatchReconcileSummary{}, fmt.Errorf(
			"Batch output covers %d custom IDs, want %d",
			len(lines),
			state.RequestCount,
		)
	}

	recordsByID := make(map[string]CorpusRecord, len(canonical.Records))
	for _, record := range canonical.Records {
		recordsByID[record.CaseID] = record
	}
	var (
		summary             OpenAIBatchReconcileSummary
		directReturnedModel string
	)
	for _, binding := range state.Cases {
		line, ok := lines[binding.CustomID]
		if !ok {
			if requireFullCoverage {
				return OpenAIBatchReconcileSummary{}, fmt.Errorf(
					"Batch output is missing custom_id %q",
					binding.CustomID,
				)
			}
			continue
		}
		delete(lines, binding.CustomID)
		summary.Covered++
		record, ok := recordsByID[binding.CaseID]
		if !ok {
			return OpenAIBatchReconcileSummary{}, fmt.Errorf(
				"Batch state case is absent from corpus",
			)
		}
		expectedContract, err := RequestContractSHA256(
			record,
			system,
			thresholds,
		)
		if err != nil {
			return OpenAIBatchReconcileSummary{}, err
		}
		if expectedContract != binding.RequestContractSHA256 {
			return OpenAIBatchReconcileSummary{}, fmt.Errorf(
				"Batch state request contract mismatch",
			)
		}

		completion, callErr := completionFromOpenAIBatchLine(
			system,
			line,
		)
		completion.Usage = withExplicitUnavailableCostSource(completion.Usage)
		executionAudit, routeErr := auditCompletionRoute(
			system,
			ExecutionBinding{ManifestSHA256: state.ManifestSHA256},
			completion,
			&directReturnedModel,
		)
		if routeErr != nil &&
			(callErr == nil || completionHasRouteEvidence(completion)) {
			callErr = routeErr
		}
		executionAudit.OpenAIBatch = &OpenAIBatchExecutionAudit{
			BatchID:              batchID,
			InputFileSHA256:      state.InputFileSHA256,
			Endpoint:             state.Endpoint,
			CustomID:             binding.CustomID,
			PricingMultiplierPPM: OpenAIBatchPricingPPM,
		}
		executionAudit.UncappedOutputTokenAllowance =
			state.UncappedOutputAllowance

		var result ResultRecord
		if callErr != nil {
			result = ErrorResult(
				canonical.SHA256,
				record,
				system,
				thresholds,
				callErr,
			)
			result.Usage = completion.Usage
			summary.Failed++
		} else {
			result = ParseCompletion(
				canonical.SHA256,
				record,
				system,
				completion,
				thresholds,
			)
			if result.Status == ResultStatusOK {
				summary.Completed++
			} else {
				summary.Failed++
			}
		}
		result.EvaluatorBuildSHA256 = state.EvaluatorBuildSHA256
		result.ExecutionAudit = executionAudit
		if state.UncappedOutputAllowance > 0 &&
			result.Usage.OutputTokens > state.UncappedOutputAllowance {
			return OpenAIBatchReconcileSummary{}, fmt.Errorf(
				"Batch result %q output_tokens %d exceeds its bound uncapped output allowance %d",
				result.CaseID,
				result.Usage.OutputTokens,
				state.UncappedOutputAllowance,
			)
		}
		if summary.CostMicroUSD > math.MaxInt64-result.Usage.CostMicroUSD {
			return OpenAIBatchReconcileSummary{}, fmt.Errorf(
				"Batch actual cost overflows",
			)
		}
		summary.CostMicroUSD += result.Usage.CostMicroUSD
		summary.Results = append(summary.Results, result)
	}
	if len(lines) != 0 {
		return OpenAIBatchReconcileSummary{}, fmt.Errorf(
			"Batch output contains unknown custom IDs",
		)
	}
	if err := ValidateResultsWithThresholds(summary.Results, thresholds); err != nil {
		return OpenAIBatchReconcileSummary{}, err
	}
	return summary, nil
}

func readOpenAIBatchOutputLines(
	raw []byte,
	artifact string,
) ([]openAIBatchOutputLine, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if len(raw) > maxOpenAIBatchResultBytes {
		return nil, fmt.Errorf("Batch %s file exceeds local limit", artifact)
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), maxJSONLLineBytes)
	var lines []openAIBatchOutputLine
	for scanner.Scan() {
		lineNo := len(lines) + 1
		lineRaw := append([]byte(nil), scanner.Bytes()...)
		if len(bytes.TrimSpace(lineRaw)) == 0 {
			return nil, fmt.Errorf(
				"Batch %s line %d is blank",
				artifact,
				lineNo,
			)
		}
		if err := rejectDuplicateJSONKeys(lineRaw); err != nil {
			return nil, fmt.Errorf(
				"Batch %s line %d: %w",
				artifact,
				lineNo,
				err,
			)
		}
		var line openAIBatchOutputLine
		if err := json.Unmarshal(lineRaw, &line); err != nil {
			return nil, fmt.Errorf(
				"Batch %s line %d: decode: %w",
				artifact,
				lineNo,
				err,
			)
		}
		if err := validateIdentifier("Batch custom_id", line.CustomID); err != nil {
			return nil, err
		}
		if line.Response == nil && isNullJSON(line.Error) {
			return nil, fmt.Errorf(
				"Batch %s line %d has neither response nor error",
				artifact,
				lineNo,
			)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan Batch %s: %w", artifact, err)
	}
	return lines, nil
}

func isNullJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func completionFromOpenAIBatchLine(
	system SystemConfig,
	line openAIBatchOutputLine,
) (Completion, error) {
	if line.Response == nil || line.Response.StatusCode != 200 ||
		!isNullJSON(line.Error) {
		return Completion{}, &CallError{Code: "batch_request_failed"}
	}
	if len(line.Response.Body) == 0 {
		return Completion{}, &CallError{Code: "decode_envelope"}
	}
	if err := rejectDuplicateJSONKeys(line.Response.Body); err != nil {
		return Completion{}, &CallError{Code: "decode_envelope"}
	}
	switch system.APIKind {
	case APIKindChat:
		return decodeOpenAIBatchChat(
			system,
			line.Response.RequestID,
			line.Response.Body,
		)
	case APIKindResponses:
		return decodeOpenAIBatchResponses(
			system,
			line.Response.RequestID,
			line.Response.Body,
		)
	default:
		return Completion{}, &CallError{Code: "unsupported_api_kind"}
	}
}

func decodeOpenAIBatchChat(
	system SystemConfig,
	requestID string,
	raw []byte,
) (Completion, error) {
	var response struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Usage chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return Completion{}, &CallError{Code: "decode_envelope"}
	}
	requestID = validAuxiliaryIdentifier(requestID)
	if responseID := validAuxiliaryIdentifier(response.ID); responseID != "" {
		requestID = responseID
	}
	decodedUsage, accountingComplete := decodeCompleteChatUsage(
		jsonObjectField(raw, "usage"),
	)
	usage := decodedUsage.normalize(system, requestID)
	usage.CostMicroUSD = openAIBatchPricedSystem(system).
		EstimateCostWithCacheWriteMicroUSD(
			usage.InputTokens,
			usage.CachedInputTokens,
			usage.CacheWriteTokens,
			usage.OutputTokens,
		)
	usage.CostSource = CostSourceManifestEstimate
	usage.AccountingComplete = accountingComplete
	usage = sanitizeNormalizedUsage(usage, requestID)
	completion := Completion{
		Usage:         usage,
		ReturnedModel: strings.TrimSpace(response.Model),
		RouteProof:    RouteProofDirectResponseModel,
	}
	if completion.ReturnedModel == "" {
		completion.RouteProof = RouteProofUnverified
	}
	if len(response.Choices) == 0 {
		return completion, &CallError{Code: "empty_choices"}
	}
	message := response.Choices[0].Message
	if message.Refusal != "" {
		return completion, &CallError{Code: "refusal"}
	}
	if strings.TrimSpace(message.Content) == "" {
		return completion, &CallError{Code: "empty_content"}
	}
	completion.Text = []byte(message.Content)
	return completion, nil
}

func decodeOpenAIBatchResponses(
	system SystemConfig,
	requestID string,
	raw []byte,
) (Completion, error) {
	var response struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			InputDetails struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return Completion{}, &CallError{Code: "decode_envelope"}
	}
	requestID = validAuxiliaryIdentifier(requestID)
	if responseID := validAuxiliaryIdentifier(response.ID); responseID != "" {
		requestID = responseID
	}
	usage := Usage{
		RequestID:         requestID,
		InputTokens:       response.Usage.InputTokens,
		CachedInputTokens: response.Usage.InputDetails.CachedTokens,
		CacheWriteTokens:  response.Usage.InputDetails.CacheWriteTokens,
		OutputTokens:      response.Usage.OutputTokens,
		ReasoningTokens:   response.Usage.OutputDetails.ReasoningTokens,
		CostSource:        CostSourceManifestEstimate,
		AccountingComplete: usageObjectHasPositiveFields(
			jsonObjectField(raw, "usage"),
			"input_tokens",
			"output_tokens",
		),
	}
	usage.CostMicroUSD = openAIBatchPricedSystem(system).
		EstimateCostWithCacheWriteMicroUSD(
			usage.InputTokens,
			usage.CachedInputTokens,
			usage.CacheWriteTokens,
			usage.OutputTokens,
		)
	usage = sanitizeNormalizedUsage(usage, requestID)
	completion := Completion{
		Usage:         usage,
		ReturnedModel: strings.TrimSpace(response.Model),
		RouteProof:    RouteProofDirectResponseModel,
	}
	if completion.ReturnedModel == "" {
		completion.RouteProof = RouteProofUnverified
	}
	text := strings.TrimSpace(response.OutputText)
	for _, output := range response.Output {
		for _, content := range output.Content {
			if content.Type == "refusal" || content.Refusal != "" {
				return completion, &CallError{Code: "refusal"}
			}
			if text == "" &&
				(content.Type == "output_text" || content.Type == "text") {
				text = strings.TrimSpace(content.Text)
			}
		}
	}
	if text == "" {
		return completion, &CallError{Code: "empty_content"}
	}
	completion.Text = []byte(text)
	return completion, nil
}

func CanonicalResultsSHA256(results []ResultRecord) (string, []byte, error) {
	var buffer bytes.Buffer
	if err := WriteResults(&buffer, results); err != nil {
		return "", nil, err
	}
	raw := buffer.Bytes()
	return sha256Hex(raw), append([]byte(nil), raw...), nil
}

func SortedOpenAIBatchCases(
	cases []OpenAIBatchCaseBinding,
) []OpenAIBatchCaseBinding {
	out := append([]OpenAIBatchCaseBinding(nil), cases...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CaseID < out[j].CaseID
	})
	return out
}
