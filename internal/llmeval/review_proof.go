package llmeval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

const maxReviewProofArtifactBytes = 4 << 20

// ReviewWorkflow keeps the two deliberately different human-label protocols
// from accepting one another's policy or evidence artifacts.
type ReviewWorkflow string

const (
	ReviewWorkflowContentJunk ReviewWorkflow = "content_junk"
	ReviewWorkflowMatcher     ReviewWorkflow = "matcher"
)

// ReviewPolicyArtifact is the checked-in, immutable decision policy. Its
// exact file bytes are hashed into every downstream review artifact.
type ReviewPolicyArtifact struct {
	SchemaVersion int                `json:"schema_version"`
	PolicyID      string             `json:"policy_id"`
	PolicyVersion string             `json:"policy_version"`
	Workflow      ReviewWorkflow     `json:"workflow"`
	TaskPolicies  []ReviewTaskPolicy `json:"task_policies"`
}

// ReviewTaskPolicy enumerates the allowed decision states and the rules a
// reviewer must apply. Rules are bounded policy text, never case evidence.
type ReviewTaskPolicy struct {
	Task      Task     `json:"task"`
	Decisions []string `json:"decisions"`
	Rules     []string `json:"rules"`
}

// ReviewEvidenceManifest is a local-only manifest of the evidence available
// for each source case. Evidence content and paths never enter assignments;
// only this artifact's hash and its source snapshot identity flow downstream.
type ReviewEvidenceManifest struct {
	SchemaVersion        int                  `json:"schema_version"`
	ManifestID           string               `json:"manifest_id"`
	Workflow             ReviewWorkflow       `json:"workflow"`
	CorpusSHA256         string               `json:"corpus_sha256"`
	SourceSnapshotSHA256 string               `json:"source_snapshot_sha256"`
	Items                []ReviewEvidenceItem `json:"items"`
}

// ReviewEvidenceItem binds a corpus case to one opaque local evidence
// reference and the exact evidence bytes. EvidenceRef must not be a path, URL,
// free-form note, credential, or raw production identifier.
type ReviewEvidenceItem struct {
	CaseID                  string `json:"case_id"`
	EvidenceRef             string `json:"evidence_ref"`
	EvidenceSHA256          string `json:"evidence_sha256"`
	TaskInputSHA256         string `json:"task_input_sha256,omitempty"`
	CanonicalEvidenceKind   string `json:"canonical_evidence_kind,omitempty"`
	CanonicalEvidenceSHA256 string `json:"canonical_evidence_sha256,omitempty"`
}

// HumanReviewProof is the compact immutable proof carried through assignment,
// response, agreement, adjudication, and promoted human-review label metadata.
type HumanReviewProof struct {
	Workflow               ReviewWorkflow `json:"workflow"`
	PolicyID               string         `json:"policy_id"`
	PolicyVersion          string         `json:"policy_version"`
	PolicySHA256           string         `json:"policy_sha256"`
	EvidenceManifestID     string         `json:"evidence_manifest_id"`
	EvidenceManifestSHA256 string         `json:"evidence_manifest_sha256"`
	EvidenceCorpusSHA256   string         `json:"evidence_corpus_sha256"`
	SourceSnapshotSHA256   string         `json:"source_snapshot_sha256"`
}

// LoadReviewPolicyArtifact strictly parses one policy and returns the SHA-256
// of its exact bytes. Unknown fields, duplicate keys, and trailing JSON fail.
func LoadReviewPolicyArtifact(r io.Reader) (ReviewPolicyArtifact, string, error) {
	var policy ReviewPolicyArtifact
	raw, err := readStrictReviewArtifact(r, "review policy")
	if err != nil {
		return policy, "", err
	}
	if err := decodeStrictReviewArtifact(raw, &policy, "review policy"); err != nil {
		return ReviewPolicyArtifact{}, "", err
	}
	if err := policy.Validate(); err != nil {
		return ReviewPolicyArtifact{}, "", fmt.Errorf("review policy: %w", err)
	}
	return policy, sha256Hex(raw), nil
}

// LoadReviewEvidenceManifest strictly parses a local evidence manifest and
// returns the SHA-256 of its exact bytes.
func LoadReviewEvidenceManifest(
	r io.Reader,
) (ReviewEvidenceManifest, string, error) {
	var manifest ReviewEvidenceManifest
	raw, err := readStrictReviewArtifact(r, "review evidence manifest")
	if err != nil {
		return manifest, "", err
	}
	if err := decodeStrictReviewArtifact(raw, &manifest, "review evidence manifest"); err != nil {
		return ReviewEvidenceManifest{}, "", err
	}
	if err := manifest.Validate(); err != nil {
		return ReviewEvidenceManifest{}, "", fmt.Errorf(
			"review evidence manifest: %w",
			err,
		)
	}
	return manifest, sha256Hex(raw), nil
}

// LoadHumanReviewProof loads and validates both immutable artifacts against
// the exact source corpus, selected tasks, and expected workflow.
func LoadHumanReviewProof(
	policyReader io.Reader,
	evidenceReader io.Reader,
	source Corpus,
	tasks []Task,
	workflow ReviewWorkflow,
) (HumanReviewProof, error) {
	policy, policySHA, err := LoadReviewPolicyArtifact(policyReader)
	if err != nil {
		return HumanReviewProof{}, err
	}
	manifest, manifestSHA, err := LoadReviewEvidenceManifest(evidenceReader)
	if err != nil {
		return HumanReviewProof{}, err
	}
	return buildHumanReviewProof(
		policy,
		policySHA,
		manifest,
		manifestSHA,
		source,
		tasks,
		workflow,
	)
}

// VerifyHumanReviewProof re-loads local artifacts and proves that their exact
// bytes are the ones bound into an existing downstream artifact.
func VerifyHumanReviewProof(
	expected HumanReviewProof,
	policyReader io.Reader,
	evidenceReader io.Reader,
	source Corpus,
	tasks []Task,
	workflow ReviewWorkflow,
) error {
	actual, err := LoadHumanReviewProof(
		policyReader,
		evidenceReader,
		source,
		tasks,
		workflow,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf(
			"human review proof: local policy or evidence manifest does not match bound proof",
		)
	}
	return nil
}

// Validate checks the compact proof independent of local artifact access.
func (p HumanReviewProof) Validate() error {
	if p.Workflow != ReviewWorkflowContentJunk && p.Workflow != ReviewWorkflowMatcher {
		return fmt.Errorf("workflow: unsupported value %q", p.Workflow)
	}
	if err := validateIdentifier("policy_id", p.PolicyID); err != nil {
		return err
	}
	if err := validateTag("policy_version", p.PolicyVersion); err != nil {
		return err
	}
	if err := validateSHA256("policy_sha256", p.PolicySHA256); err != nil {
		return err
	}
	if err := validateIdentifier("evidence_manifest_id", p.EvidenceManifestID); err != nil {
		return err
	}
	if err := validateSHA256(
		"evidence_manifest_sha256",
		p.EvidenceManifestSHA256,
	); err != nil {
		return err
	}
	if err := validateSHA256("evidence_corpus_sha256", p.EvidenceCorpusSHA256); err != nil {
		return err
	}
	if err := validateSHA256("source_snapshot_sha256", p.SourceSnapshotSHA256); err != nil {
		return err
	}
	return nil
}

// Validate enforces the exact task/decision vocabulary of the two v1 review
// workflows. A policy cannot silently add a decision that code does not score.
func (p ReviewPolicyArtifact) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"schema_version: got %d, want %d",
			p.SchemaVersion,
			SchemaVersion,
		)
	}
	if err := validateIdentifier("policy_id", p.PolicyID); err != nil {
		return err
	}
	if err := validateTag("policy_version", p.PolicyVersion); err != nil {
		return err
	}
	if p.Workflow != ReviewWorkflowContentJunk && p.Workflow != ReviewWorkflowMatcher {
		return fmt.Errorf("workflow: unsupported value %q", p.Workflow)
	}
	if len(p.TaskPolicies) == 0 {
		return fmt.Errorf("task_policies: at least one task policy is required")
	}
	previous := Task("")
	for i, taskPolicy := range p.TaskPolicies {
		if i > 0 && taskPolicy.Task <= previous {
			return fmt.Errorf("task_policies: must be sorted by task and unique")
		}
		previous = taskPolicy.Task
		expected, ok := expectedPolicyDecisions(p.Workflow, taskPolicy.Task)
		if !ok {
			return fmt.Errorf(
				"task_policies[%d].task: %q is invalid for workflow %q",
				i,
				taskPolicy.Task,
				p.Workflow,
			)
		}
		if !reflect.DeepEqual(taskPolicy.Decisions, expected) {
			return fmt.Errorf(
				"task_policies[%d].decisions: got %v, want %v",
				i,
				taskPolicy.Decisions,
				expected,
			)
		}
		if len(taskPolicy.Rules) == 0 {
			return fmt.Errorf("task_policies[%d].rules: at least one rule is required", i)
		}
		for j, rule := range taskPolicy.Rules {
			if err := validateText(
				fmt.Sprintf("task_policies[%d].rules[%d]", i, j),
				rule,
				maxTextLength,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks the local evidence manifest without exposing its contents
// to a reviewer packet.
func (m ReviewEvidenceManifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"schema_version: got %d, want %d",
			m.SchemaVersion,
			SchemaVersion,
		)
	}
	if err := validateIdentifier("manifest_id", m.ManifestID); err != nil {
		return err
	}
	if m.Workflow != ReviewWorkflowContentJunk && m.Workflow != ReviewWorkflowMatcher {
		return fmt.Errorf("workflow: unsupported value %q", m.Workflow)
	}
	if err := validateSHA256("corpus_sha256", m.CorpusSHA256); err != nil {
		return err
	}
	if err := validateSHA256("source_snapshot_sha256", m.SourceSnapshotSHA256); err != nil {
		return err
	}
	if len(m.Items) == 0 {
		return fmt.Errorf("items: at least one evidence item is required")
	}
	previous := ""
	for i, item := range m.Items {
		if err := validateIdentifier(fmt.Sprintf("items[%d].case_id", i), item.CaseID); err != nil {
			return err
		}
		if i > 0 && item.CaseID <= previous {
			return fmt.Errorf("items: must be sorted by case_id and unique")
		}
		previous = item.CaseID
		if err := validateOpaqueEvidenceRef(
			fmt.Sprintf("items[%d].evidence_ref", i),
			item.EvidenceRef,
		); err != nil {
			return err
		}
		if err := validateSHA256(
			fmt.Sprintf("items[%d].evidence_sha256", i),
			item.EvidenceSHA256,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateOpaqueEvidenceRef(name, value string) error {
	lower := strings.ToLower(value)
	if strings.ContainsAny(value, `/\`) ||
		strings.HasPrefix(lower, "http:") ||
		strings.HasPrefix(lower, "https:") ||
		strings.HasPrefix(lower, "file:") {
		return fmt.Errorf("%s: must be an opaque identifier, not a path or URL", name)
	}
	return validateIdentifier(name, value)
}

func buildHumanReviewProof(
	policy ReviewPolicyArtifact,
	policySHA string,
	manifest ReviewEvidenceManifest,
	manifestSHA string,
	source Corpus,
	tasks []Task,
	workflow ReviewWorkflow,
) (HumanReviewProof, error) {
	canonical, err := NewCorpus(source.Records)
	if err != nil {
		return HumanReviewProof{}, fmt.Errorf("review proof source: %w", err)
	}
	if source.SHA256 != canonical.SHA256 {
		return HumanReviewProof{}, fmt.Errorf(
			"review proof source: corpus_sha256 does not match canonical source",
		)
	}
	if workflow != ReviewWorkflowContentJunk && workflow != ReviewWorkflowMatcher {
		return HumanReviewProof{}, fmt.Errorf(
			"review proof: unsupported workflow %q",
			workflow,
		)
	}
	if policy.Workflow != workflow || manifest.Workflow != workflow {
		return HumanReviewProof{}, fmt.Errorf(
			"review proof: policy and evidence workflow must both be %q",
			workflow,
		)
	}
	if manifest.CorpusSHA256 != canonical.SHA256 {
		return HumanReviewProof{}, fmt.Errorf(
			"review proof: evidence corpus_sha256 does not match source",
		)
	}

	selected, err := canonicalReviewProofTasks(tasks, workflow)
	if err != nil {
		return HumanReviewProof{}, err
	}
	policyTasks := make(map[Task]struct{}, len(policy.TaskPolicies))
	for _, taskPolicy := range policy.TaskPolicies {
		policyTasks[taskPolicy.Task] = struct{}{}
	}
	for _, task := range selected {
		if _, ok := policyTasks[task]; !ok {
			return HumanReviewProof{}, fmt.Errorf(
				"review proof: policy does not cover selected task %q",
				task,
			)
		}
	}

	sourceByID := make(map[string]CorpusRecord, len(canonical.Records))
	for _, record := range canonical.Records {
		sourceByID[record.CaseID] = record
	}
	evidenceByID := make(map[string]ReviewEvidenceItem, len(manifest.Items))
	for _, item := range manifest.Items {
		if _, ok := sourceByID[item.CaseID]; !ok {
			return HumanReviewProof{}, fmt.Errorf(
				"review proof: evidence item %q is not in source corpus",
				item.CaseID,
			)
		}
		evidenceByID[item.CaseID] = item
	}
	selectedCases := 0
	for _, record := range canonical.Records {
		if !containsTask(selected, record.Task) {
			continue
		}
		selectedCases++
		if record.Label.Provenance != LabelProvenanceSynthetic &&
			(record.PrivacyAttestation == nil ||
				record.PrivacyAttestation.SourceSnapshotSHA256 !=
					manifest.SourceSnapshotSHA256) {
			return HumanReviewProof{}, fmt.Errorf(
				"review proof: selected source case %q privacy snapshot does not match evidence manifest",
				record.CaseID,
			)
		}
		item, ok := evidenceByID[record.CaseID]
		if !ok {
			return HumanReviewProof{}, fmt.Errorf(
				"review proof: selected source case %q lacks evidence",
				record.CaseID,
			)
		}
		if record.Label.Provenance != LabelProvenanceSynthetic {
			if err := validateSHA256(
				"review proof task_input_sha256",
				item.TaskInputSHA256,
			); err != nil {
				return HumanReviewProof{}, fmt.Errorf(
					"review proof: production case %q does not bind its exact task input: %w",
					record.CaseID,
					err,
				)
			}
			if err := validateSHA256(
				"review proof canonical_evidence_sha256",
				item.CanonicalEvidenceSHA256,
			); err != nil {
				return HumanReviewProof{}, fmt.Errorf(
					"review proof: production case %q does not bind canonical/TMDB evidence: %w",
					record.CaseID,
					err,
				)
			}
			switch item.CanonicalEvidenceKind {
			case "tmdb_snapshot",
				"catalog_snapshot",
				"policy_evidence_bundle":
			default:
				return HumanReviewProof{}, fmt.Errorf(
					"review proof: production case %q has unsupported canonical_evidence_kind %q",
					record.CaseID,
					item.CanonicalEvidenceKind,
				)
			}
			if item.EvidenceSHA256 != item.CanonicalEvidenceSHA256 {
				return HumanReviewProof{}, fmt.Errorf(
					"review proof: production case %q evidence_sha256 is not the canonical/TMDB evidence hash",
					record.CaseID,
				)
			}
		}
	}
	if selectedCases == 0 {
		return HumanReviewProof{}, fmt.Errorf(
			"review proof: no source cases match selected tasks",
		)
	}

	proof := HumanReviewProof{
		Workflow:               workflow,
		PolicyID:               policy.PolicyID,
		PolicyVersion:          policy.PolicyVersion,
		PolicySHA256:           policySHA,
		EvidenceManifestID:     manifest.ManifestID,
		EvidenceManifestSHA256: manifestSHA,
		EvidenceCorpusSHA256:   manifest.CorpusSHA256,
		SourceSnapshotSHA256:   manifest.SourceSnapshotSHA256,
	}
	if err := proof.Validate(); err != nil {
		return HumanReviewProof{}, err
	}
	return proof, nil
}

func canonicalReviewProofTasks(
	tasks []Task,
	workflow ReviewWorkflow,
) ([]Task, error) {
	if len(tasks) == 0 {
		return nil, fmt.Errorf("review proof tasks: at least one task is required")
	}
	out := append([]Task(nil), tasks...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	for i, task := range out {
		if _, ok := expectedPolicyDecisions(workflow, task); !ok {
			return nil, fmt.Errorf(
				"review proof tasks[%d]: task %q is invalid for workflow %q",
				i,
				task,
				workflow,
			)
		}
		if i > 0 && task == out[i-1] {
			return nil, fmt.Errorf(
				"review proof tasks[%d]: duplicate task %q",
				i,
				task,
			)
		}
	}
	return out, nil
}

func expectedPolicyDecisions(
	workflow ReviewWorkflow,
	task Task,
) ([]string, bool) {
	switch workflow {
	case ReviewWorkflowContentJunk:
		switch task {
		case TaskContentFilter:
			return []string{
				string(ReviewLabelAmbiguous),
				string(ReviewLabelEnglish),
				string(ReviewLabelNonEnglish),
				string(ReviewLabelUncertain),
			}, true
		case TaskJunkPurge:
			// The junk-purge decision set is the content-class vocabulary, not
			// the old provenance one. Sorted, because the caller compares
			// against the bound policy artifact's `decisions` list.
			labels := JunkReviewLabels()
			decisions := make([]string, 0, len(labels))
			for _, label := range labels {
				decisions = append(decisions, string(label))
			}
			sort.Strings(decisions)
			return decisions, true
		}
	case ReviewWorkflowMatcher:
		switch task {
		case TaskMatcherExtract, TaskMatcherRerank:
			return []string{
				"allow_abstain",
				"ambiguous",
				"expected_shaped_answer",
			}, true
		}
	}
	return nil, false
}

func reviewProofIdentity(proof HumanReviewProof) string {
	return hashReviewFields(
		"llmeval-human-review-proof-v1",
		string(proof.Workflow),
		proof.PolicyID,
		proof.PolicyVersion,
		proof.PolicySHA256,
		proof.EvidenceManifestID,
		proof.EvidenceManifestSHA256,
		proof.EvidenceCorpusSHA256,
		proof.SourceSnapshotSHA256,
	)
}

func readStrictReviewArtifact(r io.Reader, name string) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("%s reader is nil", name)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxReviewProofArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read: %w", name, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s is empty", name)
	}
	if len(raw) > maxReviewProofArtifactBytes {
		return nil, fmt.Errorf(
			"%s exceeds the %d-byte limit",
			name,
			maxReviewProofArtifactBytes,
		)
	}
	return raw, nil
}

func decodeStrictReviewArtifact(raw []byte, out any, name string) error {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s: multiple JSON values", name)
		}
		return fmt.Errorf("%s: trailing data: %w", name, err)
	}
	return nil
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
