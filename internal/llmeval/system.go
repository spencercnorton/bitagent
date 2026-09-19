package llmeval

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const maxSystemManifestBytes = 4 << 20

const legacyOpenModelFeasibilityManifestSHA256 = "56576864cb7a507fe85d11442b93eb9628339b8cf8548277169ba7b9caec05b2"
const legacyOpenModelFeasibilityCanonicalSHA256 = "f0209107ee37616873e587e90028073bb1e0feb572a66804a629a326f46e38de"
const legacyNanoAzureContentShadowSystemID = "or-nano-azure-content-shadow-20260728"
const legacyNovaBedrockContentShadowSystemID = "or-nova-micro-amazon-bedrock-content-shadow-feasibility-v2-20260728"

type APIKind string

const (
	APIKindChat      APIKind = "chat"
	APIKindResponses APIKind = "responses"
	APIKindEmbedding APIKind = "embedding"
	APIKindRerank    APIKind = "rerank"
)

type EvaluationLane string

const (
	EvaluationLaneProductionFidelity     EvaluationLane = "production_fidelity"
	EvaluationLaneShadowFidelity         EvaluationLane = "shadow_fidelity"
	EvaluationLaneNormalizedStrict       EvaluationLane = "normalized_strict"
	EvaluationLaneSpecialist             EvaluationLane = "specialist"
	EvaluationLaneRedactedReference      EvaluationLane = "redacted_reference"
	EvaluationLanePromptOnlyExperimental EvaluationLane = "prompt_only_experimental"
)

type OutputContract string

const (
	OutputContractPromptOnly OutputContract = "prompt_only"
	OutputContractJSONObject OutputContract = "json_object"
	OutputContractJSONSchema OutputContract = "json_schema"
	OutputContractNative     OutputContract = "native"
)

type NoThinkLocation string

const (
	NoThinkLocationNone   NoThinkLocation = "none"
	NoThinkLocationSystem NoThinkLocation = "system"
	NoThinkLocationUser   NoThinkLocation = "user"
)

// NoThinkFormat makes the exact delimiter before /no_think part of the
// immutable system contract. The deployed matcher uses a blank line while the
// deployed junk judge uses one newline; treating both as an implementation
// default would make a "production_fidelity" request silently drift.
type NoThinkFormat string

const (
	NoThinkFormatNone          NoThinkFormat = ""
	NoThinkFormatSingleNewline NoThinkFormat = "single_newline"
	NoThinkFormatDoubleNewline NoThinkFormat = "double_newline"
)

// SystemManifest contains only non-secret route configuration. APIKeyEnv names
// an environment variable supplied at run time; the key itself must never
// appear in this file or in a result record.
type SystemManifest struct {
	SchemaVersion int            `json:"schema_version"`
	PricingNotes  []PricingNote  `json:"pricing_notes,omitempty"`
	Systems       []SystemConfig `json:"systems"`
}

type PricingNote struct {
	SystemID string `json:"system_id"`
	Note     string `json:"note"`
}

// SystemConfig makes model, provider endpoint, request contract, privacy, and
// pricing part of the evaluated system. A model name alone is not a
// reproducible target on a multi-provider router.
type SystemConfig struct {
	SystemID                    string          `json:"system_id"`
	Provider                    string          `json:"provider"`
	Model                       string          `json:"model"`
	Variant                     string          `json:"variant,omitempty"`
	PromptVersion               string          `json:"prompt_version"`
	EvaluationLane              EvaluationLane  `json:"evaluation_lane"`
	APIKind                     APIKind         `json:"api_kind"`
	OutputContract              OutputContract  `json:"output_contract"`
	BaseURL                     string          `json:"base_url"`
	APIKeyEnv                   string          `json:"api_key_env"`
	ProviderEndpoint            string          `json:"provider_endpoint,omitempty"`
	Tasks                       []Task          `json:"tasks"`
	InputUSDPerMillion          float64         `json:"input_usd_per_million"`
	CachedInputUSDPerMillion    float64         `json:"cached_input_usd_per_million,omitempty"`
	CacheWriteUSDPerMillion     float64         `json:"cache_write_usd_per_million,omitempty"`
	OutputUSDPerMillion         float64         `json:"output_usd_per_million"`
	USDPerRequest               float64         `json:"usd_per_request,omitempty"`
	BillingMultiplier           float64         `json:"billing_multiplier,omitempty"`
	StructuredOutputs           bool            `json:"structured_outputs"`
	ZDR                         bool            `json:"zdr"`
	ReasoningEffort             string          `json:"reasoning_effort,omitempty"`
	NoThinkLocation             NoThinkLocation `json:"no_think_location"`
	NoThinkFormat               NoThinkFormat   `json:"no_think_format,omitempty"`
	AppendNoThink               bool            `json:"append_no_think,omitempty"`
	UseMaxTokens                bool            `json:"use_max_tokens,omitempty"`
	OmitMaxCompletionTokens     bool            `json:"omit_max_completion_tokens,omitempty"`
	MaxCompletionTokensOverride int             `json:"max_completion_tokens_override,omitempty"`
	Temperature                 *float64        `json:"temperature,omitempty"`
	Seed                        *int64          `json:"seed,omitempty"`
	// RequestTimeoutMS is part of the evaluated behavior. A five-minute
	// evaluator default cannot stand in for an eight-second production fuse.
	RequestTimeoutMS int64 `json:"request_timeout_ms,omitempty"`
	// RequireSourceTitle applies the same independent parser/catalogue identity
	// gate as the bounded live matcher. It is part of the request contract even
	// though it does not alter model-visible bytes.
	RequireSourceTitle bool `json:"require_source_title,omitempty"`
}

func LoadSystemManifest(path string) (SystemManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return SystemManifest{}, err
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, maxSystemManifestBytes+1))
	if err != nil {
		return SystemManifest{}, fmt.Errorf("read system manifest: %w", err)
	}
	if len(raw) > maxSystemManifestBytes {
		return SystemManifest{}, fmt.Errorf(
			"system manifest exceeds %d bytes",
			maxSystemManifestBytes,
		)
	}
	manifest, err := decodeSystemManifest(raw)
	if err != nil {
		return SystemManifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return SystemManifest{}, err
	}
	return manifest, nil
}

// DecodeOpenModelBakeoffRegistrySystemManifest is a read-only compatibility
// path for validating the source-safe historical bakeoff registry. It does
// not authorize execution: normal manifest readers continue to call
// SystemManifest.Validate and reject provider-only shadow-fidelity routes.
func DecodeOpenModelBakeoffRegistrySystemManifest(
	raw []byte,
) (SystemManifest, string, error) {
	if len(raw) > maxSystemManifestBytes {
		return SystemManifest{}, "", fmt.Errorf(
			"system manifest exceeds %d bytes",
			maxSystemManifestBytes,
		)
	}
	manifest, err := decodeSystemManifest(raw)
	if err != nil {
		return SystemManifest{}, "", err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if err := validateOpenModelBakeoffRegistrySystemManifest(
		manifest,
		digest,
	); err != nil {
		return SystemManifest{}, "", err
	}
	return manifest, digest, nil
}

func decodeSystemManifest(raw []byte) (SystemManifest, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return SystemManifest{}, fmt.Errorf("decode system manifest: %w", err)
	}

	var manifest SystemManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return SystemManifest{}, fmt.Errorf("decode system manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return SystemManifest{}, fmt.Errorf("decode system manifest: multiple JSON values")
		}
		return SystemManifest{}, fmt.Errorf("decode system manifest: trailing data: %w", err)
	}
	return manifest, nil
}

func (m SystemManifest) Validate() error {
	return m.validateWith(SystemConfig.Validate)
}

// validateOpenModelBakeoffRegistrySystemManifest permits exactly one
// immutable historical manifest to retain the provider-only shadow-fidelity
// identities under which its private evidence was originally recorded. Every
// other manifest, including a byte-changed copy, receives normal strict
// validation.
func validateOpenModelBakeoffRegistrySystemManifest(
	m SystemManifest,
	manifestSHA256 string,
) error {
	if manifestSHA256 != legacyOpenModelFeasibilityManifestSHA256 {
		return m.Validate()
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode legacy bakeoff manifest: %w", err)
	}
	canonicalDigest := fmt.Sprintf("%x", sha256.Sum256(canonical))
	if canonicalDigest != legacyOpenModelFeasibilityCanonicalSHA256 {
		return fmt.Errorf(
			"legacy bakeoff manifest parsed content changed from its immutable historical contract (canonical sha256 %s)",
			canonicalDigest,
		)
	}

	seen := make(map[string]int, 2)
	for _, system := range m.Systems {
		if !legacyProviderOnlyShadowSystem(system.SystemID) {
			continue
		}
		seen[system.SystemID]++
	}
	for _, systemID := range []string{
		legacyNanoAzureContentShadowSystemID,
		legacyNovaBedrockContentShadowSystemID,
	} {
		if seen[systemID] != 1 {
			return fmt.Errorf(
				"legacy bakeoff manifest must contain exactly one immutable system %q",
				systemID,
			)
		}
	}

	return m.validateWith(func(system SystemConfig) error {
		if legacyProviderOnlyShadowSystem(system.SystemID) {
			return nil
		}
		return system.Validate()
	})
}

func (m SystemManifest) validateWith(
	validateSystem func(SystemConfig) error,
) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("system manifest schema_version must be %d", SchemaVersion)
	}
	if len(m.Systems) == 0 {
		return fmt.Errorf("system manifest contains no systems")
	}
	seen := make(map[string]struct{}, len(m.Systems))
	for i := range m.Systems {
		if err := validateSystem(m.Systems[i]); err != nil {
			return fmt.Errorf("systems[%d]: %w", i, err)
		}
		if _, ok := seen[m.Systems[i].SystemID]; ok {
			return fmt.Errorf("systems[%d]: duplicate system_id %q", i, m.Systems[i].SystemID)
		}
		seen[m.Systems[i].SystemID] = struct{}{}
	}
	return nil
}

func legacyProviderOnlyShadowSystem(systemID string) bool {
	switch systemID {
	case legacyNanoAzureContentShadowSystemID,
		legacyNovaBedrockContentShadowSystemID:
		return true
	default:
		return false
	}
}

var envNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func (s SystemConfig) Validate() error {
	if strings.TrimSpace(s.SystemID) == "" ||
		strings.TrimSpace(s.Model) == "" ||
		strings.TrimSpace(s.PromptVersion) == "" {
		return fmt.Errorf("system_id, model, and prompt_version are required")
	}
	switch s.Provider {
	case "openai", "openrouter":
	default:
		return fmt.Errorf("provider %q is unsupported; hosted openai or openrouter is required", s.Provider)
	}
	switch s.APIKind {
	case APIKindChat, APIKindResponses, APIKindEmbedding, APIKindRerank:
	default:
		return fmt.Errorf("api_kind %q is unsupported", s.APIKind)
	}
	switch s.EvaluationLane {
	case EvaluationLaneProductionFidelity,
		EvaluationLaneShadowFidelity,
		EvaluationLaneNormalizedStrict,
		EvaluationLaneSpecialist,
		EvaluationLaneRedactedReference,
		EvaluationLanePromptOnlyExperimental:
	default:
		return fmt.Errorf("evaluation_lane %q is unsupported", s.EvaluationLane)
	}
	switch s.OutputContract {
	case OutputContractPromptOnly,
		OutputContractJSONObject,
		OutputContractJSONSchema,
		OutputContractNative:
	default:
		return fmt.Errorf("output_contract %q is unsupported", s.OutputContract)
	}
	switch s.NoThinkLocation {
	case NoThinkLocationNone, NoThinkLocationSystem, NoThinkLocationUser:
	default:
		return fmt.Errorf("no_think_location %q is unsupported", s.NoThinkLocation)
	}
	switch s.NoThinkFormat {
	case NoThinkFormatNone,
		NoThinkFormatSingleNewline,
		NoThinkFormatDoubleNewline:
	default:
		return fmt.Errorf("no_think_format %q is unsupported", s.NoThinkFormat)
	}
	if s.NoThinkLocation == NoThinkLocationNone {
		if s.NoThinkFormat != NoThinkFormatNone {
			return fmt.Errorf("no_think_format requires a system or user no_think_location")
		}
	} else if s.NoThinkFormat == NoThinkFormatNone {
		return fmt.Errorf("system or user no_think_location requires an explicit no_think_format")
	}
	if s.Provider == "openrouter" && strings.TrimSpace(s.ProviderEndpoint) == "" {
		return fmt.Errorf("openrouter systems must pin provider_endpoint")
	}
	if s.Provider == "openrouter" && !s.ZDR &&
		s.EvaluationLane != EvaluationLaneRedactedReference {
		return fmt.Errorf("openrouter systems must enforce zdr outside the redacted-reference lane")
	}
	if (s.APIKind == APIKindChat || s.APIKind == APIKindResponses) &&
		s.Provider == "openrouter" && !s.StructuredOutputs &&
		s.EvaluationLane != EvaluationLanePromptOnlyExperimental &&
		s.EvaluationLane != EvaluationLaneShadowFidelity {
		return fmt.Errorf("openrouter generative systems must require structured_outputs")
	}
	if s.APIKind == APIKindResponses && s.Provider != "openai" {
		return fmt.Errorf("responses api is currently supported only for direct openai baselines")
	}
	if s.StructuredOutputs != (s.OutputContract == OutputContractJSONSchema) {
		return fmt.Errorf("structured_outputs must be true exactly when output_contract is json_schema")
	}
	switch s.APIKind {
	case APIKindChat, APIKindResponses:
		if s.OutputContract == OutputContractNative {
			return fmt.Errorf("%s api cannot use native output_contract", s.APIKind)
		}
	case APIKindEmbedding, APIKindRerank:
		if s.OutputContract != OutputContractNative {
			return fmt.Errorf("%s api requires native output_contract", s.APIKind)
		}
	}
	if s.EvaluationLane == EvaluationLaneProductionFidelity {
		if s.Provider != "openai" {
			return fmt.Errorf("production_fidelity lane is reserved for direct openai controls")
		}
		if s.OutputContract == OutputContractJSONSchema {
			return fmt.Errorf("production_fidelity lane cannot silently normalize to json_schema")
		}
	}
	if s.EvaluationLane == EvaluationLaneShadowFidelity {
		if s.Provider != "openrouter" || !s.ZDR || s.APIKind != APIKindChat {
			return fmt.Errorf(
				"shadow_fidelity lane requires an openrouter zdr chat route",
			)
		}
		provider, quantization := splitOpenRouterProviderEndpoint(
			s.ProviderEndpoint,
		)
		if provider == "" || quantization == "" || quantization == "unknown" {
			return fmt.Errorf(
				"shadow_fidelity lane requires a pinned provider_endpoint with an explicit recognized quantization",
			)
		}
	}
	if s.EvaluationLane == EvaluationLaneNormalizedStrict &&
		s.OutputContract != OutputContractJSONSchema {
		return fmt.Errorf("normalized_strict lane requires json_schema output_contract")
	}
	if s.EvaluationLane == EvaluationLaneRedactedReference && s.ZDR {
		return fmt.Errorf("redacted_reference lane is only for routes that are not zdr-verified")
	}
	if s.EvaluationLane == EvaluationLanePromptOnlyExperimental {
		if s.Provider != "openrouter" || !s.ZDR || s.APIKind != APIKindChat ||
			s.OutputContract != OutputContractPromptOnly {
			return fmt.Errorf(
				"prompt_only_experimental lane requires an openrouter zdr chat route with prompt_only output_contract",
			)
		}
	}
	if s.RequestTimeoutMS < 0 || s.RequestTimeoutMS > 10*60*1000 {
		return fmt.Errorf("request_timeout_ms must be in 0..600000")
	}
	if s.AppendNoThink != (s.NoThinkLocation == NoThinkLocationUser) {
		return fmt.Errorf("append_no_think must mirror no_think_location=user")
	}
	if s.UseMaxTokens && s.APIKind != APIKindChat {
		return fmt.Errorf("use_max_tokens is valid only for chat api systems")
	}
	if s.UseMaxTokens && s.OmitMaxCompletionTokens {
		return fmt.Errorf("use_max_tokens and omit_max_completion_tokens are mutually exclusive")
	}
	if s.OmitMaxCompletionTokens && s.MaxCompletionTokensOverride != 0 {
		return fmt.Errorf("omit_max_completion_tokens and max_completion_tokens_override are mutually exclusive")
	}
	if s.OmitMaxCompletionTokens &&
		s.APIKind != APIKindChat && s.APIKind != APIKindResponses {
		return fmt.Errorf(
			"omit_max_completion_tokens is valid only for generative APIs",
		)
	}
	if s.MaxCompletionTokensOverride < 0 {
		return fmt.Errorf("max_completion_tokens_override cannot be negative")
	}
	if s.Temperature != nil {
		if math.IsNaN(*s.Temperature) || math.IsInf(*s.Temperature, 0) ||
			*s.Temperature < 0 || *s.Temperature > 2 {
			return fmt.Errorf("temperature must be in [0,2]")
		}
		model := strings.ToLower(s.Model)
		if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
			model = model[slash+1:]
		}
		if strings.HasPrefix(model, "gpt-5") {
			return fmt.Errorf("temperature must be omitted for gpt-5 models")
		}
	}
	if s.APIKind == APIKindResponses && (s.Temperature != nil || s.Seed != nil) {
		return fmt.Errorf("responses api systems must omit temperature and seed")
	}
	if !envNamePattern.MatchString(s.APIKeyEnv) {
		return fmt.Errorf("api_key_env %q is not a valid environment variable name", s.APIKeyEnv)
	}
	if len(s.Tasks) == 0 {
		return fmt.Errorf("at least one task is required")
	}
	seenTasks := make(map[Task]struct{}, len(s.Tasks))
	for _, task := range s.Tasks {
		if !validTask(task) {
			return fmt.Errorf("unsupported task %q", task)
		}
		if _, ok := seenTasks[task]; ok {
			return fmt.Errorf("duplicate task %q", task)
		}
		seenTasks[task] = struct{}{}
	}
	if s.EvaluationLane == EvaluationLaneShadowFidelity {
		for _, task := range s.Tasks {
			if err := validateDeployedWireRequestControls(s, task); err != nil {
				return err
			}
		}
	}
	if s.APIKind == APIKindRerank &&
		(len(s.Tasks) != 1 || s.Tasks[0] != TaskMatcherRerank) {
		return fmt.Errorf("rerank api requires exactly the matcher_rerank task")
	}
	if s.APIKind == APIKindEmbedding {
		if len(s.Tasks) != 1 || s.Tasks[0] != TaskMatcherRerank {
			return fmt.Errorf(
				"embedding api requires exactly the matcher_rerank task",
			)
		}
		if s.PromptVersion != MatcherSpecialistAlgorithmID {
			return fmt.Errorf(
				"embedding api requires prompt_version %q",
				MatcherSpecialistAlgorithmID,
			)
		}
		if s.Provider != "openrouter" {
			return fmt.Errorf("embedding api requires the hosted OpenRouter route")
		}
	}
	for _, price := range []struct {
		name  string
		value float64
	}{
		{"input_usd_per_million", s.InputUSDPerMillion},
		{"cached_input_usd_per_million", s.CachedInputUSDPerMillion},
		{"cache_write_usd_per_million", s.CacheWriteUSDPerMillion},
		{"output_usd_per_million", s.OutputUSDPerMillion},
		{"usd_per_request", s.USDPerRequest},
		{"billing_multiplier", s.BillingMultiplier},
	} {
		if math.IsNaN(price.value) || math.IsInf(price.value, 0) || price.value < 0 {
			return fmt.Errorf("%s must be finite and non-negative", price.name)
		}
	}
	switch s.ReasoningEffort {
	case "", "none", "minimal", "low":
	default:
		return fmt.Errorf("reasoning_effort %q is outside the low-cost evaluation policy", s.ReasoningEffort)
	}
	if err := validateHostedURL(s.Provider, s.BaseURL); err != nil {
		return err
	}
	return nil
}

func (s SystemConfig) SupportsTask(task Task) bool {
	for _, candidate := range s.Tasks {
		if candidate == task {
			return true
		}
	}
	return false
}

// UsesDeployedWireContract identifies lanes that must reuse the exact runtime
// request builders, response decoders, and action gates. Shadow fidelity is
// intentionally separate from production_fidelity: it adds only evaluator
// routing controls for a pinned OpenRouter endpoint and remains ineligible for
// promotion anywhere that requires the production_fidelity lane explicitly.
func (s SystemConfig) UsesDeployedWireContract() bool {
	return s.EvaluationLane == EvaluationLaneProductionFidelity ||
		s.EvaluationLane == EvaluationLaneShadowFidelity
}

func (s SystemConfig) Descriptor() SystemDescriptor {
	return SystemDescriptor{
		SystemID:      s.SystemID,
		Provider:      s.Provider,
		Model:         s.Model,
		Variant:       s.Variant,
		PromptVersion: s.PromptVersion,
	}
}

func (s SystemConfig) APIKey() (string, error) {
	value := strings.TrimSpace(os.Getenv(s.APIKeyEnv))
	if value == "" {
		return "", fmt.Errorf("required API key environment variable %s is empty", s.APIKeyEnv)
	}
	return value, nil
}

func (s SystemConfig) EstimateCostMicroUSD(input, cachedInput, output int64) int64 {
	return s.EstimateCostWithCacheWriteMicroUSD(input, cachedInput, 0, output)
}

// EstimateCostWithCacheWriteMicroUSD prices cache writes separately for
// providers such as direct GPT-5.6, where a cache write is billed above the
// ordinary input rate. input includes all regular, cached-read, and
// cache-write input tokens; the subsets are clamped to that total.
func (s SystemConfig) EstimateCostWithCacheWriteMicroUSD(
	input, cachedInput, cacheWrite, output int64,
) int64 {
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	if cachedInput < 0 {
		cachedInput = 0
	}
	if cachedInput > input {
		cachedInput = input
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	if cacheWrite > input-cachedInput {
		cacheWrite = input - cachedInput
	}
	regularInput := input - cachedInput - cacheWrite
	cachedPrice := s.CachedInputUSDPerMillion
	if cachedPrice == 0 {
		cachedPrice = s.InputUSDPerMillion
	}
	cacheWritePrice := s.CacheWriteUSDPerMillion
	if cacheWritePrice == 0 {
		cacheWritePrice = s.InputUSDPerMillion
	}
	costUSD := float64(regularInput)*s.InputUSDPerMillion/1_000_000 +
		float64(cachedInput)*cachedPrice/1_000_000 +
		float64(cacheWrite)*cacheWritePrice/1_000_000 +
		float64(output)*s.OutputUSDPerMillion/1_000_000 +
		s.USDPerRequest
	multiplier := s.billingMultiplier()
	microUSD := costUSD * multiplier * 1_000_000
	if math.IsNaN(microUSD) || math.IsInf(microUSD, 0) ||
		microUSD >= float64(math.MaxInt64) {
		// The fuse must fail closed. Converting an out-of-range float directly
		// to int64 can wrap negative and make an extreme price look cheaper.
		return math.MaxInt64
	}
	if microUSD <= 0 {
		return 0
	}
	return roundPositiveMicroUSD(microUSD)
}

// roundPositiveMicroUSD preserves the smallest representable positive charge.
// Usage artifacts account in whole micro-dollars; rounding a real sub-micro
// charge to zero would let a paid route masquerade as free during promotion
// and would make repeated tiny requests invisible to the local cost fuse.
func roundPositiveMicroUSD(microUSD float64) int64 {
	if math.IsNaN(microUSD) || microUSD <= 0 {
		return 0
	}
	if math.IsInf(microUSD, 1) || microUSD >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	rounded := int64(microUSD + 0.5)
	if rounded == 0 {
		return 1
	}
	return rounded
}

func (s SystemConfig) billingMultiplier() float64 {
	if s.BillingMultiplier > 0 {
		return s.BillingMultiplier
	}
	if s.Provider == "openrouter" {
		return 1.055
	}
	return 1
}

func (s SystemConfig) isZeroPriced() bool {
	return s.InputUSDPerMillion == 0 &&
		s.CachedInputUSDPerMillion == 0 &&
		s.CacheWriteUSDPerMillion == 0 &&
		s.OutputUSDPerMillion == 0 &&
		s.USDPerRequest == 0
}

func (s SystemConfig) noThinkSuffix() string {
	switch s.NoThinkFormat {
	case NoThinkFormatSingleNewline:
		return "\n/no_think"
	case NoThinkFormatDoubleNewline:
		return "\n\n/no_think"
	default:
		return ""
	}
}

func validateHostedURL(provider, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Hostname() == "" {
		return fmt.Errorf("base_url must be an absolute https URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if ip := net.ParseIP(host); ip != nil || host == "localhost" || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("base_url must be a hosted provider endpoint, not a local or literal-IP endpoint")
	}
	switch provider {
	case "openai":
		if host != "api.openai.com" {
			return fmt.Errorf("openai base_url host must be api.openai.com")
		}
	case "openrouter":
		if host != "openrouter.ai" {
			return fmt.Errorf("openrouter base_url host must be openrouter.ai")
		}
	}
	return nil
}

func validTask(task Task) bool {
	for _, candidate := range orderedTasks {
		if task == candidate {
			return true
		}
	}
	return false
}
