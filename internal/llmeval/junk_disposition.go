package llmeval

import "fmt"

// The junk-purge gold contract labels DISPOSITION — what deployed policy should
// do with a torrent — not PROVENANCE, which is whether its title resolves
// against a catalogue. The two are different predicates: a live sport broadcast
// resolves perfectly against the catalogued title "FIFA World Cup" and is
// provenance-real, while deployed policy intends it as junk. A model scored
// against a cataloguedness corpus is scored on the wrong question and penalised
// for correctly deleting sport.
//
// The design is two axes:
//
//	human labels ──► ContentClass   (durable, 15 values)
//	                      │  ordered table, first match wins
//	                      ▼
//	                 Disposition    (derived: keep | delete | abstain)
//
// A reviewer labels ContentClass ONLY. Disposition is derived. A policy
// revision — flipping adult from keep to delete, say — is therefore a
// re-derivation over existing labels, never a re-review. The human review
// budget is spent once; this is what stops a later policy change spending it
// again.
//
// The table below must stay in lockstep with
// ops/llm-eval/junk-disposition-policy-v1.json, which carries the per-class
// anchors and the SHA-256 pin of the deployed judge prompt. See
// docs/design/junk-disposition-contract.md.

// JunkDisposition is what deployed policy should do with a candidate. It is
// deliberately the same three-way shape as JunkPurgeAction so the scored
// confusion matrix is square.
type JunkDisposition string

const (
	// JunkDispositionKeep is the ONLY harm denominator. False-junk is a rate
	// over content policy intends to retain; stating it over provenance-real
	// imports the live-event error straight into the deletion authorisation.
	JunkDispositionKeep JunkDisposition = "keep"
	// JunkDispositionDelete is recoverable error territory: wrongly keeping
	// junk costs clutter, wrongly deleting real content is irreversible.
	JunkDispositionDelete JunkDisposition = "delete"
	// JunkDispositionAbstain is excluded from BOTH the harm numerator and the
	// harm denominator. It is not a quiet keep.
	JunkDispositionAbstain JunkDisposition = "abstain"
)

// JunkContentClass is the durable human judgement. Nine of the fifteen values
// are model.ContentType strings verbatim, so their disposition is config-exact
// against CLASSIFIER_DELETE_CONTENT_TYPES / CLASSIFIER_DELETE_XXX.
type JunkContentClass string

const (
	JunkClassDegenerate     JunkContentClass = "degenerate"
	JunkClassXxx            JunkContentClass = "xxx"
	JunkClassLiveEvent      JunkContentClass = "live_event"
	JunkClassMusic          JunkContentClass = "music"
	JunkClassGame           JunkContentClass = "game"
	JunkClassSoftware       JunkContentClass = "software"
	JunkClassEbook          JunkContentClass = "ebook"
	JunkClassAudiobook      JunkContentClass = "audiobook"
	JunkClassComic          JunkContentClass = "comic"
	JunkClassInstructional  JunkContentClass = "instructional"
	JunkClassMovie          JunkContentClass = "movie"
	JunkClassTVShow         JunkContentClass = "tv_show"
	JunkClassUnresolved     JunkContentClass = "unresolved"
	JunkClassLowInformation JunkContentClass = "low_information"
	// JunkClassAmbiguous lets a reviewer decline, so the adjudication rate can
	// be reported honestly. It is NOT storable as gold — see Validate.
	JunkClassAmbiguous JunkContentClass = "ambiguous"
)

// JunkDispositionPolicyV1 is the policy_id of
// ops/llm-eval/junk-disposition-policy-v1.json. Every gold record records the
// policy that derived its disposition, which is what makes a later policy
// revision a re-derivation rather than a re-review.
const JunkDispositionPolicyV1 = "junk-disposition-policy-v1-2026-07-26"

// junkDispositionV1 is the ordered derivation table, first match wins. Order is
// load-bearing where classes overlap: a concert video is both live_event and
// arguably music, and every delete-side class sorts before every keep-side one
// so a spurious catalogue resolution cannot promote junk to keep.
var junkDispositionV1 = []struct {
	Class       JunkContentClass
	Disposition JunkDisposition
}{
	{JunkClassDegenerate, JunkDispositionDelete},
	{JunkClassXxx, JunkDispositionDelete},
	{JunkClassLiveEvent, JunkDispositionDelete},
	{JunkClassMusic, JunkDispositionDelete},
	{JunkClassGame, JunkDispositionDelete},
	{JunkClassSoftware, JunkDispositionDelete},
	{JunkClassEbook, JunkDispositionDelete},
	{JunkClassAudiobook, JunkDispositionDelete},
	{JunkClassComic, JunkDispositionDelete},
	// instructional is UNANCHORED — it appears in no content_type enum value,
	// no classifier delete list, and not in the deployed judge prompt. Never
	// delete on a class with no deployed anchor.
	{JunkClassInstructional, JunkDispositionKeep},
	{JunkClassMovie, JunkDispositionKeep},
	{JunkClassTVShow, JunkDispositionKeep},
	{JunkClassUnresolved, JunkDispositionAbstain},
	{JunkClassLowInformation, JunkDispositionAbstain},
	{JunkClassAmbiguous, JunkDispositionAbstain},
}

// JunkContentClasses returns every class in derivation order.
func JunkContentClasses() []JunkContentClass {
	classes := make([]JunkContentClass, 0, len(junkDispositionV1))
	for _, row := range junkDispositionV1 {
		classes = append(classes, row.Class)
	}
	return classes
}

// JunkDispositions returns the scored disposition axis in a stable order.
func JunkDispositions() []JunkDisposition {
	return []JunkDisposition{
		JunkDispositionKeep,
		JunkDispositionDelete,
		JunkDispositionAbstain,
	}
}

// DeriveJunkDisposition resolves a content class to its disposition under a
// named policy. It is the single place the mapping exists; nothing else may
// hard-code it, because a hard-coded copy is how a policy revision silently
// becomes a re-review.
func DeriveJunkDisposition(
	policy string,
	class JunkContentClass,
) (JunkDisposition, error) {
	if policy != JunkDispositionPolicyV1 {
		return "", fmt.Errorf(
			"unknown disposition_policy %q (this build derives %q only)",
			policy, JunkDispositionPolicyV1,
		)
	}
	for _, row := range junkDispositionV1 {
		if row.Class == class {
			return row.Disposition, nil
		}
	}
	return "", fmt.Errorf("unknown content_class %q", class)
}

// dispositionOfAction projects a production action onto the disposition axis so
// the confusion matrix is square. "junk" is the action; "delete" is the
// disposition it realises. Naming them apart is deliberate: "junk" is the
// vocabulary that carried the provenance confusion in the first place.
func dispositionOfAction(action JunkPurgeAction) JunkDisposition {
	switch action {
	case JunkPurgeActionKeep:
		return JunkDispositionKeep
	case JunkPurgeActionJunk:
		return JunkDispositionDelete
	case JunkPurgeActionAbstain:
		return JunkDispositionAbstain
	default:
		return ""
	}
}

// GoldTier separates populations that must never be blended into one score.
// Tier A is the easy end of the distribution by construction — oracle-resolvable
// titles — so aggregating it with Tier B lets it mask exactly the hard slice
// where false-junk lives. That is the same mistake as scoring against the 2,506
// off-distribution import-confirmed hashes.
type GoldTier string

const (
	// GoldTierSamplingCandidate is a row drawn for review but not yet gold. It
	// may carry ambiguous or otherwise unsettled labels. It is also the tier
	// for the synthetic protocol fixture, which exercises transport and the
	// contract but is never a gold measurement — a fixture that claims a gold
	// tier gets scored as if it bounded something, and trips
	// rejectCrossTierJunkPurge the moment it covers more than one disposition.
	GoldTierSamplingCandidate GoldTier = "sampling_candidate"
	// GoldTierAResolvableKeep bounds false-junk on RESOLVABLE keep-worthy
	// content only.
	GoldTierAResolvableKeep GoldTier = "tier_a_resolvable_keep"
	// GoldTierBHardKeep bounds it on HARD keep-worthy content — transliterated,
	// foreign, structurally unparseable names.
	GoldTierBHardKeep GoldTier = "tier_b_hard_keep"
	// GoldTierCRepresentativeDelete measures usefulness and recall. NEVER
	// safety: it contains no keep-worthy content to lose.
	GoldTierCRepresentativeDelete GoldTier = "tier_c_representative_delete"
)

// IsGold reports whether a tier carries adjudicated labels.
func (t GoldTier) IsGold() bool {
	switch t {
	case GoldTierAResolvableKeep,
		GoldTierBHardKeep,
		GoldTierCRepresentativeDelete:
		return true
	default:
		return false
	}
}

func (t GoldTier) valid() bool {
	return t == GoldTierSamplingCandidate || t.IsGold()
}

// Validate is the anti-recurrence interlock. Three rules, each pinned by a test
// that fails if the rule is reverted:
//
//  1. Disposition must equal what the policy derives from ContentClass. A
//     hand-written disposition is a provenance judgement wearing a disposition
//     name, and it defeats free re-derivation.
//  2. ContentClass "ambiguous" cannot be gold. A reviewer declining is a fact
//     to count, not a label to score against.
//  3. Only a sampling candidate may carry an unsettled label.
func (e JunkPurgeExpected) Validate(tier GoldTier) error {
	if !tier.valid() {
		return fmt.Errorf("junkpurge expected: unknown tier %q", tier)
	}
	if e.DispositionPolicy == "" {
		return fmt.Errorf("junkpurge expected: disposition_policy is required")
	}
	derived, err := DeriveJunkDisposition(e.DispositionPolicy, e.ContentClass)
	if err != nil {
		return fmt.Errorf("junkpurge expected: %w", err)
	}
	if e.Disposition != derived {
		return fmt.Errorf(
			"junkpurge expected: disposition %q contradicts policy %q, which "+
				"derives %q from content_class %q; disposition is DERIVED, "+
				"never authored",
			e.Disposition, e.DispositionPolicy, derived, e.ContentClass,
		)
	}
	if tier.IsGold() && e.ContentClass == JunkClassAmbiguous {
		return fmt.Errorf(
			"junkpurge expected: content_class %q cannot be gold (tier %q); "+
				"a declined adjudication is counted, not scored",
			JunkClassAmbiguous, tier,
		)
	}
	return nil
}
