package llmeval

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckedInReviewPoliciesAreStrictAndWorkflowExact(t *testing.T) {
	tests := []struct {
		path     string
		workflow ReviewWorkflow
		policyID string
		version  string
		sha256   string
		tasks    int
	}{
		{
			path:     "../../ops/llm-eval/review-policies/content-junk-review-v2.json",
			workflow: ReviewWorkflowContentJunk,
			policyID: "content-junk-review",
			version:  "content-junk-review-v2",
			sha256:   "9024a874d1ad14c7a8047e8bc8573d055cb77493736db56238d9ec3ae48e657f",
			tasks:    2,
		},
		{
			path:     "../../ops/llm-eval/review-policies/matcher-review-v1.json",
			workflow: ReviewWorkflowMatcher,
			policyID: "matcher-review",
			version:  "matcher-review-v1",
			sha256:   "0219d22db484108032f81f97a1d1fc0d6359292c1457fdc82ff2b616def531d1",
			tasks:    2,
		},
	}
	for _, test := range tests {
		t.Run(string(test.workflow), func(t *testing.T) {
			raw, err := os.ReadFile(opsFixture(t, test.path))
			require.NoError(t, err)
			policy, identity, err := LoadReviewPolicyArtifact(bytes.NewReader(raw))
			require.NoError(t, err)
			require.Equal(t, test.workflow, policy.Workflow)
			require.Equal(t, test.policyID, policy.PolicyID)
			require.Equal(t, test.version, policy.PolicyVersion)
			require.Len(t, policy.TaskPolicies, test.tasks)
			require.Equal(t, test.sha256, identity)
			var rules strings.Builder
			for _, taskPolicy := range policy.TaskPolicies {
				rules.WriteString(strings.Join(taskPolicy.Rules, "\n"))
				rules.WriteByte('\n')
			}
			switch test.workflow {
			case ReviewWorkflowContentJunk:
				for _, required := range []string{
					"Anime",
					"transliterated",
					"uncertain",
					"pornography",
					"music",
					"sports",
					"software",
					"spam",
					// Disposition vocabulary. The v1 sheet required
					// real_mangled/real_absent/unsure here; those words asked
					// the reviewer a cataloguedness question and must not
					// reappear in the instruction text.
					"live_event",
					"unresolved",
					"low_information",
					"degenerate",
					"ambiguous",
				} {
					require.Contains(t, rules.String(), required)
				}
			case ReviewWorkflowMatcher:
				for _, required := range []string{
					"title, year, type, season, episode, is_anime, english, is_pack and is_adult",
					"equivalent normalized outputs",
					"frozen candidate list",
					"acceptable TMDB ID",
					"allow_abstain",
				} {
					require.Contains(t, rules.String(), required)
				}
			}

			withUnknown := bytes.Replace(
				raw,
				[]byte(`"schema_version": 1,`),
				[]byte(`"schema_version": 1, "unknown": true,`),
				1,
			)
			_, _, err = LoadReviewPolicyArtifact(bytes.NewReader(withUnknown))
			require.ErrorContains(t, err, "unknown field")

			withDuplicate := bytes.Replace(
				raw,
				[]byte(`"schema_version": 1,`),
				[]byte(`"schema_version": 1, "schema_version": 1,`),
				1,
			)
			_, _, err = LoadReviewPolicyArtifact(bytes.NewReader(withDuplicate))
			require.ErrorContains(t, err, "duplicate object key")
		})
	}
}

func TestHumanReviewProofBindsExactPolicyEvidenceAndCorpus(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("proof:a", LanguageEnglish),
		testContentRecord("proof:b", LanguageNonEnglish),
	})
	policy, err := os.ReadFile(opsFixture(
		t,
		"../../ops/llm-eval/review-policies/content-junk-review-v2.json",
	))
	require.NoError(t, err)
	manifest := reviewProofManifestFixture(corpus)
	evidence, err := json.Marshal(manifest)
	require.NoError(t, err)

	proof, err := LoadHumanReviewProof(
		bytes.NewReader(policy),
		bytes.NewReader(evidence),
		corpus,
		[]Task{TaskContentFilter},
		ReviewWorkflowContentJunk,
	)
	require.NoError(t, err)
	require.Equal(t, corpus.SHA256, proof.EvidenceCorpusSHA256)
	require.Equal(t, "content-junk-review-v2", proof.PolicyVersion)

	err = VerifyHumanReviewProof(
		proof,
		bytes.NewReader(append(append([]byte(nil), policy...), '\n')),
		bytes.NewReader(evidence),
		corpus,
		[]Task{TaskContentFilter},
		ReviewWorkflowContentJunk,
	)
	require.ErrorContains(t, err, "does not match bound proof")

	wrongSnapshot := manifest
	wrongSnapshot.SourceSnapshotSHA256 = strings.Repeat("f", 64)
	wrongSnapshotEvidence, err := json.Marshal(wrongSnapshot)
	require.NoError(t, err)
	_, err = LoadHumanReviewProof(
		bytes.NewReader(policy),
		bytes.NewReader(wrongSnapshotEvidence),
		corpus,
		[]Task{TaskContentFilter},
		ReviewWorkflowContentJunk,
	)
	require.ErrorContains(t, err, "privacy snapshot does not match")

	manifest.Items = manifest.Items[:1]
	missingEvidence, err := json.Marshal(manifest)
	require.NoError(t, err)
	_, err = LoadHumanReviewProof(
		bytes.NewReader(policy),
		bytes.NewReader(missingEvidence),
		corpus,
		[]Task{TaskContentFilter},
		ReviewWorkflowContentJunk,
	)
	require.ErrorContains(t, err, `selected source case "proof:b" lacks evidence`)
}

func TestReviewEvidenceManifestRejectsPathAndURLReferences(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("proof:opaque-ref", LanguageEnglish),
	})
	manifest := reviewProofManifestFixture(corpus)
	for _, evidenceRef := range []string{
		"local/evidence.json",
		`C:\evidence.json`,
		"https://evidence.example/item",
		"file:evidence",
	} {
		t.Run(evidenceRef, func(t *testing.T) {
			changed := manifest
			changed.Items = append([]ReviewEvidenceItem(nil), manifest.Items...)
			changed.Items[0].EvidenceRef = evidenceRef
			require.ErrorContains(
				t,
				changed.Validate(),
				"must be an opaque identifier, not a path or URL",
			)
		})
	}
}

func reviewProofManifestFixture(corpus Corpus) ReviewEvidenceManifest {
	items := make([]ReviewEvidenceItem, 0, len(corpus.Records))
	for _, record := range corpus.Records {
		items = append(items, ReviewEvidenceItem{
			CaseID:                  record.CaseID,
			EvidenceRef:             "evidence:" + record.CaseID,
			EvidenceSHA256:          strings.Repeat("e", 64),
			TaskInputSHA256:         strings.Repeat("d", 64),
			CanonicalEvidenceKind:   "policy_evidence_bundle",
			CanonicalEvidenceSHA256: strings.Repeat("e", 64),
		})
	}
	return ReviewEvidenceManifest{
		SchemaVersion:        SchemaVersion,
		ManifestID:           "evidence:proof-fixture",
		Workflow:             ReviewWorkflowContentJunk,
		CorpusSHA256:         corpus.SHA256,
		SourceSnapshotSHA256: testEvaluatorBuildSHA,
		Items:                items,
	}
}
