package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	OpenModelBakeoffRegistrySchemaVersion = 1
	OpenModelBakeoffRegistryArtifactKind  = "open_model_bakeoff_registry"
	maxOpenModelBakeoffRegistryBytes      = 8 << 20
)

var (
	lowercaseSHA256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	registryKeyAcronymBoundary = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	registryKeyWordBoundary    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	registryCredentialPattern  = regexp.MustCompile(
		`(?i)(?:glpat-[a-z0-9_-]{8,}|ghp_[a-z0-9]{8,}|github_pat_[a-z0-9_]{8,}|sk-[a-z0-9_-]{10,}|xox[baprs]-[a-z0-9-]{8,})`,
	)
	registryAWSAccessKeyPattern = regexp.MustCompile(
		`(?i)(?:^|[^a-z0-9])(?:akia|asia)[a-z0-9]{16}(?:$|[^a-z0-9])`,
	)
	registryPEMPrivateKeyPattern = regexp.MustCompile(
		`(?i)-----begin [a-z0-9 ]*private key-----`,
	)
	registryCredentialBearingKeyQualifiers = []string{
		"api", "access", "secret", "private", "signing", "encryption", "decryption",
		"symmetric", "auth", "authentication", "hmac", "ssh", "tls", "master",
		"root", "shared", "consumer", "client", "application", "app", "service",
		"serviceaccount", "webhook", "oauth", "jwt", "sas",
	}
)

// OpenModelBakeoffRegistry is the source-safe, checked-in index for a dated
// diagnostic model screen. Raw prompts, replies, row identifiers, and source
// material stay outside the repository; exact hashes keep the index bound to
// those private artifacts.
type OpenModelBakeoffRegistry struct {
	SchemaVersion              int                                     `json:"schema_version"`
	ArtifactKind               string                                  `json:"artifact_kind"`
	ProducedUTC                string                                  `json:"produced_utc"`
	Status                     string                                  `json:"status"`
	Purpose                    string                                  `json:"purpose"`
	SourceSafety               BakeoffRegistrySourceSafety             `json:"source_safety"`
	Lineage                    BakeoffRegistryLineage                  `json:"lineage"`
	Bindings                   BakeoffRegistryBindings                 `json:"bindings"`
	ComparisonSelections       map[string]BakeoffComparisonSelection   `json:"comparison_selections"`
	EvidencePolicy             BakeoffRegistryEvidencePolicy           `json:"evidence_policy"`
	TaskCaveats                map[string][]string                     `json:"task_caveats"`
	PreflightIncompatibilities []BakeoffPreflightIncompatibility       `json:"preflight_incompatibilities"`
	NotRun                     []BakeoffNotRun                         `json:"not_run"`
	Cells                      map[Task][]OpenModelBakeoffRegistryCell `json:"cells"`
	CurrentProduction          *BakeoffCurrentProductionInventory      `json:"current_production,omitempty"`
}

type BakeoffRegistrySourceSafety struct {
	SourceMaterialIncluded              bool `json:"source_material_included"`
	RowIdentifiersIncluded              bool `json:"row_identifiers_included"`
	PromptsOrRawRepliesIncluded         bool `json:"prompts_or_raw_replies_included"`
	CredentialsIncluded                 bool `json:"credentials_included"`
	PathsAreRepositoryRelativeBasenames bool `json:"paths_are_repository_relative_basenames_only"`
}

type BakeoffRegistryLineage struct {
	PriorRegistry BakeoffBoundArtifact `json:"prior_registry"`
}

type BakeoffBoundArtifact struct {
	Basename     string `json:"basename"`
	SHA256       string `json:"sha256"`
	Immutable    bool   `json:"immutable,omitempty"`
	Relationship string `json:"relationship,omitempty"`
}

type BakeoffRegistryBindings struct {
	Manifest        BakeoffBoundArtifact                    `json:"manifest"`
	EvaluatorBuilds map[string]BakeoffEvaluatorBuildBinding `json:"evaluator_builds"`
	Corpora         map[string]BakeoffCorpusBinding         `json:"corpora"`
}

type BakeoffEvaluatorBuildBinding struct {
	ArtifactSchemaVersion int      `json:"artifact_schema_version"`
	SHA256                string   `json:"sha256"`
	CellIDs               []string `json:"cell_ids"`
}

type BakeoffCorpusBinding struct {
	Task           Task   `json:"task"`
	Records        int    `json:"records"`
	SHA256         string `json:"sha256"`
	LabelSemantics string `json:"label_semantics"`
}

type BakeoffComparisonSelection struct {
	Task     Task   `json:"task"`
	Selected int    `json:"selected"`
	SHA256   string `json:"sha256"`
}

type BakeoffRegistryEvidencePolicy struct {
	AllHistoricalScreens string            `json:"all_historical_screens"`
	ShadowFidelity       string            `json:"shadow_fidelity"`
	NormalizedStrict     string            `json:"normalized_strict"`
	PromotionEligible    bool              `json:"promotion_eligible"`
	Tiers                map[string]string `json:"tiers"`
}

type BakeoffPreflightIncompatibility struct {
	SystemID             string         `json:"system_id"`
	Task                 Task           `json:"task"`
	Lane                 EvaluationLane `json:"lane"`
	Stage                string         `json:"stage"`
	EvidenceTier         string         `json:"evidence_tier"`
	Decision             string         `json:"decision"`
	PromotionEligible    bool           `json:"promotion_eligible"`
	ReasonCodes          []string       `json:"reason_codes"`
	MatchedEndpointCount *int           `json:"matched_endpoint_count,omitempty"`
}

// BakeoffNotRun makes every manifest system/task pair that was intentionally
// omitted from the dated diagnostic matrix explicit. A not-run record is not
// evidence of compatibility, quality, or failure.
type BakeoffNotRun struct {
	SystemID          string         `json:"system_id"`
	Task              Task           `json:"task"`
	Lane              EvaluationLane `json:"lane"`
	Stage             string         `json:"stage"`
	Decision          string         `json:"decision"`
	PromotionEligible bool           `json:"promotion_eligible"`
	ReasonCodes       []string       `json:"reason_codes"`
}

type OpenModelBakeoffRegistryCell struct {
	CellID              string                     `json:"cell_id"`
	SystemID            string                     `json:"system_id"`
	MethodologyVersion  string                     `json:"methodology_version"`
	Lane                EvaluationLane             `json:"lane"`
	CorpusBinding       string                     `json:"corpus_binding"`
	ComparisonSelection string                     `json:"comparison_selection"`
	Artifact            BakeoffCellArtifact        `json:"artifact"`
	Sample              BakeoffCellSample          `json:"sample"`
	SupersededBy        string                     `json:"superseded_by,omitempty"`
	Metrics             map[string]json.RawMessage `json:"metrics"`
	Reliability         BakeoffCellReliability     `json:"reliability"`
	LatencyMS           BakeoffCellLatency         `json:"latency_ms"`
	Cost                BakeoffCellCost            `json:"cost"`
	Assessment          BakeoffCellAssessment      `json:"assessment"`
}

type BakeoffCellArtifact struct {
	Basename            string `json:"basename"`
	SHA256              string `json:"sha256"`
	RouteSnapshotSHA256 string `json:"route_snapshot_sha256,omitempty"`
	RouteBinding        string `json:"route_binding,omitempty"`
}

type BakeoffCellSample struct {
	Selected    int `json:"selected"`
	Completed   int `json:"completed"`
	Concurrency int `json:"concurrency"`
}

type BakeoffCountRate struct {
	Count       int     `json:"count"`
	Denominator int     `json:"denominator"`
	Rate        float64 `json:"rate"`
}

type BakeoffCellReliability struct {
	SchemaErrors  BakeoffCountRate `json:"schema_errors"`
	RuntimeErrors BakeoffCountRate `json:"runtime_errors"`
	Timeouts      BakeoffCountRate `json:"timeouts"`
	ErrorCodes    map[string]int   `json:"error_codes"`
}

type BakeoffLatencyPercentiles struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

type BakeoffRouteAuditCounts struct {
	Attempted int `json:"attempted"`
	Failed    int `json:"failed"`
}

type BakeoffCellLatency struct {
	Deadline       int64                     `json:"deadline"`
	Primary        BakeoffLatencyPercentiles `json:"primary"`
	OperationTotal BakeoffLatencyPercentiles `json:"operation_total"`
	RouteAudit     BakeoffRouteAuditCounts   `json:"route_audit"`
}

type BakeoffCellCost struct {
	CostMicroUSD         int64  `json:"cost_micro_usd"`
	CostSource           string `json:"cost_source"`
	AccountingComplete   int    `json:"accounting_complete"`
	AccountingIncomplete int    `json:"accounting_incomplete"`
}

type BakeoffCellAssessment struct {
	EvidenceTier      string   `json:"evidence_tier"`
	Decision          string   `json:"decision"`
	PromotionEligible bool     `json:"promotion_eligible"`
	ReasonCodes       []string `json:"reason_codes"`
}

// BakeoffCurrentProductionInventory keeps historical diagnostic conclusions
// from being mistaken for live state. Its source artifacts are checked-in,
// source-safe observations rather than mutable prose.
type BakeoffCurrentProductionInventory struct {
	AsOfUTC                           string                                   `json:"as_of_utc"`
	PriorRegistryLiveClaimsSuperseded bool                                     `json:"prior_registry_live_claims_superseded"`
	NoActiveOpenWeightDecisionPath    bool                                     `json:"no_active_open_weight_decision_path"`
	SourceArtifacts                   []BakeoffBoundArtifact                   `json:"source_artifacts"`
	Routes                            map[string]BakeoffCurrentProductionRoute `json:"routes"`
}

type BakeoffCurrentProductionRoute struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	RuntimeState   string `json:"runtime_state"`
	OpenWeight     bool   `json:"open_weight"`
	SourceArtifact string `json:"source_artifact"`
}

type bakeoffManifestPair struct {
	SystemID string
	Task     Task
}

func ReadOpenModelBakeoffRegistry(r io.Reader) (OpenModelBakeoffRegistry, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxOpenModelBakeoffRegistryBytes+1))
	if err != nil {
		return OpenModelBakeoffRegistry{}, fmt.Errorf("read bakeoff registry: %w", err)
	}
	if len(raw) > maxOpenModelBakeoffRegistryBytes {
		return OpenModelBakeoffRegistry{}, fmt.Errorf(
			"bakeoff registry exceeds %d bytes",
			maxOpenModelBakeoffRegistryBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return OpenModelBakeoffRegistry{}, fmt.Errorf("decode bakeoff registry: %w", err)
	}
	if err := validateBakeoffRegistryRawSourceSafety(raw); err != nil {
		return OpenModelBakeoffRegistry{}, fmt.Errorf("decode bakeoff registry: source safety: %w", err)
	}

	var registry OpenModelBakeoffRegistry
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return OpenModelBakeoffRegistry{}, fmt.Errorf("decode bakeoff registry: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return OpenModelBakeoffRegistry{}, fmt.Errorf("decode bakeoff registry: multiple JSON values")
		}
		return OpenModelBakeoffRegistry{}, fmt.Errorf("decode bakeoff registry: trailing data: %w", err)
	}
	return registry, nil
}

// ValidateOpenModelBakeoffRegistry validates both the registry's internal
// consistency and its exact bound model manifest. It intentionally does not
// treat diagnostic screens as promotion evidence.
func ValidateOpenModelBakeoffRegistry(
	registry OpenModelBakeoffRegistry,
	manifest SystemManifest,
	manifestSHA256 string,
) error {
	if registry.SchemaVersion != OpenModelBakeoffRegistrySchemaVersion {
		return fmt.Errorf(
			"schema_version must be %d",
			OpenModelBakeoffRegistrySchemaVersion,
		)
	}
	if registry.ArtifactKind != OpenModelBakeoffRegistryArtifactKind {
		return fmt.Errorf("artifact_kind must be %q", OpenModelBakeoffRegistryArtifactKind)
	}
	producedDate, err := time.Parse("2006-01-02", registry.ProducedUTC)
	if err != nil {
		return fmt.Errorf("produced_utc must be YYYY-MM-DD")
	}
	if strings.TrimSpace(registry.Status) == "" || strings.TrimSpace(registry.Purpose) == "" {
		return fmt.Errorf("status and purpose are required")
	}
	if registry.SourceSafety.SourceMaterialIncluded ||
		registry.SourceSafety.RowIdentifiersIncluded ||
		registry.SourceSafety.PromptsOrRawRepliesIncluded ||
		registry.SourceSafety.CredentialsIncluded ||
		!registry.SourceSafety.PathsAreRepositoryRelativeBasenames {
		return fmt.Errorf("source_safety must exclude source material, row identifiers, prompts, replies, and credentials and require basenames")
	}
	if err := validateBakeoffBoundArtifact("lineage.prior_registry", registry.Lineage.PriorRegistry, true); err != nil {
		return err
	}
	if !registry.Lineage.PriorRegistry.Immutable {
		return fmt.Errorf("lineage.prior_registry must be immutable")
	}
	if strings.TrimSpace(registry.Lineage.PriorRegistry.Relationship) == "" {
		return fmt.Errorf("lineage.prior_registry.relationship is required")
	}
	if err := validateBakeoffBoundArtifact("bindings.manifest", registry.Bindings.Manifest, false); err != nil {
		return err
	}
	if !validLowercaseSHA256(manifestSHA256) || registry.Bindings.Manifest.SHA256 != manifestSHA256 {
		return fmt.Errorf("bindings.manifest.sha256 does not match the loaded manifest")
	}
	if err := validateOpenModelBakeoffRegistrySystemManifest(
		manifest,
		manifestSHA256,
	); err != nil {
		return fmt.Errorf("bound manifest: %w", err)
	}

	systems := make(map[string]SystemConfig, len(manifest.Systems))
	expectedPairs := make(map[bakeoffManifestPair]struct{})
	for _, system := range manifest.Systems {
		systems[system.SystemID] = system
		for _, task := range system.Tasks {
			expectedPairs[bakeoffManifestPair{SystemID: system.SystemID, Task: task}] = struct{}{}
		}
	}
	if len(registry.ComparisonSelections) == 0 {
		return fmt.Errorf("comparison_selections must not be empty")
	}
	for name, selection := range registry.ComparisonSelections {
		if strings.TrimSpace(name) == "" || !validEvaluationTask(selection.Task) ||
			selection.Selected <= 0 || !validLowercaseSHA256(selection.SHA256) {
			return fmt.Errorf("comparison_selections[%q] is invalid", name)
		}
	}
	if len(registry.Bindings.EvaluatorBuilds) == 0 || len(registry.Bindings.Corpora) == 0 {
		return fmt.Errorf("evaluator and corpus bindings must not be empty")
	}
	for name, corpus := range registry.Bindings.Corpora {
		if strings.TrimSpace(name) == "" || !validEvaluationTask(corpus.Task) ||
			corpus.Records <= 0 ||
			!validLowercaseSHA256(corpus.SHA256) || strings.TrimSpace(corpus.LabelSemantics) == "" {
			return fmt.Errorf("bindings.corpora[%q] is invalid", name)
		}
	}
	if registry.EvidencePolicy.PromotionEligible {
		return fmt.Errorf("historical bakeoff registry cannot be promotion eligible")
	}
	if len(registry.EvidencePolicy.Tiers) == 0 ||
		strings.TrimSpace(registry.EvidencePolicy.AllHistoricalScreens) == "" ||
		strings.TrimSpace(registry.EvidencePolicy.ShadowFidelity) == "" ||
		strings.TrimSpace(registry.EvidencePolicy.NormalizedStrict) == "" {
		return fmt.Errorf("evidence_policy is incomplete")
	}

	allCells := make(map[string]struct{})
	pairCoverage := make(map[bakeoffManifestPair]string, len(expectedPairs))
	cellsByID := make(map[string]struct {
		task Task
		cell OpenModelBakeoffRegistryCell
	})
	for task, cells := range registry.Cells {
		if !validEvaluationTask(task) {
			return fmt.Errorf("cells has unsupported task %q", task)
		}
		if len(cells) == 0 {
			return fmt.Errorf("cells[%q] must not be empty", task)
		}
		for i, cell := range cells {
			if err := validateBakeoffCell(task, i, cell, systems, registry); err != nil {
				return err
			}
			if _, exists := allCells[cell.CellID]; exists {
				return fmt.Errorf("duplicate cell_id %q", cell.CellID)
			}
			allCells[cell.CellID] = struct{}{}
			pairCoverage[bakeoffManifestPair{
				SystemID: cell.SystemID,
				Task:     task,
			}] = "cells"
			cellsByID[cell.CellID] = struct {
				task Task
				cell OpenModelBakeoffRegistryCell
			}{task: task, cell: cell}
		}
	}
	if len(allCells) == 0 {
		return fmt.Errorf("cells must not be empty")
	}

	for cellID, entry := range cellsByID {
		if entry.cell.SupersededBy == "" {
			continue
		}
		target, exists := cellsByID[entry.cell.SupersededBy]
		if !exists {
			return fmt.Errorf("cell %q superseded_by references unknown cell %q", cellID, entry.cell.SupersededBy)
		}
		if target.task != entry.task || target.cell.SystemID != entry.cell.SystemID ||
			target.cell.Sample.Selected <= entry.cell.Sample.Selected {
			return fmt.Errorf("cell %q superseded_by must identify a larger sample for the same task and system", cellID)
		}
		seen := map[string]struct{}{cellID: {}}
		for next := entry.cell.SupersededBy; next != ""; next = cellsByID[next].cell.SupersededBy {
			if _, duplicate := seen[next]; duplicate {
				return fmt.Errorf("superseded_by cycle includes cell %q", next)
			}
			seen[next] = struct{}{}
			if _, exists := cellsByID[next]; !exists {
				break
			}
		}
	}

	buildCoverage := make(map[string]string, len(allCells))
	for buildName, build := range registry.Bindings.EvaluatorBuilds {
		if strings.TrimSpace(buildName) == "" || build.ArtifactSchemaVersion <= 0 ||
			!validLowercaseSHA256(build.SHA256) || len(build.CellIDs) == 0 {
			return fmt.Errorf("bindings.evaluator_builds[%q] is invalid", buildName)
		}
		for _, cellID := range build.CellIDs {
			if _, exists := allCells[cellID]; !exists {
				return fmt.Errorf("evaluator build %q references unknown cell %q", buildName, cellID)
			}
			if prior, exists := buildCoverage[cellID]; exists {
				return fmt.Errorf("cell %q is bound to evaluator builds %q and %q", cellID, prior, buildName)
			}
			if cellsByID[cellID].cell.MethodologyVersion != buildName {
				return fmt.Errorf(
					"cell %q methodology_version %q does not match evaluator build %q",
					cellID,
					cellsByID[cellID].cell.MethodologyVersion,
					buildName,
				)
			}
			buildCoverage[cellID] = buildName
		}
	}
	if len(buildCoverage) != len(allCells) {
		missing := make([]string, 0)
		for cellID := range allCells {
			if _, exists := buildCoverage[cellID]; !exists {
				missing = append(missing, cellID)
			}
		}
		sort.Strings(missing)
		return fmt.Errorf("evaluator build coverage is incomplete: %s", strings.Join(missing, ", "))
	}

	for i, incompatible := range registry.PreflightIncompatibilities {
		if err := validateBakeoffPreflight(i, incompatible, systems, registry); err != nil {
			return err
		}
		pair := bakeoffManifestPair{SystemID: incompatible.SystemID, Task: incompatible.Task}
		if prior, exists := pairCoverage[pair]; exists {
			return fmt.Errorf(
				"manifest pair %q/%q appears in both %s and preflight_incompatibilities",
				pair.SystemID,
				pair.Task,
				prior,
			)
		}
		pairCoverage[pair] = "preflight_incompatibilities"
	}
	for i, notRun := range registry.NotRun {
		if err := validateBakeoffNotRun(i, notRun, systems); err != nil {
			return err
		}
		pair := bakeoffManifestPair{SystemID: notRun.SystemID, Task: notRun.Task}
		if prior, exists := pairCoverage[pair]; exists {
			return fmt.Errorf(
				"manifest pair %q/%q appears in both %s and not_run",
				pair.SystemID,
				pair.Task,
				prior,
			)
		}
		pairCoverage[pair] = "not_run"
	}
	missingPairs := make([]string, 0)
	for pair := range expectedPairs {
		if _, covered := pairCoverage[pair]; !covered {
			missingPairs = append(missingPairs, fmt.Sprintf("%s/%s", pair.SystemID, pair.Task))
		}
	}
	if len(missingPairs) > 0 {
		sort.Strings(missingPairs)
		return fmt.Errorf(
			"manifest system/task coverage is incomplete: %s",
			strings.Join(missingPairs, ", "),
		)
	}
	if err := validateLegacyProviderOnlyShadowRegistryUse(
		registry,
		manifestSHA256,
	); err != nil {
		return err
	}
	if registry.CurrentProduction != nil {
		if err := validateBakeoffCurrentProduction(*registry.CurrentProduction, producedDate); err != nil {
			return err
		}
	}
	return nil
}

func validateLegacyProviderOnlyShadowRegistryUse(
	registry OpenModelBakeoffRegistry,
	manifestSHA256 string,
) error {
	if manifestSHA256 != legacyOpenModelFeasibilityManifestSHA256 {
		return nil
	}
	if !strings.HasPrefix(registry.Status, "diagnostic_") ||
		!strings.Contains(registry.Purpose, "not_promotion_evidence") ||
		registry.EvidencePolicy.AllHistoricalScreens != "diagnostic_only" ||
		registry.EvidencePolicy.ShadowFidelity !=
			"production_wire_contract_compatibility_screen_only" ||
		registry.EvidencePolicy.PromotionEligible {
		return fmt.Errorf(
			"legacy provider-only shadow systems require an explicitly diagnostic, non-promotion registry",
		)
	}
	for task, cells := range registry.Cells {
		for i, cell := range cells {
			if !legacyProviderOnlyShadowSystem(cell.SystemID) {
				continue
			}
			if cell.Lane != EvaluationLaneShadowFidelity ||
				cell.Assessment.PromotionEligible ||
				!strings.HasPrefix(cell.Assessment.Decision, "diagnostic_") {
				return fmt.Errorf(
					"cells[%q][%d]: legacy provider-only shadow evidence must be explicitly diagnostic and non-promotion",
					task,
					i,
				)
			}
		}
	}
	for i, preflight := range registry.PreflightIncompatibilities {
		if !legacyProviderOnlyShadowSystem(preflight.SystemID) {
			continue
		}
		if preflight.Lane != EvaluationLaneShadowFidelity ||
			preflight.PromotionEligible ||
			!strings.HasPrefix(preflight.Decision, "diagnostic_") {
			return fmt.Errorf(
				"preflight_incompatibilities[%d]: legacy provider-only shadow evidence must be explicitly diagnostic and non-promotion",
				i,
			)
		}
	}
	for i, notRun := range registry.NotRun {
		if !legacyProviderOnlyShadowSystem(notRun.SystemID) {
			continue
		}
		if notRun.Lane != EvaluationLaneShadowFidelity ||
			notRun.PromotionEligible || notRun.Decision != "not_run" {
			return fmt.Errorf(
				"not_run[%d]: legacy provider-only shadow entry must remain non-promotion",
				i,
			)
		}
	}
	return nil
}

// OpenModelBakeoffRequiredEvidenceSHA256 returns the private evidence
// identities that an operator must retain to reproduce the checked-in index.
// Repository-resident manifest, lineage, and current-state artifacts are
// validated separately by the CLI.
func OpenModelBakeoffRequiredEvidenceSHA256(
	registry OpenModelBakeoffRegistry,
) []string {
	set := make(map[string]struct{})
	for _, build := range registry.Bindings.EvaluatorBuilds {
		set[build.SHA256] = struct{}{}
	}
	for _, corpus := range registry.Bindings.Corpora {
		set[corpus.SHA256] = struct{}{}
	}
	for _, cells := range registry.Cells {
		for _, cell := range cells {
			set[cell.Artifact.SHA256] = struct{}{}
			if cell.Artifact.RouteSnapshotSHA256 != "" {
				set[cell.Artifact.RouteSnapshotSHA256] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(set))
	for digest := range set {
		result = append(result, digest)
	}
	sort.Strings(result)
	return result
}

func validateBakeoffCell(
	task Task,
	index int,
	cell OpenModelBakeoffRegistryCell,
	systems map[string]SystemConfig,
	registry OpenModelBakeoffRegistry,
) error {
	prefix := fmt.Sprintf("cells[%q][%d]", task, index)
	if strings.TrimSpace(cell.CellID) == "" || strings.TrimSpace(cell.SystemID) == "" ||
		strings.TrimSpace(cell.MethodologyVersion) == "" {
		return fmt.Errorf("%s: cell_id, system_id, and methodology_version are required", prefix)
	}
	system, exists := systems[cell.SystemID]
	if !exists {
		return fmt.Errorf("%s: system_id %q is absent from the bound manifest", prefix, cell.SystemID)
	}
	if !system.SupportsTask(task) {
		return fmt.Errorf("%s: bound system does not support task %q", prefix, task)
	}
	if cell.Lane != system.EvaluationLane {
		return fmt.Errorf("%s: lane does not match the bound system", prefix)
	}
	corpus, exists := registry.Bindings.Corpora[cell.CorpusBinding]
	if !exists {
		return fmt.Errorf("%s: unknown corpus_binding %q", prefix, cell.CorpusBinding)
	}
	if corpus.Task != task {
		return fmt.Errorf("%s: corpus_binding task does not match cell task", prefix)
	}
	selection, exists := registry.ComparisonSelections[cell.ComparisonSelection]
	if !exists {
		return fmt.Errorf("%s: unknown comparison_selection %q", prefix, cell.ComparisonSelection)
	}
	if selection.Task != task {
		return fmt.Errorf("%s: comparison_selection task does not match cell task", prefix)
	}
	if cell.Sample.Selected != selection.Selected || cell.Sample.Completed < 0 ||
		cell.Sample.Completed > cell.Sample.Selected || cell.Sample.Concurrency <= 0 ||
		cell.Sample.Selected > corpus.Records {
		return fmt.Errorf("%s: invalid sample counts or comparison selection", prefix)
	}
	if !safeRegistryBasename(cell.Artifact.Basename) || !validLowercaseSHA256(cell.Artifact.SHA256) {
		return fmt.Errorf("%s: artifact basename or sha256 is invalid", prefix)
	}
	hasSnapshot := cell.Artifact.RouteSnapshotSHA256 != ""
	hasDirectBinding := strings.TrimSpace(cell.Artifact.RouteBinding) != ""
	if hasSnapshot == hasDirectBinding {
		return fmt.Errorf("%s: artifact must bind exactly one route snapshot or direct route", prefix)
	}
	if hasSnapshot && !validLowercaseSHA256(cell.Artifact.RouteSnapshotSHA256) {
		return fmt.Errorf("%s: route_snapshot_sha256 is invalid", prefix)
	}
	if system.Provider == "openrouter" && !hasSnapshot {
		return fmt.Errorf("%s: OpenRouter cell must bind a route snapshot", prefix)
	}
	if system.Provider != "openrouter" && !hasDirectBinding {
		return fmt.Errorf("%s: direct-provider cell must state its route binding", prefix)
	}
	if err := validateBakeoffTaskMetrics(prefix, task, cell.Metrics); err != nil {
		return err
	}
	for name, value := range map[string]BakeoffCountRate{
		"schema_errors":  cell.Reliability.SchemaErrors,
		"runtime_errors": cell.Reliability.RuntimeErrors,
		"timeouts":       cell.Reliability.Timeouts,
	} {
		if err := validateBakeoffCountRate(prefix+".reliability."+name, value); err != nil {
			return err
		}
		if value.Denominator != cell.Sample.Completed {
			return fmt.Errorf("%s.reliability.%s denominator must equal completed", prefix, name)
		}
	}
	errorCodeTotal := 0
	for code, count := range cell.Reliability.ErrorCodes {
		if strings.TrimSpace(code) == "" || count <= 0 {
			return fmt.Errorf("%s: error_codes contains an invalid entry", prefix)
		}
		errorCodeTotal += count
	}
	if errorCodeTotal != cell.Reliability.SchemaErrors.Count+cell.Reliability.RuntimeErrors.Count {
		return fmt.Errorf("%s: error_codes do not reconcile to schema plus runtime errors", prefix)
	}
	if err := validateBakeoffLatency(prefix+".latency_ms", cell.LatencyMS, cell.Sample.Completed); err != nil {
		return err
	}
	if cell.Cost.CostMicroUSD < 0 ||
		cell.Cost.AccountingComplete < 0 || cell.Cost.AccountingIncomplete < 0 ||
		cell.Cost.AccountingComplete+cell.Cost.AccountingIncomplete != cell.Sample.Completed {
		return fmt.Errorf("%s: cost accounting does not reconcile to completed requests", prefix)
	}
	switch cell.Cost.CostSource {
	case "provider_reported", "manifest_estimate", "mixed", "unavailable":
	default:
		return fmt.Errorf("%s: unsupported cost_source %q", prefix, cell.Cost.CostSource)
	}
	if cell.Cost.CostSource == "unavailable" &&
		(cell.Cost.CostMicroUSD != 0 || cell.Cost.AccountingComplete != 0) {
		return fmt.Errorf("%s: unavailable cost cannot claim cost or complete accounting", prefix)
	}
	if cell.Cost.AccountingComplete == 0 && cell.Cost.CostSource != "unavailable" {
		return fmt.Errorf("%s: zero complete accounting requires unavailable cost_source", prefix)
	}
	if strings.TrimSpace(cell.Assessment.EvidenceTier) == "" ||
		strings.TrimSpace(cell.Assessment.Decision) == "" || len(cell.Assessment.ReasonCodes) == 0 {
		return fmt.Errorf("%s: assessment is incomplete", prefix)
	}
	if _, exists := registry.EvidencePolicy.Tiers[cell.Assessment.EvidenceTier]; !exists {
		return fmt.Errorf("%s: unknown evidence tier %q", prefix, cell.Assessment.EvidenceTier)
	}
	if registry.EvidencePolicy.PromotionEligible || cell.Assessment.PromotionEligible {
		return fmt.Errorf("%s: historical diagnostic cell cannot be promotion eligible", prefix)
	}
	for _, reason := range cell.Assessment.ReasonCodes {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("%s: assessment contains an empty reason code", prefix)
		}
	}
	return nil
}

func validateBakeoffTaskMetrics(prefix string, task Task, metrics map[string]json.RawMessage) error {
	var expected map[string]bool
	switch task {
	case TaskContentFilter:
		expected = map[string]bool{
			"english_raw_accuracy":     true,
			"non_english_raw_accuracy": true,
			"raw_balanced_accuracy":    true,
			"production_false_drops":   true,
			"production_correct_drops": true,
			"production_abstentions":   true,
		}
	case TaskMatcherExtract:
		expected = map[string]bool{
			"answered":                 true,
			"normalized_exact_title":   true,
			"exact_type":               true,
			"exact_year":               true,
			"combined_title_type_year": true,
			"combined_over_selected":   true,
			"strata":                   false,
		}
	case TaskJunkPurge:
		expected = map[string]bool{
			"teacher_binary_agreement":        true,
			"teacher_exact_verdict_agreement": true,
			"known_junk_catch":                true,
			"known_real_hold":                 true,
			"production_actions":              true,
		}
	default:
		return fmt.Errorf("%s: registry metric schema is not defined for task %q", prefix, task)
	}
	for key, required := range expected {
		if required {
			if _, exists := metrics[key]; !exists {
				return fmt.Errorf("%s.metrics: missing %q", prefix, key)
			}
		}
	}
	for key := range metrics {
		if _, exists := expected[key]; !exists {
			return fmt.Errorf("%s.metrics: unknown field %q", prefix, key)
		}

		if key == "raw_balanced_accuracy" {
			var rate float64
			if err := decodeStrictBakeoffRaw(metrics[key], &rate); err != nil || !validRate(rate) {
				return fmt.Errorf("%s.metrics.%s is invalid", prefix, key)
			}
			continue
		}
		if key == "production_actions" {
			var actions struct {
				Keep    int `json:"keep"`
				Junk    int `json:"junk"`
				Abstain int `json:"abstain"`
			}
			if err := decodeStrictBakeoffRaw(metrics[key], &actions); err != nil ||
				actions.Keep < 0 || actions.Junk < 0 || actions.Abstain < 0 {
				return fmt.Errorf("%s.metrics.%s is invalid", prefix, key)
			}
			continue
		}
		if key == "strata" {
			var strata map[string]struct {
				Selected                         int              `json:"selected"`
				Answered                         BakeoffCountRate `json:"answered"`
				NormalizedExactTitleOverSelected BakeoffCountRate `json:"normalized_exact_title_over_selected"`
			}
			if err := decodeStrictBakeoffRaw(metrics[key], &strata); err != nil || len(strata) == 0 {
				return fmt.Errorf("%s.metrics.strata is invalid", prefix)
			}
			for name, stratum := range strata {
				if strings.TrimSpace(name) == "" || stratum.Selected <= 0 ||
					stratum.Answered.Denominator != stratum.Selected ||
					stratum.NormalizedExactTitleOverSelected.Denominator != stratum.Selected {
					return fmt.Errorf("%s.metrics.strata[%q] is invalid", prefix, name)
				}
				if err := validateBakeoffCountRate(prefix+".metrics.strata."+name+".answered", stratum.Answered); err != nil {
					return err
				}
				if err := validateBakeoffCountRate(prefix+".metrics.strata."+name+".normalized_exact_title_over_selected", stratum.NormalizedExactTitleOverSelected); err != nil {
					return err
				}
			}
			continue
		}
		var countRate BakeoffCountRate
		if err := decodeStrictBakeoffRaw(metrics[key], &countRate); err != nil {
			return fmt.Errorf("%s.metrics.%s is invalid", prefix, key)
		}
		if err := validateBakeoffCountRate(prefix+".metrics."+key, countRate); err != nil {
			return err
		}
	}
	return nil
}

func validateBakeoffPreflight(
	index int,
	preflight BakeoffPreflightIncompatibility,
	systems map[string]SystemConfig,
	registry OpenModelBakeoffRegistry,
) error {
	prefix := fmt.Sprintf("preflight_incompatibilities[%d]", index)
	system, exists := systems[preflight.SystemID]
	if !exists || !system.SupportsTask(preflight.Task) || system.EvaluationLane != preflight.Lane {
		return fmt.Errorf("%s does not match a system/task/lane in the bound manifest", prefix)
	}
	if strings.TrimSpace(preflight.Stage) == "" || strings.TrimSpace(preflight.Decision) == "" ||
		len(preflight.ReasonCodes) == 0 || preflight.PromotionEligible {
		return fmt.Errorf("%s is incomplete or promotion eligible", prefix)
	}
	if _, exists := registry.EvidencePolicy.Tiers[preflight.EvidenceTier]; !exists {
		return fmt.Errorf("%s has unknown evidence tier %q", prefix, preflight.EvidenceTier)
	}
	if preflight.MatchedEndpointCount != nil && *preflight.MatchedEndpointCount < 0 {
		return fmt.Errorf("%s matched_endpoint_count cannot be negative", prefix)
	}
	for _, reason := range preflight.ReasonCodes {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("%s contains an empty reason code", prefix)
		}
	}
	return nil
}

func validateBakeoffNotRun(
	index int,
	notRun BakeoffNotRun,
	systems map[string]SystemConfig,
) error {
	prefix := fmt.Sprintf("not_run[%d]", index)
	system, exists := systems[notRun.SystemID]
	if !exists || !system.SupportsTask(notRun.Task) || system.EvaluationLane != notRun.Lane {
		return fmt.Errorf("%s does not match a system/task/lane in the bound manifest", prefix)
	}
	if strings.TrimSpace(notRun.Stage) == "" || notRun.Decision != "not_run" ||
		len(notRun.ReasonCodes) == 0 || notRun.PromotionEligible {
		return fmt.Errorf("%s is incomplete, has the wrong decision, or is promotion eligible", prefix)
	}
	for _, reason := range notRun.ReasonCodes {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("%s contains an empty reason code", prefix)
		}
	}
	return nil
}

func validateBakeoffCurrentProduction(
	inventory BakeoffCurrentProductionInventory,
	registryProducedDate time.Time,
) error {
	asOfDate, err := time.Parse("2006-01-02", inventory.AsOfUTC)
	if err != nil {
		return fmt.Errorf("current_production.as_of_utc must be YYYY-MM-DD")
	}
	if asOfDate.After(registryProducedDate) {
		return fmt.Errorf("current_production.as_of_utc cannot be later than produced_utc")
	}
	if !inventory.PriorRegistryLiveClaimsSuperseded || len(inventory.SourceArtifacts) == 0 ||
		len(inventory.Routes) == 0 {
		return fmt.Errorf("current_production must supersede prior live claims and bind sources and routes")
	}
	sourceArtifacts := make(map[string]struct{}, len(inventory.SourceArtifacts))
	for i, source := range inventory.SourceArtifacts {
		if err := validateBakeoffBoundArtifact(fmt.Sprintf("current_production.source_artifacts[%d]", i), source, false); err != nil {
			return err
		}
		if _, duplicate := sourceArtifacts[source.Basename]; duplicate {
			return fmt.Errorf("current_production contains duplicate source artifact %q", source.Basename)
		}
		sourceArtifacts[source.Basename] = struct{}{}
	}
	activeOpenWeight := false
	for name, route := range inventory.Routes {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(route.Provider) == "" ||
			strings.TrimSpace(route.Model) == "" || strings.TrimSpace(route.RuntimeState) == "" ||
			!safeRegistryBasename(route.SourceArtifact) {
			return fmt.Errorf("current_production.routes[%q] is incomplete", name)
		}
		if _, exists := sourceArtifacts[route.SourceArtifact]; !exists {
			return fmt.Errorf("current_production.routes[%q] references an unbound source artifact", name)
		}
		switch route.Provider {
		case "openai_direct", "openrouter":
		default:
			return fmt.Errorf("current_production.routes[%q] has unsupported provider %q", name, route.Provider)
		}
		switch route.RuntimeState {
		case "live", "live_enforcing", "configured_safe_off", "shadow_only":
		default:
			return fmt.Errorf("current_production.routes[%q] has unsupported runtime_state %q", name, route.RuntimeState)
		}
		if route.OpenWeight &&
			(route.RuntimeState == "live" || route.RuntimeState == "live_enforcing") {
			activeOpenWeight = true
		}
	}
	if inventory.NoActiveOpenWeightDecisionPath == activeOpenWeight {
		return fmt.Errorf("current_production open-weight summary contradicts route states")
	}
	return nil
}

func validateBakeoffLatency(prefix string, latency BakeoffCellLatency, completed int) error {
	if latency.Deadline <= 0 || latency.RouteAudit.Attempted < 0 ||
		latency.RouteAudit.Failed < 0 || latency.RouteAudit.Failed > latency.RouteAudit.Attempted ||
		latency.RouteAudit.Attempted > completed {
		return fmt.Errorf("%s has invalid deadline or route-audit counts", prefix)
	}
	for name, percentiles := range map[string]BakeoffLatencyPercentiles{
		"primary":         latency.Primary,
		"operation_total": latency.OperationTotal,
	} {
		if percentiles.P50 < 0 || percentiles.P50 > percentiles.P95 ||
			percentiles.P95 > percentiles.P99 || percentiles.P99 > percentiles.Max {
			return fmt.Errorf("%s.%s percentiles are invalid", prefix, name)
		}
	}
	if latency.OperationTotal.P50 < latency.Primary.P50 ||
		latency.OperationTotal.P95 < latency.Primary.P95 ||
		latency.OperationTotal.P99 < latency.Primary.P99 ||
		latency.OperationTotal.Max < latency.Primary.Max {
		return fmt.Errorf("%s operation_total cannot be lower than primary", prefix)
	}
	return nil
}

func validateBakeoffCountRate(path string, value BakeoffCountRate) error {
	if value.Denominator == 0 {
		if value.Count == 0 && value.Rate == 0 {
			return nil
		}
		return fmt.Errorf("%s zero denominator must carry zero count and rate", path)
	}
	if value.Count < 0 || value.Denominator < 0 || value.Count > value.Denominator ||
		!validRate(value.Rate) || math.Abs(value.Rate-float64(value.Count)/float64(value.Denominator)) > 1e-12 {
		return fmt.Errorf("%s count, denominator, and rate do not reconcile", path)
	}
	return nil
}

func validRate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func decodeStrictBakeoffRaw(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateBakeoffBoundArtifact(path string, artifact BakeoffBoundArtifact, requireRelationship bool) error {
	if !safeRegistryBasename(artifact.Basename) || !validLowercaseSHA256(artifact.SHA256) {
		return fmt.Errorf("%s basename or sha256 is invalid", path)
	}
	if requireRelationship && strings.TrimSpace(artifact.Relationship) == "" {
		return fmt.Errorf("%s relationship is required", path)
	}
	return nil
}

func safeRegistryBasename(value string) bool {
	return value != "" && value != "." && value != ".." &&
		filepath.Base(value) == value && !filepath.IsAbs(value) &&
		!strings.ContainsAny(value, `/\\`)
}

func validLowercaseSHA256(value string) bool {
	return lowercaseSHA256Pattern.MatchString(value)
}

func validEvaluationTask(task Task) bool {
	switch task {
	case TaskMatcherExtract, TaskMatcherRerank, TaskContentFilter, TaskJunkPurge:
		return true
	default:
		return false
	}
}

func validateBakeoffRegistryRawSourceSafety(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return walkBakeoffRegistrySourceSafe(value, nil)
}

func walkBakeoffRegistrySourceSafe(value any, path []string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if forbiddenBakeoffRegistryField(key) {
				return fmt.Errorf("forbidden source-bearing field %q", key)
			}
			if credentialLikeBakeoffRegistryMapKey(key) &&
				!allowedCredentialLikeBakeoffRegistrySchemaKey(path, key) {
				return fmt.Errorf("unsafe map key %q: credential-like field name is forbidden", key)
			}
			if err := validateBakeoffRegistrySourceSafeString(key); err != nil {
				return fmt.Errorf("unsafe map key %q: %w", key, err)
			}
			if err := walkBakeoffRegistrySourceSafe(child, appendRegistryPath(path, key)); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := walkBakeoffRegistrySourceSafe(child, path); err != nil {
				return err
			}
		}
	case string:
		return validateBakeoffRegistrySourceSafeString(typed)
	}
	return nil
}

func appendRegistryPath(path []string, key string) []string {
	next := make([]string, len(path)+1)
	copy(next, path)
	next[len(path)] = key
	return next
}

// credentialLikeBakeoffRegistryMapKey examines JSON object keys independently
// of their values. This closes the gap where arbitrary-key maps (for example,
// task_caveats and metrics) could otherwise carry a benign-looking value under
// a credential-bearing key. Splitting camelCase as well as punctuation keeps
// separators and harmless suffixes from bypassing the check.
func credentialLikeBakeoffRegistryMapKey(key string) bool {
	separated := registryKeyAcronymBoundary.ReplaceAllString(key, `${1} ${2}`)
	separated = registryKeyWordBoundary.ReplaceAllString(separated, `${1} ${2}`)
	tokens := strings.FieldsFunc(strings.ToLower(separated), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})

	for _, token := range tokens {
		switch token {
		case "password", "passwords", "passwd", "pwd", "passphrase", "passphrases",
			"passcode", "passcodes", "secret", "secrets", "token", "tokens",
			"authorization", "authorisation", "bearer", "dsn", "credential", "credentials":
			return true
		}
	}
	for index := 0; index+1 < len(tokens); index++ {
		if tokens[index+1] == "key" && credentialBearingKeyQualifier(tokens[index]) {
			return true
		}
		if index+2 < len(tokens) && tokens[index+2] == "key" &&
			credentialBearingKeyQualifier(tokens[index]+tokens[index+1]) {
			return true
		}
	}

	compact := strings.Join(tokens, "")
	for _, atomicMarker := range []string{
		"password", "passwd", "passphrase", "passcode", "authorization", "authorisation",
		"bearer",
	} {
		if strings.Contains(compact, atomicMarker) {
			return true
		}
	}
	for _, qualifier := range registryCredentialBearingKeyQualifiers {
		if compactContainsCredentialNoun(compact, qualifier+"key") {
			return true
		}
	}
	for _, qualifier := range []string{
		"api", "auth", "access", "refresh", "session", "bearer", "id",
	} {
		if strings.Contains(compact, qualifier+"token") {
			return true
		}
	}
	for _, noun := range []string{"credentials", "credential", "secret", "token", "pwd"} {
		if compactContainsCredentialNoun(compact, noun) {
			return true
		}
	}
	for _, pattern := range []struct {
		qualifiers []string
		noun       string
	}{
		{
			qualifiers: []string{"auth", "authorization", "bearer", "api", "oauth"},
			noun:       "header",
		},
		{
			qualifiers: []string{"session", "auth", "authentication", "login", "secure", "rememberme"},
			noun:       "cookie",
		},
		{
			qualifiers: []string{
				"db", "database", "smtp", "ftp", "sftp", "ssh", "redis", "postgres",
				"postgresql", "mysql", "mariadb", "mongo", "mongodb", "email", "login",
				"user", "account", "admin",
			},
			noun: "pass",
		},
		{
			qualifiers: []string{
				"db", "database", "jdbc", "redis", "postgres", "postgresql", "mysql",
				"mariadb", "mongo", "mongodb", "smtp",
			},
			noun: "url",
		},
		{
			qualifiers: []string{
				"db", "database", "jdbc", "redis", "postgres", "postgresql", "mysql",
				"mariadb", "mongo", "mongodb", "smtp", "connection",
			},
			noun: "uri",
		},
	} {
		if compactContainsQualifiedCredentialNoun(compact, pattern.qualifiers, pattern.noun) {
			return true
		}
	}
	if compactContainsCredentialNoun(compact, "connectionstring") {
		return true
	}
	return compactContainsCredentialNoun(compact, "dsn")
}

func compactContainsQualifiedCredentialNoun(value string, qualifiers []string, noun string) bool {
	for _, qualifier := range qualifiers {
		if compactContainsCredentialNoun(value, qualifier+noun) {
			return true
		}
	}
	return false
}

// compactContainsCredentialNoun recognizes a credential noun only at a safe
// lexical termination. This catches compact keys such as sharedsecretvalue and
// jwttoken while deliberately not treating words such as secretary or
// tokenization as credential fields.
func compactContainsCredentialNoun(value string, noun string) bool {
	for searchFrom := 0; searchFrom < len(value); {
		relative := strings.Index(value[searchFrom:], noun)
		if relative < 0 {
			return false
		}
		following := value[searchFrom+relative+len(noun):]
		if following == "" {
			return true
		}
		for _, descriptor := range []string{
			"backup", "ciphertext", "data", "encrypted", "file", "hash", "id",
			"included", "key", "material", "name", "path", "pem", "raw", "value",
			"version",
		} {
			if strings.HasPrefix(following, descriptor) {
				return true
			}
		}
		if len(following) > 1 && following[0] == 'v' && following[1] >= '0' && following[1] <= '9' {
			return true
		}
		searchFrom += relative + len(noun)
	}
	return false
}

func credentialBearingKeyQualifier(value string) bool {
	for _, qualifier := range registryCredentialBearingKeyQualifiers {
		if value == qualifier {
			return true
		}
	}
	return false
}

func allowedCredentialLikeBakeoffRegistrySchemaKey(path []string, key string) bool {
	return len(path) == 1 && path[0] == "source_safety" && key == "credentials_included"
}

func forbiddenBakeoffRegistryField(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "prompt", "raw_prompt", "reply", "raw_reply", "response",
		"raw_response", "source_text", "source_row", "row_id",
		"case_id", "request_id", "credential", "credentials", "api_key",
		"authorization", "password", "passwd", "secret", "token",
		"api_token", "auth_token", "access_token", "refresh_token",
		"client_secret", "private_key", "aws_access_key_id",
		"aws_secret_access_key", "aws_session_token":
		return true
	default:
		return false
	}
}

func validateBakeoffRegistrySourceSafeString(value string) error {
	if len(value) > 4<<10 || strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("free-form text is multiline or exceeds its limit")
	}
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "/") || strings.HasPrefix(lower, "~/") ||
		strings.HasPrefix(lower, `\\`) ||
		strings.HasPrefix(lower, "file://") ||
		(len(lower) >= 3 && lower[1] == ':' &&
			(lower[2] == '/' || lower[2] == '\\')) {
		return fmt.Errorf("absolute filesystem path is forbidden")
	}
	for _, marker := range []string{
		"bearer ", "authorization:", "x-api-key:",
		"api_key=", "apikey=", "access_token=", "refresh_token=",
		"aws_secret_access_key=", "postgres://", "postgresql://",
	} {
		if strings.Contains(lower, marker) {
			return fmt.Errorf("credential-like material is forbidden")
		}
	}
	if registryCredentialPattern.MatchString(trimmed) ||
		registryAWSAccessKeyPattern.MatchString(trimmed) ||
		registryPEMPrivateKeyPattern.MatchString(trimmed) {
		return fmt.Errorf("credential-like material is forbidden")
	}
	return nil
}
