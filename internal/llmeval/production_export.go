package llmeval

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	ProductionCandidateExportStatus = "candidate_not_gold"
	// ProductionCandidateExporterID identifies the legacy read-only
	// reconstruction. It remains useful for diagnostics but is deliberately
	// ineligible to establish production-source verification at freeze time.
	ProductionCandidateExporterID = "bitagent-read-only-production-export-v1"
	// CaptureLedgerProductionCandidateExporterID is reserved for the audited
	// adapter that reads the prospective capture/admission ledger and
	// validates the exact stored production request contracts.
	CaptureLedgerProductionCandidateExporterID = "bitagent-capture-ledger-production-export-v1"
	ProductionDiagnosticSamplingOrigin         = "diagnostic_reconstruction"
	ProductionCaptureSamplingOrigin            = "natural_capture+safety_topup_capture"
	ProductionCandidateExportScope             = "privacy-minimized read-only production-derived sampling candidates; placeholders are not truth and cannot be executed or scored"
	ProductionCandidateLabelStatus             = "sampling_candidate_unreviewed_not_gold"
	ProductionPrivacySidecarStatus             = "local_raw_infohash_admission_map"
	ProductionPrivacySidecarScope              = "local-only raw infohash admission map bound to the production candidate corpus; never upload, commit, or print"

	maxProductionCandidateExportManifestBytes = 4 << 20
	maxProductionPrivacySidecarBytes          = 64 << 20
)

// ProductionCandidateSeed is an in-memory row returned by the audited,
// read-only source adapter. SourceKey is the lower-case 40-hex infohash and is
// copied only to the local mode-0600 privacy sidecar. GroupKey is never copied
// to an artifact. The builder replaces both with plan-bound SHA-256 aliases in
// the corpus JSONL.
//
// Exactly one task input must be populated. Expected values are deliberately
// absent: this is sampling material, not truth.
type ProductionCandidateSeed struct {
	Task      Task
	SourceKey string
	// CandidateKey identifies one exact captured request. Diagnostic sources
	// may leave it empty, in which case SourceKey is used. Capture-ledger
	// exports set the lower-case 64-hex capture key so two legitimate calls
	// for one torrent (for example local then API rerank candidates) remain
	// distinct while sharing the same admission-sidecar infohash.
	CandidateKey string
	GroupKey     string
	PrimarySuite string
	SliceIDs     []string
	Stratum      string
	PrivacySafe  bool

	MatcherExtract *MatcherExtractInput
	MatcherRerank  *MatcherRerankInput
	ContentFilter  *ContentFilterInput
	JunkPurge      *JunkPurgeInput
}

// ProductionSourceSnapshot describes the repeatable-read transaction and
// exact query/config identities that produced the in-memory seeds. Database
// names, DSNs, raw source keys, and transaction snapshot text are hashed by the
// source adapter before reaching this contract.
type ProductionSourceSnapshot struct {
	ObservedAtUTC             string
	DatabaseIdentitySHA256    string
	TransactionSnapshotSHA256 string
	// UnifiedSourceSnapshotSHA256 is optional for diagnostic reconstructions.
	// The audited capture-ledger adapter sets it to one hash that binds the
	// complete four-task snapshot, allowing the two multi-task human-review
	// workflows to prove one shared immutable source snapshot.
	UnifiedSourceSnapshotSHA256 string
	QueryBundleSHA256           string
	TaskQuerySHA256             map[Task]string
	ReadOnlyVerified            bool
	RepeatableReadVerified      bool
	NoModelCalls                bool
	Seeds                       []ProductionCandidateSeed
	TaskLimitations             map[Task][]string
	TaskCohortCounts            map[Task]ProductionSourceCohortCounts
}

// ProductionSourceCohortCounts is aggregate, non-identifying source
// accounting. Exclusion categories may overlap; they prove that privacy
// exclusions were observed and enforced, not that the fields form a partition.
type ProductionSourceCohortCounts struct {
	EligibleBeforePrivacy      int64            `json:"eligible_before_privacy"`
	NativePrivateExcluded      int64            `json:"native_private_excluded"`
	PrivateEvidenceExcluded    int64            `json:"private_evidence_excluded"`
	PrivacySafeAvailable       int64            `json:"privacy_safe_available"`
	SourceRowsRead             int64            `json:"source_rows_read"`
	DeduplicatedCandidateCases int64            `json:"deduplicated_candidate_cases"`
	RowsByStratum              map[string]int64 `json:"rows_by_stratum,omitempty"`
}

type ProductionCandidateTaskManifest struct {
	Task                 Task                         `json:"task"`
	QuerySHA256          string                       `json:"query_sha256"`
	SourceSnapshotSHA256 string                       `json:"source_snapshot_sha256"`
	CandidateCases       int                          `json:"candidate_cases"`
	CandidateGroups      int                          `json:"candidate_groups"`
	Limitations          []string                     `json:"limitations,omitempty"`
	CohortCounts         ProductionSourceCohortCounts `json:"cohort_counts"`
}

// ProductionCandidateExportManifest is safe to retain with a local candidate
// artifact. It contains hashes and aggregate counts only, never source text,
// paths, info hashes, database names, or connection material.
type ProductionCandidateExportManifest struct {
	SchemaVersion              int                               `json:"schema_version"`
	Status                     string                            `json:"status"`
	Scope                      string                            `json:"scope"`
	ExporterID                 string                            `json:"exporter_id"`
	SamplingOrigin             string                            `json:"sampling_origin"`
	PlanID                     string                            `json:"plan_id"`
	PlanSHA256                 string                            `json:"plan_sha256"`
	ObservedAtUTC              string                            `json:"observed_at_utc"`
	DatabaseIdentitySHA256     string                            `json:"database_identity_sha256"`
	TransactionSnapshotSHA256  string                            `json:"transaction_snapshot_sha256"`
	QueryBundleSHA256          string                            `json:"query_bundle_sha256"`
	ReadOnlyVerified           bool                              `json:"read_only_verified"`
	RepeatableReadVerified     bool                              `json:"repeatable_read_verified"`
	NoModelCalls               bool                              `json:"no_model_calls"`
	PrivacyStatus              PrivacyVerificationStatus         `json:"privacy_status"`
	LabelStatus                string                            `json:"label_status"`
	CandidateCorpusSHA256      string                            `json:"candidate_corpus_sha256"`
	PrivacySidecarSHA256       string                            `json:"privacy_sidecar_sha256"`
	CandidateCases             int                               `json:"candidate_cases"`
	FreezeCompatibilityChecked bool                              `json:"freeze_compatibility_checked"`
	DevelopmentPreviewSHA256   string                            `json:"development_preview_sha256"`
	HoldoutPreviewSHA256       string                            `json:"holdout_preview_sha256"`
	Tasks                      []ProductionCandidateTaskManifest `json:"tasks"`
}

// ProductionPrivacySidecar is the sole artifact that contains raw infohashes.
// It must remain local, mode 0600, and separate from review/model-visible
// artifacts. Its exact byte SHA-256 is carried through export, freeze, and gold
// closure so execution can recheck each admission immediately before egress.
type ProductionPrivacySidecar struct {
	SchemaVersion         int                             `json:"schema_version"`
	Status                string                          `json:"status"`
	Scope                 string                          `json:"scope"`
	PlanID                string                          `json:"plan_id"`
	PlanSHA256            string                          `json:"plan_sha256"`
	CandidateCorpusSHA256 string                          `json:"candidate_corpus_sha256"`
	Entries               []ProductionPrivacySidecarEntry `json:"entries"`
}

type ProductionPrivacySidecarEntry struct {
	CaseID      string `json:"case_id"`
	Task        Task   `json:"task"`
	InfoHashHex string `json:"info_hash_hex"`
}

type ProductionCandidateExport struct {
	Corpus         Corpus
	Manifest       ProductionCandidateExportManifest
	PrivacySidecar ProductionPrivacySidecar
}

// BuildProductionCandidateExport converts privacy-checked source seeds to the
// existing Corpus JSONL envelope so freeze/review tooling can consume it. The
// closed sampling_candidate/unreviewed state is rejected by hosted execution,
// Score, and Compare; human promotion must replace it with gold.
func BuildProductionCandidateExport(
	plan CorpusPlan,
	planSHA256 string,
	snapshot ProductionSourceSnapshot,
) (ProductionCandidateExport, error) {
	if err := plan.Validate(); err != nil {
		return ProductionCandidateExport{}, fmt.Errorf("corpus plan: %w", err)
	}
	if err := validateSHA256("plan_sha256", planSHA256); err != nil {
		return ProductionCandidateExport{}, err
	}
	if err := validateProductionSourceSnapshot(plan, snapshot); err != nil {
		return ProductionCandidateExport{}, err
	}

	seeds := append([]ProductionCandidateSeed(nil), snapshot.Seeds...)
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].Task != seeds[j].Task {
			return seeds[i].Task < seeds[j].Task
		}
		if seeds[i].SourceKey != seeds[j].SourceKey {
			return seeds[i].SourceKey < seeds[j].SourceKey
		}
		return seeds[i].GroupKey < seeds[j].GroupKey
	})

	taskPlans := make(map[Task]CorpusPlanTask, len(plan.Tasks))
	for _, taskPlan := range plan.Tasks {
		taskPlans[taskPlan.Task] = taskPlan
	}
	seenCandidate := make(map[string]struct{}, len(seeds))
	seedsByTask := make(map[Task][]ProductionCandidateSeed, len(plan.Tasks))
	for i, seed := range seeds {
		taskPlan, ok := taskPlans[seed.Task]
		if !ok {
			return ProductionCandidateExport{}, fmt.Errorf(
				"source seed %d: task %q is not in plan",
				i+1,
				seed.Task,
			)
		}
		if err := seed.validate(taskPlan); err != nil {
			return ProductionCandidateExport{}, fmt.Errorf(
				"source seed %d: %w",
				i+1,
				err,
			)
		}
		candidateKey := seed.candidateIdentity()
		identity := string(seed.Task) + "\x00" + candidateKey
		if _, exists := seenCandidate[identity]; exists {
			return ProductionCandidateExport{}, fmt.Errorf(
				"source seed %d: duplicate task/candidate key",
				i+1,
			)
		}
		seenCandidate[identity] = struct{}{}
		seedsByTask[seed.Task] = append(seedsByTask[seed.Task], seed)
	}

	records := make([]CorpusRecord, 0, len(seeds))
	sidecarEntries := make([]ProductionPrivacySidecarEntry, 0, len(seeds))
	taskManifests := make([]ProductionCandidateTaskManifest, 0, len(plan.Tasks))
	for _, task := range orderedTasks {
		taskSeeds := seedsByTask[task]
		if len(taskSeeds) == 0 {
			return ProductionCandidateExport{}, fmt.Errorf(
				"source snapshot contains no %q candidates",
				task,
			)
		}
		cohortCounts, ok := snapshot.TaskCohortCounts[task]
		if !ok {
			return ProductionCandidateExport{}, fmt.Errorf(
				"source snapshot has no %q cohort counts",
				task,
			)
		}
		if err := validateProductionCohortCounts(
			task,
			cohortCounts,
			len(taskSeeds),
		); err != nil {
			return ProductionCandidateExport{}, err
		}
		sourceSHA, err := productionTaskSourceSHA256(
			planSHA256,
			snapshot,
			task,
			taskSeeds,
		)
		if err != nil {
			return ProductionCandidateExport{}, err
		}
		groups := make(map[string]struct{})
		for _, seed := range taskSeeds {
			record, err := productionCandidateRecord(
				plan,
				sourceSHA,
				seed,
			)
			if err != nil {
				return ProductionCandidateExport{}, err
			}
			groups[record.GroupID] = struct{}{}
			records = append(records, record)
			sidecarEntries = append(
				sidecarEntries,
				ProductionPrivacySidecarEntry{
					CaseID:      record.CaseID,
					Task:        record.Task,
					InfoHashHex: seed.SourceKey,
				},
			)
		}
		limitations := append(
			[]string(nil),
			snapshot.TaskLimitations[task]...,
		)
		sort.Strings(limitations)
		taskManifests = append(taskManifests, ProductionCandidateTaskManifest{
			Task:                 task,
			QuerySHA256:          snapshot.TaskQuerySHA256[task],
			SourceSnapshotSHA256: sourceSHA,
			CandidateCases:       len(taskSeeds),
			CandidateGroups:      len(groups),
			Limitations:          limitations,
			CohortCounts: cloneProductionCohortCounts(
				cohortCounts,
			),
		})
	}

	corpus, err := NewCorpus(records)
	if err != nil {
		return ProductionCandidateExport{}, fmt.Errorf(
			"candidate corpus: %w",
			err,
		)
	}
	preview, err := FreezeCandidateCorpus(plan, planSHA256, corpus)
	if err != nil {
		return ProductionCandidateExport{}, fmt.Errorf(
			"candidate corpus is not freeze-compatible: %w",
			err,
		)
	}
	sort.Slice(sidecarEntries, func(i, j int) bool {
		if sidecarEntries[i].Task != sidecarEntries[j].Task {
			return sidecarEntries[i].Task < sidecarEntries[j].Task
		}
		return sidecarEntries[i].CaseID < sidecarEntries[j].CaseID
	})
	sidecar := ProductionPrivacySidecar{
		SchemaVersion:         SchemaVersion,
		Status:                ProductionPrivacySidecarStatus,
		Scope:                 ProductionPrivacySidecarScope,
		PlanID:                plan.PlanID,
		PlanSHA256:            planSHA256,
		CandidateCorpusSHA256: corpus.SHA256,
		Entries:               sidecarEntries,
	}
	sidecarBytes, err := MarshalProductionPrivacySidecar(sidecar)
	if err != nil {
		return ProductionCandidateExport{}, err
	}
	sidecarSHA256 := sha256Hex(sidecarBytes)

	return ProductionCandidateExport{
		Corpus:         corpus,
		PrivacySidecar: sidecar,
		Manifest: ProductionCandidateExportManifest{
			SchemaVersion:              SchemaVersion,
			Status:                     ProductionCandidateExportStatus,
			Scope:                      ProductionCandidateExportScope,
			ExporterID:                 ProductionCandidateExporterID,
			SamplingOrigin:             ProductionDiagnosticSamplingOrigin,
			PlanID:                     plan.PlanID,
			PlanSHA256:                 planSHA256,
			ObservedAtUTC:              snapshot.ObservedAtUTC,
			DatabaseIdentitySHA256:     snapshot.DatabaseIdentitySHA256,
			TransactionSnapshotSHA256:  snapshot.TransactionSnapshotSHA256,
			QueryBundleSHA256:          snapshot.QueryBundleSHA256,
			ReadOnlyVerified:           snapshot.ReadOnlyVerified,
			RepeatableReadVerified:     snapshot.RepeatableReadVerified,
			NoModelCalls:               snapshot.NoModelCalls,
			PrivacyStatus:              PrivacyVerifiedPostRulePublic,
			LabelStatus:                ProductionCandidateLabelStatus,
			CandidateCorpusSHA256:      corpus.SHA256,
			PrivacySidecarSHA256:       sidecarSHA256,
			CandidateCases:             len(corpus.Records),
			FreezeCompatibilityChecked: true,
			DevelopmentPreviewSHA256:   preview.Development.SHA256,
			HoldoutPreviewSHA256:       preview.Holdout.SHA256,
			Tasks:                      taskManifests,
		},
	}, nil
}

// MarshalProductionPrivacySidecar returns the one canonical byte form used
// when the export manifest binds the sidecar. A trailing LF is part of the
// artifact identity.
func MarshalProductionPrivacySidecar(
	sidecar ProductionPrivacySidecar,
) ([]byte, error) {
	if err := validateProductionPrivacySidecarStructure(sidecar); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sidecar)
	if err != nil {
		return nil, fmt.Errorf("encode production privacy sidecar: %w", err)
	}
	return append(raw, '\n'), nil
}

// WriteProductionPrivacySidecar writes only the canonical local sidecar form
// and returns the exact byte SHA-256. File permissions are enforced by the CLI
// because an io.Writer does not expose a portable file mode.
func WriteProductionPrivacySidecar(
	w io.Writer,
	sidecar ProductionPrivacySidecar,
) (string, error) {
	if w == nil {
		return "", fmt.Errorf("production privacy sidecar writer is nil")
	}
	raw, err := MarshalProductionPrivacySidecar(sidecar)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(raw); err != nil {
		return "", fmt.Errorf("write production privacy sidecar: %w", err)
	}
	return sha256Hex(raw), nil
}

// ReadProductionPrivacySidecar strictly decodes a local sidecar and returns
// the SHA-256 of the exact bytes read. Unknown fields, duplicate keys, trailing
// values, malformed infohashes, and non-canonical ordering fail closed.
func ReadProductionPrivacySidecar(
	r io.Reader,
) (ProductionPrivacySidecar, string, error) {
	if r == nil {
		return ProductionPrivacySidecar{}, "", fmt.Errorf(
			"production privacy sidecar reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(
		r,
		maxProductionPrivacySidecarBytes+1,
	))
	if err != nil {
		return ProductionPrivacySidecar{}, "", fmt.Errorf(
			"read production privacy sidecar: %w",
			err,
		)
	}
	if len(raw) == 0 {
		return ProductionPrivacySidecar{}, "", fmt.Errorf(
			"production privacy sidecar is empty",
		)
	}
	if len(raw) > maxProductionPrivacySidecarBytes {
		return ProductionPrivacySidecar{}, "", fmt.Errorf(
			"production privacy sidecar exceeds the %d-byte limit",
			maxProductionPrivacySidecarBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return ProductionPrivacySidecar{}, "", fmt.Errorf(
			"decode production privacy sidecar: %w",
			err,
		)
	}
	var sidecar ProductionPrivacySidecar
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sidecar); err != nil {
		return ProductionPrivacySidecar{}, "", fmt.Errorf(
			"decode production privacy sidecar: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ProductionPrivacySidecar{}, "", fmt.Errorf(
				"decode production privacy sidecar: multiple JSON values",
			)
		}
		return ProductionPrivacySidecar{}, "", fmt.Errorf(
			"decode production privacy sidecar: trailing data: %w",
			err,
		)
	}
	if err := validateProductionPrivacySidecarStructure(sidecar); err != nil {
		return ProductionPrivacySidecar{}, "", err
	}
	return sidecar, sha256Hex(raw), nil
}

// ValidateProductionPrivacySidecar binds the exact local sidecar bytes to the
// plan, export manifest, and complete unreviewed candidate corpus. It never
// accepts a subset because omissions would disable a later per-case recheck.
func ValidateProductionPrivacySidecar(
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
	manifest ProductionCandidateExportManifest,
	sidecar ProductionPrivacySidecar,
	sidecarSHA256 string,
) error {
	if err := validateSHA256(
		"production privacy sidecar sha256",
		sidecarSHA256,
	); err != nil {
		return err
	}
	if manifest.PrivacySidecarSHA256 != sidecarSHA256 {
		return fmt.Errorf(
			"production privacy sidecar bytes do not match the export manifest",
		)
	}
	if err := ValidateProductionCandidateExportManifest(
		plan,
		planSHA256,
		candidates,
		manifest,
	); err != nil {
		return err
	}
	return ValidateProductionPrivacySidecarBinding(
		plan,
		planSHA256,
		candidates,
		sidecar,
		sidecarSHA256,
		manifest.PrivacySidecarSHA256,
	)
}

// ValidateProductionPrivacySidecarBinding validates the exact sidecar against
// a later hash-chain stage (freeze or gold closure) without requiring the
// earlier export manifest bytes to remain online.
func ValidateProductionPrivacySidecarBinding(
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
	sidecar ProductionPrivacySidecar,
	sidecarSHA256 string,
	expectedSidecarSHA256 string,
) error {
	if err := validateSHA256(
		"production privacy sidecar sha256",
		sidecarSHA256,
	); err != nil {
		return err
	}
	if err := validateSHA256(
		"expected production privacy sidecar sha256",
		expectedSidecarSHA256,
	); err != nil {
		return err
	}
	if expectedSidecarSHA256 != sidecarSHA256 {
		return fmt.Errorf(
			"production privacy sidecar bytes do not match the bound manifest",
		)
	}
	if err := validateProductionPrivacySidecarStructure(sidecar); err != nil {
		return err
	}
	canonical, err := NewCorpus(candidates.Records)
	if err != nil {
		return fmt.Errorf("production privacy sidecar candidates: %w", err)
	}
	if sidecar.PlanID != plan.PlanID ||
		sidecar.PlanSHA256 != planSHA256 ||
		sidecar.CandidateCorpusSHA256 != canonical.SHA256 {
		return fmt.Errorf(
			"production privacy sidecar does not match the exact plan and candidate corpus",
		)
	}
	if len(sidecar.Entries) != len(canonical.Records) {
		return fmt.Errorf(
			"production privacy sidecar has %d entries, want %d",
			len(sidecar.Entries),
			len(canonical.Records),
		)
	}
	records := make(map[string]CorpusRecord, len(canonical.Records))
	for _, record := range canonical.Records {
		records[record.CaseID] = record
	}
	for i, entry := range sidecar.Entries {
		record, ok := records[entry.CaseID]
		if !ok || record.Task != entry.Task {
			return fmt.Errorf(
				"production privacy sidecar entry %d does not match a candidate case",
				i+1,
			)
		}
		delete(records, entry.CaseID)
	}
	if len(records) != 0 {
		return fmt.Errorf(
			"production privacy sidecar does not cover every candidate case",
		)
	}
	return nil
}

func validateProductionPrivacySidecarStructure(
	sidecar ProductionPrivacySidecar,
) error {
	if sidecar.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"production privacy sidecar schema_version got %d, want %d",
			sidecar.SchemaVersion,
			SchemaVersion,
		)
	}
	if sidecar.Status != ProductionPrivacySidecarStatus {
		return fmt.Errorf(
			"production privacy sidecar status got %q, want %q",
			sidecar.Status,
			ProductionPrivacySidecarStatus,
		)
	}
	if sidecar.Scope != ProductionPrivacySidecarScope {
		return fmt.Errorf("production privacy sidecar scope is not recognized")
	}
	if err := validateIdentifier(
		"production privacy sidecar.plan_id",
		sidecar.PlanID,
	); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"plan_sha256":             sidecar.PlanSHA256,
		"candidate_corpus_sha256": sidecar.CandidateCorpusSHA256,
	} {
		if err := validateSHA256(
			"production privacy sidecar."+name,
			value,
		); err != nil {
			return err
		}
	}
	if len(sidecar.Entries) == 0 {
		return fmt.Errorf("production privacy sidecar entries are empty")
	}
	seen := make(map[string]struct{}, len(sidecar.Entries))
	previousTask := Task("")
	previousCaseID := ""
	for i, entry := range sidecar.Entries {
		if err := validateIdentifier(
			fmt.Sprintf("production privacy sidecar.entries[%d].case_id", i),
			entry.CaseID,
		); err != nil {
			return err
		}
		if !containsTask(orderedTasks, entry.Task) {
			return fmt.Errorf(
				"production privacy sidecar entry %d has unsupported task %q",
				i+1,
				entry.Task,
			)
		}
		decoded, err := hex.DecodeString(entry.InfoHashHex)
		if err != nil ||
			len(decoded) != 20 ||
			entry.InfoHashHex != strings.ToLower(entry.InfoHashHex) {
			return fmt.Errorf(
				"production privacy sidecar entry %d has an invalid infohash",
				i+1,
			)
		}
		if _, duplicate := seen[entry.CaseID]; duplicate {
			return fmt.Errorf(
				"production privacy sidecar contains duplicate case IDs",
			)
		}
		seen[entry.CaseID] = struct{}{}
		if i > 0 &&
			(entry.Task < previousTask ||
				(entry.Task == previousTask &&
					entry.CaseID <= previousCaseID)) {
			return fmt.Errorf(
				"production privacy sidecar entries are not in canonical task/case order",
			)
		}
		previousTask = entry.Task
		previousCaseID = entry.CaseID
	}
	return nil
}

// ReadProductionCandidateExportManifest strictly decodes and hashes the exact
// exporter manifest bytes. The byte hash is carried into the freeze manifest,
// so a later stage cannot silently substitute a different source proof.
func ReadProductionCandidateExportManifest(
	r io.Reader,
) (ProductionCandidateExportManifest, string, error) {
	if r == nil {
		return ProductionCandidateExportManifest{}, "", fmt.Errorf(
			"production export manifest reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(
		r,
		maxProductionCandidateExportManifestBytes+1,
	))
	if err != nil {
		return ProductionCandidateExportManifest{}, "", fmt.Errorf(
			"read production export manifest: %w",
			err,
		)
	}
	if len(raw) == 0 {
		return ProductionCandidateExportManifest{}, "", fmt.Errorf(
			"production export manifest is empty",
		)
	}
	if len(raw) > maxProductionCandidateExportManifestBytes {
		return ProductionCandidateExportManifest{}, "", fmt.Errorf(
			"production export manifest exceeds the %d-byte limit",
			maxProductionCandidateExportManifestBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return ProductionCandidateExportManifest{}, "", fmt.Errorf(
			"decode production export manifest: %w",
			err,
		)
	}
	var manifest ProductionCandidateExportManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ProductionCandidateExportManifest{}, "", fmt.Errorf(
			"decode production export manifest: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ProductionCandidateExportManifest{}, "", fmt.Errorf(
				"decode production export manifest: multiple JSON values",
			)
		}
		return ProductionCandidateExportManifest{}, "", fmt.Errorf(
			"decode production export manifest: trailing data: %w",
			err,
		)
	}
	return manifest, sha256Hex(raw), nil
}

// ValidateProductionCandidateExportManifest binds an exporter manifest to the
// exact plan and candidate corpus. It replays the deterministic split preview
// and checks every per-task source snapshot before a freeze may claim verified
// production provenance.
func ValidateProductionCandidateExportManifest(
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
	manifest ProductionCandidateExportManifest,
) error {
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("production export plan: %w", err)
	}
	if err := validateSHA256("plan_sha256", planSHA256); err != nil {
		return err
	}
	canonical, err := NewCorpus(candidates.Records)
	if err != nil {
		return fmt.Errorf("production export candidates: %w", err)
	}
	if candidates.SHA256 == "" || candidates.SHA256 != canonical.SHA256 {
		return fmt.Errorf(
			"production export candidates: supplied SHA-256 does not match canonical records",
		)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"production export manifest schema_version got %d, want %d",
			manifest.SchemaVersion,
			SchemaVersion,
		)
	}
	if manifest.Status != ProductionCandidateExportStatus {
		return fmt.Errorf(
			"production export manifest status got %q, want %q",
			manifest.Status,
			ProductionCandidateExportStatus,
		)
	}
	if manifest.Scope != ProductionCandidateExportScope {
		return fmt.Errorf("production export manifest scope is not recognized")
	}
	switch manifest.ExporterID {
	case ProductionCandidateExporterID:
		if manifest.SamplingOrigin != ProductionDiagnosticSamplingOrigin {
			return fmt.Errorf(
				"diagnostic production export sampling_origin got %q, want %q",
				manifest.SamplingOrigin,
				ProductionDiagnosticSamplingOrigin,
			)
		}
	case CaptureLedgerProductionCandidateExporterID:
		if manifest.SamplingOrigin != ProductionCaptureSamplingOrigin {
			return fmt.Errorf(
				"capture-ledger production export sampling_origin got %q, want %q",
				manifest.SamplingOrigin,
				ProductionCaptureSamplingOrigin,
			)
		}
	default:
		return fmt.Errorf(
			"production export manifest exporter_id %q is not recognized",
			manifest.ExporterID,
		)
	}
	if manifest.PlanID != plan.PlanID || manifest.PlanSHA256 != planSHA256 {
		return fmt.Errorf("production export manifest does not match the exact plan")
	}
	if err := validateUTC(
		"production export manifest.observed_at_utc",
		manifest.ObservedAtUTC,
	); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"database_identity_sha256":    manifest.DatabaseIdentitySHA256,
		"transaction_snapshot_sha256": manifest.TransactionSnapshotSHA256,
		"query_bundle_sha256":         manifest.QueryBundleSHA256,
		"candidate_corpus_sha256":     manifest.CandidateCorpusSHA256,
		"privacy_sidecar_sha256":      manifest.PrivacySidecarSHA256,
		"development_preview_sha256":  manifest.DevelopmentPreviewSHA256,
		"holdout_preview_sha256":      manifest.HoldoutPreviewSHA256,
	} {
		if err := validateSHA256(
			"production export manifest."+name,
			value,
		); err != nil {
			return err
		}
	}
	if !manifest.ReadOnlyVerified ||
		!manifest.RepeatableReadVerified ||
		!manifest.NoModelCalls {
		return fmt.Errorf(
			"production export manifest did not verify read-only repeatable-read operation with model calls disabled",
		)
	}
	if manifest.PrivacyStatus != PrivacyVerifiedPostRulePublic {
		return fmt.Errorf(
			"production export manifest privacy_status got %q, want %q",
			manifest.PrivacyStatus,
			PrivacyVerifiedPostRulePublic,
		)
	}
	if manifest.LabelStatus != ProductionCandidateLabelStatus {
		return fmt.Errorf(
			"production export manifest label_status got %q, want %q",
			manifest.LabelStatus,
			ProductionCandidateLabelStatus,
		)
	}
	if !manifest.FreezeCompatibilityChecked {
		return fmt.Errorf(
			"production export manifest freeze_compatibility_checked must be true",
		)
	}
	if manifest.CandidateCorpusSHA256 != canonical.SHA256 ||
		manifest.CandidateCases != len(canonical.Records) {
		return fmt.Errorf(
			"production export manifest does not match the exact candidate corpus",
		)
	}
	for i, record := range canonical.Records {
		if record.Label.Provenance != LabelProvenanceSamplingCandidate ||
			record.Label.Strength != LabelStrengthUnreviewed ||
			record.Label.PolicyVersion != CandidatePlaceholderPolicyVersion {
			return fmt.Errorf(
				"production export candidate %d (%q) is not an unreviewed sampling placeholder",
				i+1,
				record.CaseID,
			)
		}
		if record.PrivacyAttestation == nil ||
			record.PrivacyAttestation.PlanID != plan.PlanID ||
			record.PrivacyAttestation.Status != PrivacyVerifiedPostRulePublic {
			return fmt.Errorf(
				"production export candidate %d (%q) is not bound to the verified plan privacy gate",
				i+1,
				record.CaseID,
			)
		}
	}

	preview, err := FreezeCandidateCorpus(plan, planSHA256, canonical)
	if err != nil {
		return fmt.Errorf("production export freeze replay: %w", err)
	}
	if manifest.DevelopmentPreviewSHA256 != preview.Development.SHA256 ||
		manifest.HoldoutPreviewSHA256 != preview.Holdout.SHA256 {
		return fmt.Errorf(
			"production export manifest split previews do not match the exact candidate corpus",
		)
	}
	if len(manifest.Tasks) != len(preview.Manifest.Tasks) {
		return fmt.Errorf(
			"production export manifest has %d tasks, want %d",
			len(manifest.Tasks),
			len(preview.Manifest.Tasks),
		)
	}
	for i, summary := range preview.Manifest.Tasks {
		task := manifest.Tasks[i]
		if task.Task != summary.Task {
			return fmt.Errorf(
				"production export manifest task %d got %q, want %q",
				i+1,
				task.Task,
				summary.Task,
			)
		}
		for name, value := range map[string]string{
			"query_sha256":           task.QuerySHA256,
			"source_snapshot_sha256": task.SourceSnapshotSHA256,
		} {
			if err := validateSHA256(
				fmt.Sprintf(
					"production export manifest task %q.%s",
					task.Task,
					name,
				),
				value,
			); err != nil {
				return err
			}
		}
		if task.SourceSnapshotSHA256 != summary.SourceSnapshotSHA256 ||
			task.CandidateCases != summary.CandidateCases ||
			task.CandidateGroups != summary.CandidateGroups {
			return fmt.Errorf(
				"production export manifest task %q does not match candidate records",
				task.Task,
			)
		}
		if err := validateProductionCohortCounts(
			task.Task,
			task.CohortCounts,
			task.CandidateCases,
		); err != nil {
			return err
		}
		if len(task.Limitations) != 0 {
			if err := validateTextList(
				fmt.Sprintf(
					"production export manifest task %q limitations",
					task.Task,
				),
				task.Limitations,
				1,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateProductionCohortCounts(
	task Task,
	counts ProductionSourceCohortCounts,
	candidateCases int,
) error {
	fields := map[string]int64{
		"eligible_before_privacy":      counts.EligibleBeforePrivacy,
		"native_private_excluded":      counts.NativePrivateExcluded,
		"private_evidence_excluded":    counts.PrivateEvidenceExcluded,
		"privacy_safe_available":       counts.PrivacySafeAvailable,
		"source_rows_read":             counts.SourceRowsRead,
		"deduplicated_candidate_cases": counts.DeduplicatedCandidateCases,
	}
	for name, value := range fields {
		if value < 0 {
			return fmt.Errorf(
				"production export task %q cohort_counts.%s is negative",
				task,
				name,
			)
		}
	}
	if counts.EligibleBeforePrivacy == 0 ||
		counts.PrivacySafeAvailable == 0 ||
		counts.SourceRowsRead == 0 {
		return fmt.Errorf(
			"production export task %q cohort counts must attest a non-empty source",
			task,
		)
	}
	if counts.NativePrivateExcluded > counts.EligibleBeforePrivacy ||
		counts.PrivateEvidenceExcluded > counts.EligibleBeforePrivacy ||
		counts.PrivacySafeAvailable > counts.EligibleBeforePrivacy {
		return fmt.Errorf(
			"production export task %q privacy counts exceed the eligible cohort",
			task,
		)
	}
	if counts.SourceRowsRead > counts.PrivacySafeAvailable {
		return fmt.Errorf(
			"production export task %q rows read exceed privacy-safe availability",
			task,
		)
	}
	if counts.DeduplicatedCandidateCases != int64(candidateCases) {
		return fmt.Errorf(
			"production export task %q candidate count does not match cohort accounting",
			task,
		)
	}
	if len(counts.RowsByStratum) == 0 {
		return fmt.Errorf(
			"production export task %q rows_by_stratum is empty",
			task,
		)
	}
	var rowsByStratum int64
	for stratum, count := range counts.RowsByStratum {
		if err := validateIdentifier("cohort stratum", stratum); err != nil {
			return fmt.Errorf("production export task %q: %w", task, err)
		}
		if count <= 0 {
			return fmt.Errorf(
				"production export task %q stratum %q count must be positive",
				task,
				stratum,
			)
		}
		rowsByStratum += count
	}
	if rowsByStratum != counts.SourceRowsRead {
		return fmt.Errorf(
			"production export task %q rows_by_stratum does not match source_rows_read",
			task,
		)
	}
	return nil
}

func cloneProductionCohortCounts(
	in ProductionSourceCohortCounts,
) ProductionSourceCohortCounts {
	out := in
	if in.RowsByStratum != nil {
		out.RowsByStratum = make(map[string]int64, len(in.RowsByStratum))
		for key, value := range in.RowsByStratum {
			out.RowsByStratum[key] = value
		}
	}
	return out
}

func validateProductionSourceSnapshot(
	plan CorpusPlan,
	snapshot ProductionSourceSnapshot,
) error {
	if err := validateUTC("source snapshot observed_at_utc", snapshot.ObservedAtUTC); err != nil {
		return err
	}
	observed, _ := time.Parse(time.RFC3339Nano, snapshot.ObservedAtUTC)
	if !strings.HasSuffix(snapshot.ObservedAtUTC, "Z") ||
		observed.IsZero() {
		return fmt.Errorf("source snapshot observed_at_utc must be UTC")
	}
	for field, value := range map[string]string{
		"database_identity_sha256":    snapshot.DatabaseIdentitySHA256,
		"transaction_snapshot_sha256": snapshot.TransactionSnapshotSHA256,
		"query_bundle_sha256":         snapshot.QueryBundleSHA256,
	} {
		if err := validateSHA256(field, value); err != nil {
			return err
		}
	}
	if snapshot.UnifiedSourceSnapshotSHA256 != "" {
		if err := validateSHA256(
			"unified_source_snapshot_sha256",
			snapshot.UnifiedSourceSnapshotSHA256,
		); err != nil {
			return err
		}
	}
	if !snapshot.ReadOnlyVerified {
		return fmt.Errorf("source snapshot did not verify a read-only transaction")
	}
	if !snapshot.RepeatableReadVerified {
		return fmt.Errorf("source snapshot did not verify repeatable-read isolation")
	}
	if !snapshot.NoModelCalls {
		return fmt.Errorf("source snapshot does not attest that model calls were disabled")
	}
	if len(snapshot.Seeds) == 0 {
		return fmt.Errorf("source snapshot has no candidate seeds")
	}
	for _, taskPlan := range plan.Tasks {
		querySHA := snapshot.TaskQuerySHA256[taskPlan.Task]
		if err := validateSHA256(
			fmt.Sprintf("task query SHA-256 for %q", taskPlan.Task),
			querySHA,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s ProductionCandidateSeed) validate(taskPlan CorpusPlanTask) error {
	decodedInfoHash, err := hex.DecodeString(s.SourceKey)
	if err != nil ||
		len(decodedInfoHash) != 20 ||
		s.SourceKey != strings.ToLower(s.SourceKey) {
		return fmt.Errorf(
			"source key must be a lower-case 40-hex infohash",
		)
	}
	if strings.TrimSpace(s.GroupKey) == "" {
		return fmt.Errorf("group key is empty")
	}
	if s.CandidateKey != "" {
		decodedCandidateKey, err := hex.DecodeString(s.CandidateKey)
		if err != nil ||
			len(decodedCandidateKey) != 32 ||
			s.CandidateKey != strings.ToLower(s.CandidateKey) {
			return fmt.Errorf(
				"candidate key must be a lower-case 64-hex capture identity",
			)
		}
	}
	if strings.ContainsRune(s.SourceKey, '\x00') ||
		strings.ContainsRune(s.GroupKey, '\x00') {
		return fmt.Errorf("source and group keys cannot contain NUL")
	}
	if !s.PrivacySafe {
		return fmt.Errorf("source privacy gate was not verified")
	}
	if _, ok := taskPlan.HoldoutAllocation[s.PrimarySuite]; !ok {
		return fmt.Errorf(
			"primary suite %q is not a plan allocation for %q",
			s.PrimarySuite,
			s.Task,
		)
	}
	if err := validateIdentifier("stratum", s.Stratum); err != nil {
		return err
	}
	for i, sliceID := range s.SliceIDs {
		if err := validateIdentifier(
			fmt.Sprintf("slice_ids[%d]", i),
			sliceID,
		); err != nil {
			return err
		}
		if strings.HasPrefix(
			sliceID,
			CorpusPrimarySuiteSlicePrefix,
		) {
			return fmt.Errorf(
				"source slices cannot set the reserved primary-suite prefix",
			)
		}
	}
	payloads := boolInt(s.MatcherExtract != nil) +
		boolInt(s.MatcherRerank != nil) +
		boolInt(s.ContentFilter != nil) +
		boolInt(s.JunkPurge != nil)
	if payloads != 1 {
		return fmt.Errorf("exactly one task input is required")
	}
	switch s.Task {
	case TaskMatcherExtract:
		if s.MatcherExtract == nil {
			return fmt.Errorf("matcher_extract input is required")
		}
		test := MatcherExtractCase{
			Input: *s.MatcherExtract,
			Expected: MatcherExtractExpected{
				AllowAbstain: true,
			},
		}
		return test.Validate()
	case TaskMatcherRerank:
		if s.MatcherRerank == nil {
			return fmt.Errorf("matcher_rerank input is required")
		}
		test := MatcherRerankCase{
			Input: *s.MatcherRerank,
			Expected: MatcherRerankExpected{
				AllowAbstain: true,
			},
		}
		return test.Validate()
	case TaskContentFilter:
		if s.ContentFilter == nil {
			return fmt.Errorf("contentfilter input is required")
		}
		test := ContentFilterCase{
			Input: *s.ContentFilter,
			Expected: ContentFilterExpected{
				AllowAbstain: true,
			},
		}
		return test.Validate()
	case TaskJunkPurge:
		if s.JunkPurge == nil {
			return fmt.Errorf("junkpurge input is required")
		}
		test := JunkPurgeCase{
			Input: *s.JunkPurge,
			Expected: JunkPurgeExpected{
				Disposition:       JunkDispositionAbstain,
				ContentClass:      JunkClassAmbiguous,
				DispositionPolicy: JunkDispositionPolicyV1,
			},
		}
		return test.Validate(GoldTierSamplingCandidate)
	default:
		return fmt.Errorf("unsupported task %q", s.Task)
	}
}

func productionCandidateRecord(
	plan CorpusPlan,
	sourceSHA256 string,
	seed ProductionCandidateSeed,
) (CorpusRecord, error) {
	caseHash := hashReviewFields(
		"llmeval-production-case-v1",
		plan.PlanID,
		string(seed.Task),
		seed.candidateIdentity(),
	)
	groupHash := hashReviewFields(
		"llmeval-production-group-v1",
		plan.PlanID,
		string(seed.Task),
		seed.GroupKey,
	)
	slices := append([]string(nil), seed.SliceIDs...)
	slices = append(
		slices,
		CorpusPrimarySuiteSlicePrefix+seed.PrimarySuite,
	)
	sort.Strings(slices)
	for i := 1; i < len(slices); i++ {
		if slices[i] == slices[i-1] {
			return CorpusRecord{}, fmt.Errorf(
				"source seed %q contains duplicate slice %q",
				caseHash,
				slices[i],
			)
		}
	}
	record := CorpusRecord{
		SchemaVersion: SchemaVersion,
		CaseID:        "production:" + string(seed.Task) + ":" + caseHash,
		Task:          seed.Task,
		SliceIDs:      slices,
		GroupID:       "production-group:" + groupHash,
		Label: LabelMetadata{
			Provenance:    LabelProvenanceSamplingCandidate,
			Strength:      LabelStrengthUnreviewed,
			SourceRef:     "candidate:" + seed.Stratum,
			PolicyVersion: CandidatePlaceholderPolicyVersion,
		},
		PrivacyAttestation: &SourcePrivacyAttestation{
			PlanID:               plan.PlanID,
			SourceSnapshotSHA256: sourceSHA256,
			Status:               PrivacyVerifiedPostRulePublic,
		},
	}
	switch seed.Task {
	case TaskMatcherExtract:
		input := *seed.MatcherExtract
		input.FilePaths = append([]string(nil), input.FilePaths...)
		record.MatcherExtract = &MatcherExtractCase{
			Input: input,
			Expected: MatcherExtractExpected{
				AllowAbstain: true,
			},
		}
	case TaskMatcherRerank:
		input := *seed.MatcherRerank
		input.Candidates = append(
			[]MatcherCandidate(nil),
			input.Candidates...,
		)
		record.MatcherRerank = &MatcherRerankCase{
			Input: input,
			Expected: MatcherRerankExpected{
				AllowAbstain: true,
			},
		}
	case TaskContentFilter:
		record.ContentFilter = &ContentFilterCase{
			Input: *seed.ContentFilter,
			Expected: ContentFilterExpected{
				AllowAbstain: true,
			},
		}
	case TaskJunkPurge:
		record.JunkPurge = &JunkPurgeCase{
			Input: *seed.JunkPurge,
			Expected: JunkPurgeExpected{
				Disposition:       JunkDispositionAbstain,
				ContentClass:      JunkClassAmbiguous,
				DispositionPolicy: JunkDispositionPolicyV1,
			},
		}
		record.Tier = GoldTierSamplingCandidate
	}
	return record, nil
}

func productionTaskSourceSHA256(
	planSHA256 string,
	snapshot ProductionSourceSnapshot,
	task Task,
	seeds []ProductionCandidateSeed,
) (string, error) {
	if snapshot.UnifiedSourceSnapshotSHA256 != "" {
		return snapshot.UnifiedSourceSnapshotSHA256, nil
	}
	type identitySeed struct {
		SourceKey      string               `json:"source_key_sha256"`
		CandidateKey   string               `json:"candidate_key"`
		GroupKey       string               `json:"group_key_sha256"`
		PrimarySuite   string               `json:"primary_suite"`
		SliceIDs       []string             `json:"slice_ids"`
		Stratum        string               `json:"stratum"`
		MatcherExtract *MatcherExtractInput `json:"matcher_extract,omitempty"`
		MatcherRerank  *MatcherRerankInput  `json:"matcher_rerank,omitempty"`
		ContentFilter  *ContentFilterInput  `json:"contentfilter,omitempty"`
		JunkPurge      *JunkPurgeInput      `json:"junkpurge,omitempty"`
	}
	identities := make([]identitySeed, 0, len(seeds))
	for _, seed := range seeds {
		slices := append([]string(nil), seed.SliceIDs...)
		sort.Strings(slices)
		identities = append(identities, identitySeed{
			SourceKey:      sha256Hex([]byte(seed.SourceKey)),
			CandidateKey:   seed.candidateIdentity(),
			GroupKey:       sha256Hex([]byte(seed.GroupKey)),
			PrimarySuite:   seed.PrimarySuite,
			SliceIDs:       slices,
			Stratum:        seed.Stratum,
			MatcherExtract: seed.MatcherExtract,
			MatcherRerank:  seed.MatcherRerank,
			ContentFilter:  seed.ContentFilter,
			JunkPurge:      seed.JunkPurge,
		})
	}
	raw, err := json.Marshal(identities)
	if err != nil {
		return "", fmt.Errorf("marshal %q source identity: %w", task, err)
	}
	var payload bytes.Buffer
	for _, field := range []string{
		"llmeval-production-source-snapshot-v1",
		planSHA256,
		snapshot.DatabaseIdentitySHA256,
		snapshot.TransactionSnapshotSHA256,
		snapshot.TaskQuerySHA256[task],
		string(task),
	} {
		payload.WriteString(field)
		payload.WriteByte(0)
	}
	payload.Write(raw)
	return sha256Hex(payload.Bytes()), nil
}

func (s ProductionCandidateSeed) candidateIdentity() string {
	if s.CandidateKey != "" {
		return s.CandidateKey
	}
	return s.SourceKey
}
