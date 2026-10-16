package llmeval

import (
	"strings"
	"testing"
)

func TestCorpusPrivacyAttestationFailsClosedForProductionSources(t *testing.T) {
	synthetic := testContentRecord("privacy:synthetic", LanguageEnglish)
	synthetic.Label = LabelMetadata{
		Provenance:    LabelProvenanceSynthetic,
		Strength:      LabelStrengthWeak,
		PolicyVersion: "synthetic-v1",
	}
	synthetic.PrivacyAttestation = nil
	if err := synthetic.Validate(); err != nil {
		t.Fatalf("synthetic protocol record should remain backward compatible: %v", err)
	}

	production := synthetic
	production.CaseID = "privacy:production"
	production.GroupID = "group:privacy:production"
	production.Label = LabelMetadata{
		Provenance:    LabelProvenanceProductionTeacher,
		Strength:      LabelStrengthTeacher,
		PolicyVersion: "teacher-v1",
	}
	if err := production.Validate(); err == nil ||
		!strings.Contains(err.Error(), "privacy_attestation: required") {
		t.Fatalf("missing production privacy error = %v", err)
	}

	production.PrivacyAttestation = &SourcePrivacyAttestation{
		PlanID:               "bitagent-llm-corpus-v1-2026-07-24",
		SourceSnapshotSHA256: testEvaluatorBuildSHA,
		Status:               PrivacyVerifiedPostRulePublic,
	}
	if err := production.Validate(); err != nil {
		t.Fatalf("attested production record: %v", err)
	}
	corpus := mustCorpus(t, []CorpusRecord{production})
	if err := ValidateHostedCorpusPrivacy(corpus); err != nil {
		t.Fatalf("hosted privacy preflight: %v", err)
	}
	if err := ValidateGoldCorpus(corpus); err == nil ||
		!strings.Contains(err.Error(), "human_review/gold") {
		t.Fatalf("teacher corpus execution gate error = %v", err)
	}
}

func TestCorpusValidationRejectsSecretLikeMaterial(t *testing.T) {
	record := testContentRecord("content:secret", LanguageEnglish)
	record.ContentFilter.Input.Title = "release sk-1234567890abcdefghijklmnop"

	err := record.Validate()
	if err == nil || !strings.Contains(err.Error(), "secret-like material") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestCorpusValidationRequiresMatchingSingleTaskPayload(t *testing.T) {
	record := testContentRecord("content:union", LanguageEnglish)
	record.JunkPurge = &JunkPurgeCase{
		Input:    JunkPurgeInput{TorrentName: "another title"},
		Expected: JunkPurgeExpected{Disposition: JunkDispositionDelete, ContentClass: JunkClassDegenerate, DispositionPolicy: JunkDispositionPolicyV1},
	}

	err := record.Validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one payload") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestMatcherRerankValidationRequiresFrozenGoldCandidate(t *testing.T) {
	record := testRerankRecord("rerank:missing-gold", MatcherRerankExpected{
		AcceptableTMDBIDs: []int64{999},
	})

	err := record.Validate()
	if err == nil || !strings.Contains(err.Error(), "is not in candidates") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestResultValidationSeparatesSchemaErrorsFromOutputs(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testJunkRecord("junk:schema", JunkClassDegenerate),
	})
	result := testResult(corpus, "junk:schema", TaskJunkPurge)
	result.Status = ResultStatusSchemaError
	result.ErrorCode = "invalid-json"
	result.JunkPurge = &JunkPurgeResult{
		Action:     JunkPurgeActionJunk,
		Verdict:    JunkVerdictJunk,
		Confidence: 0.99,
	}

	err := result.Validate()
	if err == nil || !strings.Contains(err.Error(), "must not carry a task payload") {
		t.Fatalf("Validate error = %v", err)
	}

	result.JunkPurge = nil
	if err := result.Validate(); err != nil {
		t.Fatalf("valid schema-error result rejected: %v", err)
	}
}

func TestResultValidationChecksOptionalRequestTiming(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testJunkRecord("junk:timing", JunkClassDegenerate),
	})
	result := testResult(corpus, "junk:timing", TaskJunkPurge)
	result.Status = ResultStatusSchemaError
	result.ErrorCode = "invalid-json"
	result.Usage.CostSource = CostSourceUnavailable
	result.RequestTiming = &RequestTiming{ElapsedMS: 0, DeadlineMS: 60_000}

	err := result.Validate()
	if err == nil || !strings.Contains(err.Error(), "request_timing: elapsed_ms") {
		t.Fatalf("Validate error = %v", err)
	}

	result.RequestTiming.ElapsedMS = 1
	if err := result.Validate(); err != nil {
		t.Fatalf("valid timed schema-error result rejected: %v", err)
	}
}

func TestResultCostSourceRequiredForNewButNotLegacyRows(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:cost-source", LanguageEnglish),
	})
	result := testResult(corpus, "content:cost-source", TaskContentFilter)
	result.ContentFilter = &ContentFilterResult{
		Action: ContentFilterActionKeep, IsEnglish: boolPointer(true), Confidence: 0.9,
	}
	result.Usage = Usage{
		InputTokens: 10, OutputTokens: 2, CostMicroUSD: 3,
		AccountingComplete: true,
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("intentional timing-less legacy result rejected: %v", err)
	}

	result.ExecutionAudit.CostAccountingVersion = CurrentCostAccountingVersion
	result.RequestTiming = &RequestTiming{
		ElapsedMS: 10, DeadlineMS: 8_000, TotalElapsedMS: 10,
	}
	if err := result.Validate(); err == nil ||
		!strings.Contains(err.Error(), "cost_source is required") {
		t.Fatalf("new formal result missing provenance error = %v", err)
	}
	result.Usage.CostSource = CostSourceProviderReported
	if err := result.Validate(); err != nil {
		t.Fatalf("explicit new formal cost provenance rejected: %v", err)
	}

	result.ExecutionAudit.CostAccountingVersion = CurrentCostAccountingVersion + 1
	if err := result.Validate(); err == nil ||
		!strings.Contains(err.Error(), "cost_accounting_version") {
		t.Fatalf("unknown cost accounting version error = %v", err)
	}
}

func TestResultValidationBindsOptionalCampaignToExactRow(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:campaign-binding", LanguageEnglish),
	})
	result := testResult(corpus, "content:campaign-binding", TaskContentFilter)
	result.ContentFilter = &ContentFilterResult{
		Action:     ContentFilterActionKeep,
		IsEnglish:  boolPointer(true),
		Confidence: 0.9,
	}
	plan := testCampaignPlan()
	binding, err := plan.RunBinding(
		strings.Repeat("9", 64),
		"run-candidate-contentfilter",
	)
	if err != nil {
		t.Fatalf("RunBinding: %v", err)
	}
	binding.SystemID = result.System.SystemID
	binding.Task = result.Task
	binding.SystemManifestSHA256 = result.ExecutionAudit.ManifestSHA256
	binding.Corpus.SHA256 = result.CorpusSHA256
	binding.RouteSnapshot.SHA256 =
		result.ExecutionAudit.Route.SnapshotSHA256
	result.ExecutionAudit.Campaign = &binding
	if err := result.Validate(); err != nil {
		t.Fatalf("valid campaign-bound result: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CampaignRunBinding)
		want   string
	}{
		{
			name: "system",
			mutate: func(value *CampaignRunBinding) {
				value.SystemID = "different-system"
			},
			want: "system_id differs",
		},
		{
			name: "task",
			mutate: func(value *CampaignRunBinding) {
				value.Task = TaskJunkPurge
			},
			want: "task differs",
		},
		{
			name: "corpus",
			mutate: func(value *CampaignRunBinding) {
				value.Corpus.SHA256 = strings.Repeat("8", 64)
			},
			want: "corpus identity differs",
		},
		{
			name: "manifest",
			mutate: func(value *CampaignRunBinding) {
				value.SystemManifestSHA256 = strings.Repeat("7", 64)
			},
			want: "manifest identity differs",
		},
		{
			name: "route snapshot",
			mutate: func(value *CampaignRunBinding) {
				value.RouteSnapshot.SHA256 = strings.Repeat("6", 64)
			},
			want: "route snapshot identity differs",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := result
			campaign := *result.ExecutionAudit.Campaign
			if campaign.RouteSnapshot != nil {
				route := *campaign.RouteSnapshot
				campaign.RouteSnapshot = &route
			}
			test.mutate(&campaign)
			tampered.ExecutionAudit.Campaign = &campaign
			err := tampered.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateResultsRejectsMixedCampaignRuns(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("campaign-mix-1", LanguageEnglish),
		testContentRecord("campaign-mix-2", LanguageEnglish),
	})
	makeResult := func(caseID string) ResultRecord {
		result := testResult(corpus, caseID, TaskContentFilter)
		result.ContentFilter = &ContentFilterResult{
			Action:     ContentFilterActionKeep,
			IsEnglish:  boolPointer(true),
			Confidence: 0.9,
		}
		return result
	}
	first := makeResult("campaign-mix-1")
	second := makeResult("campaign-mix-2")
	plan := testCampaignPlan()
	binding, err := plan.RunBinding(
		strings.Repeat("9", 64),
		"run-candidate-contentfilter",
	)
	if err != nil {
		t.Fatalf("RunBinding: %v", err)
	}
	binding.SystemID = first.System.SystemID
	binding.SystemManifestSHA256 = first.ExecutionAudit.ManifestSHA256
	binding.Corpus.SHA256 = corpus.SHA256
	binding.RouteSnapshot.SHA256 =
		first.ExecutionAudit.Route.SnapshotSHA256
	first.ExecutionAudit.Campaign = cloneCampaignRunBinding(&binding)
	second.ExecutionAudit.Campaign = cloneCampaignRunBinding(&binding)
	second.ExecutionAudit.Campaign.RunID = "different-campaign-run"

	err = ValidateResults([]ResultRecord{first, second})
	if err == nil || !strings.Contains(err.Error(), "mixed execution artifact identities") {
		t.Fatalf("ValidateResults error = %v", err)
	}
}

func TestRequestTimingValidatesSeparatedRouteAudit(t *testing.T) {
	if (RequestTiming{ElapsedMS: 8_000, DeadlineMS: 8_000}).ExceededDeadline() {
		t.Fatal("elapsed time equal to the deadline must remain in budget")
	}
	if !(RequestTiming{ElapsedMS: 8_001, DeadlineMS: 8_000}).ExceededDeadline() {
		t.Fatal("elapsed time beyond the deadline must be reported")
	}
	if (RequestTiming{ElapsedMS: 1}).ExceededDeadline() {
		t.Fatal("timing without a valid deadline cannot establish an overrun")
	}

	valid := RequestTiming{
		ElapsedMS: 10, DeadlineMS: 8_000, TotalElapsedMS: 30,
		RouteAudit: &RouteAuditTiming{
			ElapsedMS: 20, DeadlineMS: OpenRouterGenerationAuditDeadlineMS,
			Attempts: 5, Succeeded: false, ErrorCode: "route_audit_unavailable",
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid separated timing rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*RequestTiming)
		want string
	}{
		{
			name: "missing total",
			edit: func(timing *RequestTiming) { timing.TotalElapsedMS = 0 },
			want: "requires total timing",
		},
		{
			name: "total below primary",
			edit: func(timing *RequestTiming) { timing.TotalElapsedMS = 9 },
			want: "at least elapsed_ms",
		},
		{
			name: "zero attempts",
			edit: func(timing *RequestTiming) { timing.RouteAudit.Attempts = 0 },
			want: "attempts",
		},
		{
			name: "failed without code",
			edit: func(timing *RequestTiming) { timing.RouteAudit.ErrorCode = "" },
			want: "error_code",
		},
		{
			name: "success with error",
			edit: func(timing *RequestTiming) { timing.RouteAudit.Succeeded = true },
			want: "successful audit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			timing := valid
			audit := *valid.RouteAudit
			timing.RouteAudit = &audit
			test.edit(&timing)
			if err := timing.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestContentFilterValidationPinsActionSemantics(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:drop", LanguageNonEnglish),
	})
	result := testResult(corpus, "content:drop", TaskContentFilter)
	result.ContentFilter = &ContentFilterResult{
		Action:     ContentFilterActionDrop,
		IsEnglish:  boolPointer(true),
		Confidence: 0.9,
	}

	err := result.Validate()
	if err == nil || !strings.Contains(err.Error(), "drop action requires false") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestResultValidationRejectsProductionImpossibleNormalizedActions(t *testing.T) {
	tests := []struct {
		name          string
		payload       func(ResultRecord) ResultRecord
		want          string
		thresholdOnly bool
	}{
		{
			name: "rerank attach below threshold",
			payload: func(result ResultRecord) ResultRecord {
				result.Task = TaskMatcherRerank
				result.MatcherRerank = &MatcherRerankResult{
					Action: MatcherRerankActionAttach, TMDBID: 101, Confidence: 0.59,
				}
				return result
			},
			want:          "attach action requires at least",
			thresholdOnly: true,
		},
		{
			name: "content drop below threshold",
			payload: func(result ResultRecord) ResultRecord {
				result.Task = TaskContentFilter
				result.ContentFilter = &ContentFilterResult{
					Action: ContentFilterActionDrop, IsEnglish: boolPointer(false),
					Confidence: 0.84,
				}
				return result
			},
			want:          "drop action requires at least",
			thresholdOnly: true,
		},
		{
			name: "content keep with false verdict",
			payload: func(result ResultRecord) ResultRecord {
				result.Task = TaskContentFilter
				result.ContentFilter = &ContentFilterResult{
					Action: ContentFilterActionKeep, IsEnglish: boolPointer(false),
					Confidence: 0.90,
				}
				return result
			},
			want: "keep action requires true",
		},
		{
			name: "content high confidence abstain",
			payload: func(result ResultRecord) ResultRecord {
				result.Task = TaskContentFilter
				result.ContentFilter = &ContentFilterResult{
					Action: ContentFilterActionAbstain, Confidence: 0.85,
				}
				return result
			},
			want:          "abstain action must be below",
			thresholdOnly: true,
		},
		{
			name: "junk below threshold action",
			payload: func(result ResultRecord) ResultRecord {
				result.Task = TaskJunkPurge
				result.JunkPurge = &JunkPurgeResult{
					Action: JunkPurgeActionJunk, Verdict: JunkVerdictJunk,
					Confidence: 0.79,
				}
				return result
			},
			want:          "junk action requires at least",
			thresholdOnly: true,
		},
		{
			name: "junk verdict normalized as keep",
			payload: func(result ResultRecord) ResultRecord {
				result.Task = TaskJunkPurge
				result.JunkPurge = &JunkPurgeResult{
					Action: JunkPurgeActionKeep, Verdict: JunkVerdictJunk,
					Confidence: 0.90,
				}
				return result
			},
			want: "keep action requires",
		},
	}

	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("case", LanguageEnglish),
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testResult(corpus, "case", TaskContentFilter)
			result = test.payload(result)
			structuralErr := result.Validate()
			if test.thresholdOnly && structuralErr != nil {
				t.Fatalf(
					"structural Validate rejected threshold-dependent action: %v",
					structuralErr,
				)
			}
			if !test.thresholdOnly &&
				(structuralErr == nil ||
					!strings.Contains(structuralErr.Error(), test.want)) {
				t.Fatalf(
					"structural Validate error = %v, want substring %q",
					structuralErr,
					test.want,
				)
			}
			err := result.ValidateWithThresholds(ProductionThresholds())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf(
					"ValidateWithThresholds error = %v, want substring %q",
					err,
					test.want,
				)
			}
		})
	}

	validLowConfidenceJunk := JunkPurgeResult{
		Action: JunkPurgeActionAbstain, Verdict: JunkVerdictJunk, Confidence: 0.79,
	}
	if err := validLowConfidenceJunk.Validate(); err != nil {
		t.Fatalf("structurally valid low-confidence junk abstention rejected: %v", err)
	}
	result := testResult(corpus, "case", TaskMatcherRerank)
	result.MatcherRerank = &MatcherRerankResult{
		Action: MatcherRerankActionAttach, TMDBID: 101, Confidence: 0.40,
	}
	if err := result.ValidateWithThresholds(ProductionThresholds()); err == nil {
		t.Fatal("production threshold unexpectedly accepted 0.40 attachment")
	}
	custom := ProductionThresholds()
	custom.MatcherAttachConfidence = 0.35
	if err := result.ValidateWithThresholds(custom); err != nil {
		t.Fatalf("frozen custom threshold did not round-trip: %v", err)
	}
}

func TestUsageValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage Usage
		want  string
	}{
		{
			name:  "negative",
			usage: Usage{InputTokens: -1},
			want:  "must be non-negative",
		},
		{
			name:  "cached exceeds input",
			usage: Usage{InputTokens: 2, CachedInputTokens: 3},
			want:  "exceeds input_tokens",
		},
		{
			name:  "cache write exceeds input",
			usage: Usage{InputTokens: 2, CacheWriteTokens: 3},
			want:  "exceeds input_tokens",
		},
		{
			name: "cached and cache write subsets exceed input",
			usage: Usage{
				InputTokens:       10,
				CachedInputTokens: 6,
				CacheWriteTokens:  5,
			},
			want: "cached_input_tokens + cache_write_tokens",
		},
		{
			name:  "reasoning exceeds output",
			usage: Usage{OutputTokens: 2, ReasoningTokens: 3},
			want:  "exceeds output_tokens",
		},
		{
			name:  "unknown cost source",
			usage: Usage{CostSource: "invented"},
			want:  "unsupported value",
		},
		{
			name:  "unavailable source with cost",
			usage: Usage{CostMicroUSD: 1, CostSource: CostSourceUnavailable},
			want:  "cannot carry cost_micro_usd",
		},
		{
			name: "unavailable source with complete accounting",
			usage: Usage{
				CostSource: CostSourceUnavailable, AccountingComplete: true,
			},
			want: "cannot claim complete accounting",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.usage.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want substring %q", err, test.want)
			}
		})
	}

	exactPartition := Usage{
		InputTokens:       10,
		CachedInputTokens: 6,
		CacheWriteTokens:  4,
	}
	if err := exactPartition.Validate(); err != nil {
		t.Fatalf("exact cached/cache-write input partition rejected: %v", err)
	}
	for _, source := range []CostSource{
		CostSourceProviderReported,
		CostSourceManifestEstimate,
	} {
		usage := Usage{CostMicroUSD: 1, CostSource: source}
		if err := usage.Validate(); err != nil {
			t.Fatalf("valid cost source %q rejected: %v", source, err)
		}
	}
}

func TestResultValidationRejectsCachePartitionTamper(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:usage-tamper", LanguageEnglish),
	})
	result := testResult(corpus, "content:usage-tamper", TaskContentFilter)
	result.ContentFilter = &ContentFilterResult{
		Action:     ContentFilterActionKeep,
		IsEnglish:  boolPointer(true),
		Confidence: 0.9,
	}
	result.Usage = Usage{
		InputTokens:       10,
		CachedInputTokens: 6,
		CacheWriteTokens:  4,
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("valid result: %v", err)
	}

	result.Usage.CacheWriteTokens++
	err := result.Validate()
	if err == nil ||
		!strings.Contains(
			err.Error(),
			"cached_input_tokens + cache_write_tokens",
		) {
		t.Fatalf("tampered cache partition error = %v", err)
	}
}

func TestValidateResultsRejectsMixedExecutionAudits(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("case-1", LanguageEnglish),
		testContentRecord("case-2", LanguageEnglish),
	})
	makeResult := func(id string) ResultRecord {
		result := testResult(corpus, id, TaskContentFilter)
		result.ContentFilter = &ContentFilterResult{
			Action:     ContentFilterActionKeep,
			IsEnglish:  boolPointer(true),
			Confidence: 0.9,
		}
		return result
	}

	tests := []struct {
		name   string
		mutate func(*ResultRecord)
		want   string
	}{
		{
			name: "snapshot",
			mutate: func(result *ResultRecord) {
				result.ExecutionAudit.Route.SnapshotSHA256 =
					"4444444444444444444444444444444444444444444444444444444444444444"
			},
			want: "mixed execution artifact identities",
		},
		{
			name: "concrete route",
			mutate: func(result *ResultRecord) {
				result.ExecutionAudit.Route.ReturnedModel = "test/other-model"
			},
			want: "mixed concrete routes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := makeResult("case-1")
			second := makeResult("case-2")
			test.mutate(&second)
			err := ValidateResults([]ResultRecord{first, second})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateResults error = %v, want %q", err, test.want)
			}
		})
	}
}
