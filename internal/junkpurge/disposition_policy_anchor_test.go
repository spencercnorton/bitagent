package junkpurge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// policyAnchorPath is the gold label space for the junk-purge task. It defines
// disposition (keep/delete/abstain) per content class, and every one of its
// delete-side classes is justified by a clause of judgeInstructions below.
const policyAnchorPath = "../../ops/llm-eval/junk-disposition-policy-v1.json"

type dispositionPolicyFile struct {
	PolicyID string `json:"policy_id"`
	Anchors  struct {
		Primary struct {
			Symbol             string `json:"symbol"`
			JudgePromptVersion string `json:"judge_prompt_version"`
			SHA256             string `json:"sha256"`
			LengthBytes        int    `json:"length_bytes"`
		} `json:"primary"`
	} `json:"anchors"`
}

// TestJudgePromptHashMatchesDispositionPolicy is the anti-drift lock between
// the deployed policy artifact and the gold label space derived from it.
//
// The failure it exists to prevent is silent: someone edits judgeInstructions
// to stop deleting (say) live events, the gold corpus keeps scoring models
// against the old disposition table, and every reported false-junk number
// quietly measures the wrong policy. Neither file imports the other, so nothing
// else would catch it.
//
// If this fails, do NOT just paste in the new hash. Re-read the disposition
// table in the policy file against the changed prompt clause by clause, decide
// whether any content_class disposition must move, and update both together.
func TestJudgePromptHashMatchesDispositionPolicy(t *testing.T) {
	raw, err := os.ReadFile(opsFixture(t, filepath.Clean(policyAnchorPath)))
	if err != nil {
		t.Fatalf("read disposition policy: %v", err)
	}
	var policy dispositionPolicyFile
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("parse disposition policy: %v", err)
	}

	sum := sha256.Sum256([]byte(judgeInstructions))
	got := hex.EncodeToString(sum[:])

	if policy.Anchors.Primary.Symbol != "judgeInstructions" {
		t.Fatalf(
			"policy %s anchors symbol %q, want judgeInstructions",
			policy.PolicyID, policy.Anchors.Primary.Symbol,
		)
	}
	if policy.Anchors.Primary.JudgePromptVersion != judgePromptVersion {
		t.Fatalf(
			"policy %s pins prompt version %q, deployed is %q",
			policy.PolicyID,
			policy.Anchors.Primary.JudgePromptVersion,
			judgePromptVersion,
		)
	}
	if policy.Anchors.Primary.LengthBytes != len(judgeInstructions) {
		t.Fatalf(
			"policy %s pins prompt length %d, deployed is %d",
			policy.PolicyID,
			policy.Anchors.Primary.LengthBytes,
			len(judgeInstructions),
		)
	}
	if policy.Anchors.Primary.SHA256 != got {
		t.Fatalf(
			"deployed judge prompt has drifted from the gold label space.\n"+
				"  policy %s pins sha256 %s\n"+
				"  judgeInstructions is    %s\n"+
				"Re-derive the content_class dispositions against the new prompt "+
				"before updating the hash.",
			policy.PolicyID, policy.Anchors.Primary.SHA256, got,
		)
	}
}
