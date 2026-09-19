package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
)

const (
	CampaignRunCheckpointSchemaVersion = 1
	maxCampaignRunCheckpointBytes      = int64(1 << 30)
)

// CampaignRunCheckpoint is a private, crash-recovery transaction record. It
// is written before either public run artifact is replaced, so a resumed
// campaign can deterministically finish the results/evidence pair without
// repeating a provider request.
type CampaignRunCheckpoint struct {
	SchemaVersion         int                      `json:"schema_version"`
	Campaign              CampaignRunBinding       `json:"campaign"`
	ResultsSHA256         string                   `json:"results_sha256"`
	AttemptEvidenceSHA256 string                   `json:"attempt_evidence_sha256"`
	Results               []ResultRecord           `json:"results"`
	AttemptEvidence       PromotionAttemptEvidence `json:"attempt_evidence"`
}

// CampaignResultsBinding returns the one exact campaign run binding carried
// by every result. Mixed campaign/non-campaign rows and mixed run identities
// fail closed even when their ordinary system/corpus fields happen to match.
func CampaignResultsBinding(
	results []ResultRecord,
) (CampaignRunBinding, error) {
	if len(results) == 0 {
		return CampaignRunBinding{}, fmt.Errorf("campaign results are empty")
	}
	if err := ValidateResults(results); err != nil {
		return CampaignRunBinding{}, fmt.Errorf("campaign results: %w", err)
	}
	first := results[0].ExecutionAudit.Campaign
	if first == nil {
		return CampaignRunBinding{}, fmt.Errorf(
			"campaign results are missing a campaign run binding",
		)
	}
	for i := range results {
		current := results[i].ExecutionAudit.Campaign
		if current == nil {
			return CampaignRunBinding{}, fmt.Errorf(
				"campaign result %d is missing a campaign run binding",
				i+1,
			)
		}
		if !reflect.DeepEqual(*current, *first) {
			return CampaignRunBinding{}, fmt.Errorf(
				"campaign result %d belongs to a different campaign run binding",
				i+1,
			)
		}
	}
	return *cloneCampaignRunBinding(first), nil
}

// ValidateCampaignResultsBinding authenticates result rows against a binding
// freshly derived from the exact campaign-plan bytes.
func ValidateCampaignResultsBinding(
	results []ResultRecord,
	expected CampaignRunBinding,
) error {
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("expected campaign run binding: %w", err)
	}
	actual, err := CampaignResultsBinding(results)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf(
			"results do not match the exact preregistered campaign run binding",
		)
	}
	return nil
}

// BuildCampaignAttemptEvidence converts the synchronous runner's factual
// one-call-per-case result set into promotion-v2 attempt evidence. A schema
// error remains a single failed initial attempt; it is deliberately not
// invented into a repair and will therefore fail the later promotion gate.
func BuildCampaignAttemptEvidence(
	results []ResultRecord,
) (PromotionAttemptEvidence, error) {
	binding, err := CampaignResultsBinding(results)
	if err != nil {
		return PromotionAttemptEvidence{}, err
	}
	resultsSHA256, err := ResultsIdentity(results)
	if err != nil {
		return PromotionAttemptEvidence{}, fmt.Errorf(
			"campaign results identity: %w",
			err,
		)
	}
	canonical := canonicalResults(results)
	evidence := PromotionAttemptEvidence{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: PromotionBundleProtocolVersion,
		SystemID:        binding.SystemID,
		Task:            binding.Task,
		CorpusSHA256:    binding.Corpus.SHA256,
		ResultsSHA256:   resultsSHA256,
		Attempts:        make([]PromotionAttempt, 0, len(canonical)),
	}
	for _, result := range canonical {
		evidence.Attempts = append(evidence.Attempts, PromotionAttempt{
			CaseID:   result.CaseID,
			Sequence: 1,
			Kind:     PromotionAttemptInitial,
			Status:   result.Status,
			Usage:    result.Usage,
		})
	}
	return evidence, nil
}

// ValidateCampaignAttemptEvidence requires the exact deterministic evidence
// derivable from the bound results. It rejects omissions, injected retries,
// altered usage/status, and evidence copied from another run.
func ValidateCampaignAttemptEvidence(
	results []ResultRecord,
	evidence PromotionAttemptEvidence,
) error {
	expected, err := BuildCampaignAttemptEvidence(results)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(evidence, expected) {
		return fmt.Errorf(
			"attempt evidence does not exactly match the campaign results",
		)
	}
	return nil
}

// WriteCampaignAttemptEvidence emits the deterministic JSON representation
// used for artifact hashing and crash recovery.
func WriteCampaignAttemptEvidence(
	w io.Writer,
	evidence PromotionAttemptEvidence,
) error {
	if w == nil {
		return fmt.Errorf("campaign attempt evidence writer is nil")
	}
	payload, err := campaignJSONBytes(evidence)
	if err != nil {
		return fmt.Errorf("encode campaign attempt evidence: %w", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("write campaign attempt evidence: %w", err)
	}
	return nil
}

// CampaignAttemptEvidenceIdentity returns the digest of the exact bytes
// emitted by WriteCampaignAttemptEvidence.
func CampaignAttemptEvidenceIdentity(
	evidence PromotionAttemptEvidence,
) (string, error) {
	payload, err := campaignJSONBytes(evidence)
	if err != nil {
		return "", fmt.Errorf("encode campaign attempt evidence: %w", err)
	}
	return sha256Hex(payload), nil
}

// NewCampaignRunCheckpoint builds and fully validates one recovery record.
func NewCampaignRunCheckpoint(
	expected CampaignRunBinding,
	results []ResultRecord,
) (CampaignRunCheckpoint, error) {
	if err := ValidateCampaignResultsBinding(results, expected); err != nil {
		return CampaignRunCheckpoint{}, err
	}
	evidence, err := BuildCampaignAttemptEvidence(results)
	if err != nil {
		return CampaignRunCheckpoint{}, err
	}
	resultsSHA256, err := ResultsIdentity(results)
	if err != nil {
		return CampaignRunCheckpoint{}, err
	}
	evidenceSHA256, err := CampaignAttemptEvidenceIdentity(evidence)
	if err != nil {
		return CampaignRunCheckpoint{}, err
	}
	checkpoint := CampaignRunCheckpoint{
		SchemaVersion:         CampaignRunCheckpointSchemaVersion,
		Campaign:              *cloneCampaignRunBinding(&expected),
		ResultsSHA256:         resultsSHA256,
		AttemptEvidenceSHA256: evidenceSHA256,
		Results:               canonicalResults(results),
		AttemptEvidence:       evidence,
	}
	if err := checkpoint.Validate(); err != nil {
		return CampaignRunCheckpoint{}, err
	}
	return checkpoint, nil
}

func (checkpoint CampaignRunCheckpoint) Validate() error {
	if checkpoint.SchemaVersion != CampaignRunCheckpointSchemaVersion {
		return fmt.Errorf(
			"campaign run checkpoint schema_version: got %d, want %d",
			checkpoint.SchemaVersion,
			CampaignRunCheckpointSchemaVersion,
		)
	}
	if err := ValidateCampaignResultsBinding(
		checkpoint.Results,
		checkpoint.Campaign,
	); err != nil {
		return fmt.Errorf("campaign run checkpoint: %w", err)
	}
	resultsSHA256, err := ResultsIdentity(checkpoint.Results)
	if err != nil {
		return fmt.Errorf("campaign run checkpoint results: %w", err)
	}
	if checkpoint.ResultsSHA256 != resultsSHA256 {
		return fmt.Errorf("campaign run checkpoint results hash mismatch")
	}
	if err := ValidateCampaignAttemptEvidence(
		checkpoint.Results,
		checkpoint.AttemptEvidence,
	); err != nil {
		return fmt.Errorf("campaign run checkpoint: %w", err)
	}
	evidenceSHA256, err := CampaignAttemptEvidenceIdentity(
		checkpoint.AttemptEvidence,
	)
	if err != nil {
		return err
	}
	if checkpoint.AttemptEvidenceSHA256 != evidenceSHA256 {
		return fmt.Errorf(
			"campaign run checkpoint attempt evidence hash mismatch",
		)
	}
	return nil
}

func WriteCampaignRunCheckpoint(
	w io.Writer,
	checkpoint CampaignRunCheckpoint,
) error {
	if w == nil {
		return fmt.Errorf("campaign run checkpoint writer is nil")
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	payload, err := campaignCompactJSONBytes(checkpoint)
	if err != nil {
		return fmt.Errorf("encode campaign run checkpoint: %w", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("write campaign run checkpoint: %w", err)
	}
	return nil
}

func ReadCampaignRunCheckpoint(
	r io.Reader,
) (CampaignRunCheckpoint, error) {
	if r == nil {
		return CampaignRunCheckpoint{}, fmt.Errorf(
			"campaign run checkpoint reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(
		r,
		maxCampaignRunCheckpointBytes+1,
	))
	if err != nil {
		return CampaignRunCheckpoint{}, fmt.Errorf(
			"read campaign run checkpoint: %w",
			err,
		)
	}
	if len(raw) == 0 {
		return CampaignRunCheckpoint{}, fmt.Errorf(
			"campaign run checkpoint is empty",
		)
	}
	if int64(len(raw)) > maxCampaignRunCheckpointBytes {
		return CampaignRunCheckpoint{}, fmt.Errorf(
			"campaign run checkpoint exceeds %d bytes",
			maxCampaignRunCheckpointBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return CampaignRunCheckpoint{}, fmt.Errorf(
			"decode campaign run checkpoint: %w",
			err,
		)
	}
	var checkpoint CampaignRunCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return CampaignRunCheckpoint{}, fmt.Errorf(
			"decode campaign run checkpoint: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return CampaignRunCheckpoint{}, fmt.Errorf(
				"decode campaign run checkpoint: multiple JSON values",
			)
		}
		return CampaignRunCheckpoint{}, fmt.Errorf(
			"decode campaign run checkpoint: trailing data: %w",
			err,
		)
	}
	if err := checkpoint.Validate(); err != nil {
		return CampaignRunCheckpoint{}, err
	}
	return checkpoint, nil
}

func campaignJSONBytes(value any) ([]byte, error) {
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return payload.Bytes(), nil
}

func campaignCompactJSONBytes(value any) ([]byte, error) {
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return payload.Bytes(), nil
}
