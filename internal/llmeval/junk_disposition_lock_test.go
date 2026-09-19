package llmeval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file is the anti-recurrence lock for the disposition contract. Each test
// names the specific regression it prevents, and each is written so that
// reverting the corresponding production change makes it fail. The adversarial
// checks are recorded in the comments so a future reader can re-run them.

// TestKeepIsTheOnlyHarmDenominator sweeps every content class against every
// production action and asserts that harm eligibility tracks the KEEP
// disposition exactly — never cataloguedness.
//
// Adversarial check: restore `keep := ...Verdict == JunkVerdictRealMangled ||
// ...RealAbsent` in comparison.go and this fails on live_event, xxx, music and
// every other delete class, because a correctly-deleted sport broadcast would
// once again count as harm.
func TestKeepIsTheOnlyHarmDenominator(t *testing.T) {
	actions := []JunkPurgeAction{
		JunkPurgeActionKeep,
		JunkPurgeActionJunk,
		JunkPurgeActionAbstain,
	}
	for _, class := range JunkContentClasses() {
		if class == JunkClassAmbiguous {
			continue // never gold
		}
		disposition, err := DeriveJunkDisposition(JunkDispositionPolicyV1, class)
		if err != nil {
			t.Fatalf("class %q: %v", class, err)
		}
		for _, action := range actions {
			record := CorpusRecord{
				Task: TaskJunkPurge,
				Tier: GoldTierAResolvableKeep,
				JunkPurge: &JunkPurgeCase{
					Input: JunkPurgeInput{TorrentName: "x"},
					Expected: JunkPurgeExpected{
						Disposition:       disposition,
						ContentClass:      class,
						DispositionPolicy: JunkDispositionPolicyV1,
					},
				},
			}
			result := ResultRecord{
				Status:    ResultStatusOK,
				JunkPurge: &JunkPurgeResult{Action: action},
			}
			got := indicatorsForCase(record, result)

			wantEligible := disposition == JunkDispositionKeep
			if got.harmEligible != wantEligible {
				t.Fatalf(
					"class %q (disposition %q) action %q: harmEligible=%v, want %v",
					class, disposition, action, got.harmEligible, wantEligible,
				)
			}
			wantHarm := wantEligible && action == JunkPurgeActionJunk
			if got.harm != wantHarm {
				t.Fatalf(
					"class %q (disposition %q) action %q: harm=%v, want %v",
					class, disposition, action, got.harm, wantHarm,
				)
			}
		}
	}
}

// TestDeletingALiveEventIsNeverHarm is the single case the whole contract
// exists for, pinned on its own so the failure message says what broke.
func TestDeletingALiveEventIsNeverHarm(t *testing.T) {
	record := CorpusRecord{
		Task: TaskJunkPurge,
		Tier: GoldTierCRepresentativeDelete,
		JunkPurge: &JunkPurgeCase{
			Input: JunkPurgeInput{
				TorrentName: "FIFA World Cup 2026 Quarter-final AUT vs BRA 1080p",
			},
			Expected: JunkPurgeExpected{
				Disposition:       JunkDispositionDelete,
				ContentClass:      JunkClassLiveEvent,
				DispositionPolicy: JunkDispositionPolicyV1,
			},
		},
	}
	got := indicatorsForCase(record, ResultRecord{
		Status:    ResultStatusOK,
		JunkPurge: &JunkPurgeResult{Action: JunkPurgeActionJunk},
	})
	if got.harm || got.harmEligible {
		t.Fatal(
			"deleting a live sport broadcast scored as harm: the gold contract " +
				"has reverted to cataloguedness. A live event resolves against a " +
				"catalogued title but policy intends it deleted.",
		)
	}
	if !got.success {
		t.Fatal("deleting a delete-disposition case should score as success")
	}
}

// TestJunkPurgeGoldCarriesNoCatalogueField reflects over the serialised gold
// shape. The defect being prevented is not a wrong value but a re-added field:
// once a `verdict` or `catalogued` key exists on the expected payload, some
// scoring path will read it.
//
// Adversarial check: add `Verdict JunkVerdict \`json:"verdict"\“ back to
// JunkPurgeExpected and this fails.
func TestJunkPurgeGoldCarriesNoCatalogueField(t *testing.T) {
	want := map[string]struct{}{
		"disposition":        {},
		"content_class":      {},
		"disposition_policy": {},
	}
	typ := reflect.TypeOf(JunkPurgeExpected{})
	got := make(map[string]struct{}, typ.NumField())
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		got[name] = struct{}{}
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf(
			"JunkPurgeExpected json fields = %v, want exactly %v. The gold "+
				"contract labels disposition; a provenance field on it will be "+
				"scored as one.",
			keysOf(got), keysOf(want),
		)
	}
}

// TestDispositionIsNeverAuthored pins the derivation interlock: a hand-written
// disposition that disagrees with the policy must be rejected, because a gold
// record whose disposition is authored rather than derived cannot be re-derived
// when the policy changes — which is the entire economic argument for the
// contract.
func TestDispositionIsNeverAuthored(t *testing.T) {
	bad := JunkPurgeExpected{
		Disposition:       JunkDispositionKeep, // live_event derives to delete
		ContentClass:      JunkClassLiveEvent,
		DispositionPolicy: JunkDispositionPolicyV1,
	}
	if err := bad.Validate(GoldTierBHardKeep); err == nil {
		t.Fatal("an authored disposition contradicting the policy was accepted")
	}
	good := JunkPurgeExpected{
		Disposition:       JunkDispositionDelete,
		ContentClass:      JunkClassLiveEvent,
		DispositionPolicy: JunkDispositionPolicyV1,
	}
	if err := good.Validate(GoldTierBHardKeep); err != nil {
		t.Fatalf("a correctly derived record was rejected: %v", err)
	}
}

// TestAmbiguousCannotBeGold pins the decline path. Ambiguous must be sayable at
// review time and unstorable as gold.
func TestAmbiguousCannotBeGold(t *testing.T) {
	expected := JunkPurgeExpected{
		Disposition:       JunkDispositionAbstain,
		ContentClass:      JunkClassAmbiguous,
		DispositionPolicy: JunkDispositionPolicyV1,
	}
	for _, tier := range []GoldTier{
		GoldTierAResolvableKeep,
		GoldTierBHardKeep,
		GoldTierCRepresentativeDelete,
	} {
		if err := expected.Validate(tier); err == nil {
			t.Fatalf("ambiguous was accepted as gold in tier %q", tier)
		}
	}
	if err := expected.Validate(GoldTierSamplingCandidate); err != nil {
		t.Fatalf("ambiguous must remain valid on a sampling candidate: %v", err)
	}
}

// TestEveryContentClassDerivesADisposition guards against a class being added
// to the enum but not to the table, which would fail closed at validation time
// with a confusing "unknown content_class" rather than at build time.
func TestEveryContentClassDerivesADisposition(t *testing.T) {
	for _, class := range JunkContentClasses() {
		if _, err := DeriveJunkDisposition(
			JunkDispositionPolicyV1, class,
		); err != nil {
			t.Fatalf("class %q has no disposition: %v", class, err)
		}
	}
	if _, err := DeriveJunkDisposition(
		JunkDispositionPolicyV1, JunkContentClass("not_a_class"),
	); err == nil {
		t.Fatal("an unknown content class derived a disposition")
	}
	if _, err := DeriveJunkDisposition(
		"some-other-policy", JunkClassMovie,
	); err == nil {
		t.Fatal("an unknown policy id derived a disposition")
	}
}

// TestGateStringsSpeakDisposition is AC-5. Wording drifts back to provenance
// far more easily than logic does, and the gate strings are what an owner reads
// in a sign-off.
func TestGateStringsSpeakDisposition(t *testing.T) {
	harm, success := comparisonDefinitions(TaskJunkPurge)
	safety := safetySuccessDefinition(TaskJunkPurge)
	for name, text := range map[string]string{
		"harm":           harm,
		"success":        success,
		"safety_success": safety,
	} {
		lower := strings.ToLower(text)
		if !strings.Contains(lower, "keep") {
			t.Fatalf("%s gate string does not mention keep: %q", name, text)
		}
		for _, banned := range []string{"real media", "real_"} {
			if strings.Contains(lower, banned) {
				t.Fatalf(
					"%s gate string contains provenance wording %q: %q",
					name, banned, text,
				)
			}
		}
	}
}

// TestJunkReviewLabelsMatchPolicyArtifact ties the Go decision set to the
// reviewer's own instruction sheet. If they diverge, reviewers are answering a
// different question from the one the code scores.
func TestJunkReviewLabelsMatchPolicyArtifact(t *testing.T) {
	raw, err := os.ReadFile(opsFixture(t, filepath.Clean(
		"../../ops/llm-eval/review-policies/content-junk-review-v2.json",
	)))
	if err != nil {
		t.Fatalf("read review policy: %v", err)
	}
	var policy struct {
		PolicyVersion string `json:"policy_version"`
		TaskPolicies  []struct {
			Task      string   `json:"task"`
			Decisions []string `json:"decisions"`
		} `json:"task_policies"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("parse review policy: %v", err)
	}
	var decisions []string
	for _, tp := range policy.TaskPolicies {
		if tp.Task == string(TaskJunkPurge) {
			decisions = append([]string(nil), tp.Decisions...)
		}
	}
	if decisions == nil {
		t.Fatal("review policy has no junkpurge task policy")
	}
	want, ok := expectedPolicyDecisions(ReviewWorkflowContentJunk, TaskJunkPurge)
	if !ok {
		t.Fatal("no expected decision set for junkpurge")
	}
	sort.Strings(decisions)
	if !reflect.DeepEqual(decisions, want) {
		t.Fatalf(
			"reviewer instruction sheet decisions %v != code decision set %v",
			decisions, want,
		)
	}
	// And the v1 sheet must no longer validate: it asks the wrong question.
	v1, err := os.ReadFile(opsFixture(t, filepath.Clean(
		"../../ops/llm-eval/review-policies/content-junk-review-v1.json",
	)))
	if err != nil {
		t.Fatalf("read v1 review policy: %v", err)
	}
	if err := json.Unmarshal(v1, &policy); err != nil {
		t.Fatalf("parse v1 review policy: %v", err)
	}
	for _, tp := range policy.TaskPolicies {
		if tp.Task != string(TaskJunkPurge) {
			continue
		}
		v1Decisions := append([]string(nil), tp.Decisions...)
		sort.Strings(v1Decisions)
		if reflect.DeepEqual(v1Decisions, want) {
			t.Fatal(
				"content-junk-review-v1 still validates; it poses the " +
					"cataloguedness question and must not be usable",
			)
		}
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCrossTierAggregationIsASchemaError pins W1.5. Two tiers in one scored
// corpus must fail loudly, not average quietly.
func TestCrossTierAggregationIsASchemaError(t *testing.T) {
	a := testJunkRecord("junk:tier-a", JunkClassMovie)
	a.Tier = GoldTierAResolvableKeep
	b := testJunkRecord("junk:tier-b", JunkClassMovie)
	b.Tier = GoldTierBHardKeep

	err := rejectCrossTierJunkPurge([]CorpusRecord{a, b})
	if err == nil {
		t.Fatal("a corpus mixing tier A and tier B was accepted")
	}
	if !strings.Contains(err.Error(), "tier_a_resolvable_keep") ||
		!strings.Contains(err.Error(), "tier_b_hard_keep") {
		t.Fatalf("error does not name both tiers: %v", err)
	}
	if err := rejectCrossTierJunkPurge([]CorpusRecord{a, a}); err != nil {
		t.Fatalf("a single-tier corpus was rejected: %v", err)
	}
}
