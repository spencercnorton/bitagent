package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
)

type DecisionThresholds struct {
	MatcherAttachConfidence float64 `json:"matcher_attach_confidence"`
	ContentDropConfidence   float64 `json:"content_drop_confidence"`
	JunkConfidence          float64 `json:"junk_confidence"`
}

func ProductionThresholds() DecisionThresholds {
	return DecisionThresholds{
		MatcherAttachConfidence: 0.60,
		ContentDropConfidence:   0.85,
		JunkConfidence:          0.80,
	}
}

func (t DecisionThresholds) Validate() error {
	for name, value := range map[string]float64{
		"matcher_attach_confidence": t.MatcherAttachConfidence,
		"content_drop_confidence":   t.ContentDropConfidence,
		"junk_confidence":           t.JunkConfidence,
	} {
		if value < 0 || value > 1 {
			return fmt.Errorf("%s must be in [0,1]", name)
		}
	}
	return nil
}

// ParseCompletion converts a provider reply into the evaluator's normalized
// intervention action at the supplied production decision thresholds. Abstain
// means "take no destructive action": in production that preserves content for
// a low-confidence non-English or junk verdict and declines a matcher attach.
func ParseCompletion(
	corpusSHA string,
	record CorpusRecord,
	system SystemConfig,
	completion Completion,
	thresholds DecisionThresholds,
) ResultRecord {
	result := ResultRecord{
		SchemaVersion: SchemaVersion,
		CorpusSHA256:  corpusSHA,
		CaseID:        record.CaseID,
		Task:          record.Task,
		System:        system.Descriptor(),
		Status:        ResultStatusOK,
		Usage:         completion.Usage,
	}
	if err := thresholds.Validate(); err != nil {
		result.Status = ResultStatusError
		result.ErrorCode = "invalid_thresholds"
		return result
	}
	contractSHA, err := RequestContractSHA256(record, system, thresholds)
	if err != nil {
		result.Status = ResultStatusError
		result.ErrorCode = "request_contract_error"
		return result
	}
	result.RequestContractSHA256 = contractSHA

	raw := completion.Text
	if system.OutputContract == OutputContractPromptOnly &&
		!system.UsesDeployedWireContract() {
		raw = []byte(stripProductionWrappers(string(raw)))
	}
	if system.OutputContract == OutputContractJSONSchema {
		// JSON Schema is part of the evaluated route, not merely a prompting
		// hint. Validate the untouched provider bytes before applying any
		// production-compatible canonicalization (case folding, trimming, or
		// reason tagging), and reject duplicate keys before encoding/json can
		// erase them with last-key-wins decoding.
		if err := validateExactOutputSchema(record.Task, raw); err != nil {
			result.Status = ResultStatusSchemaError
			result.ErrorCode = boundedParseCode(err)
			return result
		}
	}

	err = nil
	deployedWireContract := system.UsesDeployedWireContract()
	switch record.Task {
	case TaskMatcherExtract:
		if deployedWireContract {
			result.MatcherExtract, err = parseProductionMatcherExtraction(raw)
		} else {
			result.MatcherExtract, err = parseMatcherExtraction(raw)
		}
	case TaskMatcherRerank:
		if deployedWireContract {
			result.MatcherRerank, err = parseProductionMatcherRerank(
				raw,
				record.MatcherRerank,
				thresholds.MatcherAttachConfidence,
				system,
			)
		} else {
			result.MatcherRerank, err = parseMatcherRerank(
				raw,
				record.MatcherRerank,
				thresholds.MatcherAttachConfidence,
				system,
				completion.MatcherSpecialist,
			)
		}
	case TaskContentFilter:
		if deployedWireContract {
			result.ContentFilter, err = parseProductionContentFilter(
				raw,
				thresholds.ContentDropConfidence,
			)
		} else {
			result.ContentFilter, err = parseContentFilter(
				raw,
				thresholds.ContentDropConfidence,
			)
		}
	case TaskJunkPurge:
		if deployedWireContract {
			result.JunkPurge, err = parseProductionJunkPurge(
				raw,
				thresholds.JunkConfidence,
			)
		} else {
			result.JunkPurge, err = parseJunkPurge(
				raw,
				thresholds.JunkConfidence,
			)
		}
	default:
		result.Status = ResultStatusError
		result.ErrorCode = "unsupported_task"
		return result
	}
	if err != nil {
		result.Status = ResultStatusSchemaError
		result.ErrorCode = boundedParseCode(err)
		result.MatcherExtract = nil
		result.MatcherRerank = nil
		result.ContentFilter = nil
		result.JunkPurge = nil
	}
	return result
}

func ErrorResult(
	corpusSHA string,
	record CorpusRecord,
	system SystemConfig,
	thresholds DecisionThresholds,
	err error,
) ResultRecord {
	code := "provider_error"
	var callError *CallError
	if errors.As(err, &callError) {
		code = callError.Code
	}
	result := ResultRecord{
		SchemaVersion: SchemaVersion,
		CorpusSHA256:  corpusSHA,
		CaseID:        record.CaseID,
		Task:          record.Task,
		System:        system.Descriptor(),
		Status:        ResultStatusError,
		ErrorCode:     code,
	}
	contractSHA, contractErr := RequestContractSHA256(record, system, thresholds)
	if contractErr != nil {
		result.ErrorCode = "request_contract_error"
		return result
	}
	result.RequestContractSHA256 = contractSHA
	return result
}

func parseProductionMatcherExtraction(
	raw []byte,
) (*MatcherExtractResult, error) {
	extraction, err := llmmatch.EvaluationDecodeExtraction(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	if !extraction.OK {
		return &MatcherExtractResult{
			Action: MatcherExtractActionAbstain,
		}, nil
	}
	mediaType := MediaType(extraction.Type)
	english := EnglishTrack(extraction.English)
	return &MatcherExtractResult{
		Action: MatcherExtractActionExtract,
		Extraction: &MatcherExtraction{
			Title: extraction.Title, Year: extraction.Year, Type: mediaType,
			Season: extraction.Season, Episode: extraction.Episode,
			IsAnime: extraction.IsAnime, English: english,
			IsPack: extraction.IsPack, IsAdult: extraction.IsAdult,
		},
	}, nil
}

func parseProductionMatcherRerank(
	raw []byte,
	testCase *MatcherRerankCase,
	attachThreshold float64,
	system SystemConfig,
) (*MatcherRerankResult, error) {
	if testCase == nil {
		return nil, fmt.Errorf("missing_case")
	}
	if testCase.Input.EffectiveMediaType != MediaTypeMovie &&
		testCase.Input.EffectiveMediaType != MediaTypeTV {
		return nil, fmt.Errorf("missing_effective_media_type")
	}
	candidates := make([]llmmatch.Candidate, 0, len(testCase.Input.Candidates))
	for _, candidate := range testCase.Input.Candidates {
		converted := llmmatch.Candidate{
			ID:        candidate.TMDBID,
			Title:     candidate.Title,
			Year:      candidate.Year,
			Overview:  candidate.Overview,
			AltTitles: append([]string(nil), candidate.AltTitles...),
		}
		candidates = append(candidates, converted)
	}
	tmdbID, confidence, err := llmmatch.EvaluationDecodeRerank(
		raw,
		candidates,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	if tmdbID == 0 {
		return &MatcherRerankResult{
			Action:       MatcherRerankActionAbstain,
			Confidence:   confidence,
			PolicyReason: "model_declined",
		}, nil
	}
	policyReason, err := matcherCandidatePolicyReason(
		testCase,
		tmdbID,
		system.RequireSourceTitle,
	)
	if err != nil {
		return nil, err
	}
	if policyReason != "" {
		return &MatcherRerankResult{
			Action:       MatcherRerankActionAbstain,
			Confidence:   confidence,
			PolicyReason: policyReason,
		}, nil
	}
	if confidence < attachThreshold {
		return &MatcherRerankResult{
			Action:       MatcherRerankActionAbstain,
			Confidence:   confidence,
			PolicyReason: "below_confidence",
		}, nil
	}
	return &MatcherRerankResult{
		Action: MatcherRerankActionAttach, TMDBID: tmdbID,
		Confidence: confidence,
	}, nil
}

func parseProductionContentFilter(
	raw []byte,
	dropThreshold float64,
) (*ContentFilterResult, error) {
	verdict, err := contentfilter.EvaluationParseVerdict(string(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	action := ContentFilterActionKeep
	if !verdict.IsEnglish {
		action = ContentFilterActionAbstain
		if verdict.Confidence >= dropThreshold {
			action = ContentFilterActionDrop
		}
	}
	isEnglish := &verdict.IsEnglish
	if action == ContentFilterActionAbstain {
		isEnglish = nil
	}
	return &ContentFilterResult{
		Action: action, IsEnglish: isEnglish,
		Confidence: verdict.Confidence,
		ReasonTag:  normalizeReasonTag(verdict.Reason),
	}, nil
}

func parseProductionJunkPurge(
	raw []byte,
	junkThreshold float64,
) (*JunkPurgeResult, error) {
	judgment, err := junkpurge.EvaluationParseJudgment(string(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	verdict := JunkVerdict(judgment.Verdict)
	action := JunkPurgeActionKeep
	if verdict == JunkVerdictUnsure {
		action = JunkPurgeActionAbstain
	}
	if verdict == JunkVerdictJunk {
		action = JunkPurgeActionAbstain
		if judgment.Confidence >= junkThreshold {
			action = JunkPurgeActionJunk
		}
	}
	return &JunkPurgeResult{
		Action: action, Verdict: verdict, Confidence: judgment.Confidence,
	}, nil
}

type matcherExtractionJSON struct {
	Title   *string `json:"title"`
	Year    *int    `json:"year"`
	Type    *string `json:"type"`
	Season  *int    `json:"season"`
	Episode *int    `json:"episode"`
	IsAnime *bool   `json:"is_anime"`
	English *string `json:"english"`
	IsPack  *bool   `json:"is_pack"`
	IsAdult *bool   `json:"is_adult"`
}

func parseMatcherExtraction(raw []byte) (*MatcherExtractResult, error) {
	var parsed matcherExtractionJSON
	if err := decodeStrict(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	if parsed.Title == nil || parsed.Year == nil || parsed.Type == nil ||
		parsed.Season == nil || parsed.Episode == nil || parsed.IsAnime == nil ||
		parsed.English == nil || parsed.IsPack == nil || parsed.IsAdult == nil {
		return nil, fmt.Errorf("missing_field")
	}
	mediaType := MediaType(strings.ToLower(strings.TrimSpace(*parsed.Type)))
	if mediaType != MediaTypeMovie && mediaType != MediaTypeTV {
		return nil, fmt.Errorf("invalid_type")
	}
	english := EnglishTrack(strings.ToLower(strings.TrimSpace(*parsed.English)))
	switch english {
	case EnglishTrackDub, EnglishTrackSub, EnglishTrackNone, EnglishTrackUnknown:
	default:
		return nil, fmt.Errorf("invalid_english")
	}
	if *parsed.Year < 0 || *parsed.Season < 0 || *parsed.Episode < 0 {
		return nil, fmt.Errorf("invalid_range")
	}
	title := strings.TrimSpace(*parsed.Title)
	if title == "" {
		return &MatcherExtractResult{Action: MatcherExtractActionAbstain}, nil
	}
	extraction := &MatcherExtraction{
		Title: title, Year: *parsed.Year, Type: mediaType,
		Season: *parsed.Season, Episode: *parsed.Episode,
		IsAnime: *parsed.IsAnime, English: english,
		IsPack: *parsed.IsPack, IsAdult: *parsed.IsAdult,
	}
	return &MatcherExtractResult{
		Action: MatcherExtractActionExtract, Extraction: extraction,
	}, nil
}

type matcherRerankJSON struct {
	TMDBID     *int64   `json:"tmdb_id"`
	Confidence *float64 `json:"confidence"`
}

func parseMatcherRerank(
	raw []byte,
	testCase *MatcherRerankCase,
	attachThreshold float64,
	system SystemConfig,
	specialistAudit *MatcherSpecialistResultAudit,
) (*MatcherRerankResult, error) {
	if testCase == nil {
		return nil, fmt.Errorf("missing_case")
	}
	var parsed matcherRerankJSON
	if err := decodeStrict(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	if parsed.TMDBID == nil || parsed.Confidence == nil {
		return nil, fmt.Errorf("missing_field")
	}
	if *parsed.TMDBID < 0 || *parsed.Confidence < 0 || *parsed.Confidence > 1 {
		return nil, fmt.Errorf("invalid_range")
	}
	if system.APIKind == APIKindEmbedding {
		if specialistAudit == nil {
			return nil, fmt.Errorf("missing_specialist_audit")
		}
		if err := validateMatcherSpecialistCompletion(
			testCase,
			system,
			*parsed.TMDBID,
			*parsed.Confidence,
			*specialistAudit,
		); err != nil {
			return nil, err
		}
	} else if specialistAudit != nil {
		return nil, fmt.Errorf("unexpected_specialist_audit")
	}
	if *parsed.TMDBID == 0 {
		return &MatcherRerankResult{
			Action: MatcherRerankActionAbstain, Confidence: *parsed.Confidence,
			PolicyReason:    "model_declined",
			SpecialistAudit: cloneMatcherSpecialistResultAudit(specialistAudit),
		}, nil
	}
	offered := false
	for _, candidate := range testCase.Input.Candidates {
		if candidate.TMDBID == *parsed.TMDBID {
			offered = true
			break
		}
	}
	if !offered {
		return nil, fmt.Errorf("candidate_not_offered")
	}
	policyReason, err := matcherCandidatePolicyReason(
		testCase,
		*parsed.TMDBID,
		system.RequireSourceTitle,
	)
	if err != nil {
		return nil, err
	}
	if policyReason != "" {
		return &MatcherRerankResult{
			Action:          MatcherRerankActionAbstain,
			Confidence:      *parsed.Confidence,
			PolicyReason:    policyReason,
			SpecialistAudit: cloneMatcherSpecialistResultAudit(specialistAudit),
		}, nil
	}
	if *parsed.Confidence < attachThreshold {
		return &MatcherRerankResult{
			Action: MatcherRerankActionAbstain, Confidence: *parsed.Confidence,
			PolicyReason:    "below_confidence",
			SpecialistAudit: cloneMatcherSpecialistResultAudit(specialistAudit),
		}, nil
	}
	return &MatcherRerankResult{
		Action: MatcherRerankActionAttach, TMDBID: *parsed.TMDBID,
		Confidence:      *parsed.Confidence,
		SpecialistAudit: cloneMatcherSpecialistResultAudit(specialistAudit),
	}, nil
}

func matcherCandidatePolicyReason(
	testCase *MatcherRerankCase,
	selectedTMDBID int64,
	requireSourceTitle bool,
) (string, error) {
	candidates := make([]llmmatch.Candidate, 0, len(testCase.Input.Candidates))
	var chosen llmmatch.Candidate
	offered := false
	for _, candidate := range testCase.Input.Candidates {
		converted := llmmatch.Candidate{
			ID:        candidate.TMDBID,
			Title:     candidate.Title,
			Year:      candidate.Year,
			Overview:  candidate.Overview,
			AltTitles: append([]string(nil), candidate.AltTitles...),
		}
		candidates = append(candidates, converted)
		if candidate.TMDBID == selectedTMDBID {
			chosen = converted
			offered = true
		}
	}
	if !offered {
		return "", fmt.Errorf("candidate_not_offered")
	}
	extraction := llmmatch.Extraction{
		Title:   testCase.Input.Extraction.Title,
		Year:    testCase.Input.Extraction.Year,
		Type:    string(testCase.Input.Extraction.Type),
		Season:  testCase.Input.Extraction.Season,
		Episode: testCase.Input.Extraction.Episode,
		IsAnime: testCase.Input.Extraction.IsAnime,
		English: string(testCase.Input.Extraction.English),
		IsPack:  testCase.Input.Extraction.IsPack,
		IsAdult: testCase.Input.Extraction.IsAdult,
		OK:      true,
	}
	isTV := testCase.Input.Extraction.Type == MediaTypeTV
	if testCase.Input.EffectiveMediaType != "" {
		isTV = testCase.Input.EffectiveMediaType == MediaTypeTV
	}
	if reason := classifier.EvaluationLLMMatchCandidateGate(
		testCase.Input.ReleaseName,
		extraction,
		isTV,
		chosen,
		candidates,
	); reason != "" {
		return reason, nil
	}
	if requireSourceTitle {
		return classifier.EvaluationLLMMatchSourceGate(
			testCase.Input.ReleaseName,
			testCase.Input.ParsedTitle,
			isTV,
			chosen,
		), nil
	}
	return "", nil
}

func validateMatcherSpecialistCompletion(
	testCase *MatcherRerankCase,
	system SystemConfig,
	selectedTMDBID int64,
	confidence float64,
	audit MatcherSpecialistResultAudit,
) error {
	if err := audit.Validate(); err != nil {
		return fmt.Errorf("invalid_specialist_audit: %w", err)
	}
	if system.PromptVersion != MatcherSpecialistAlgorithmID ||
		audit.AlgorithmID != system.PromptVersion {
		return fmt.Errorf("specialist_algorithm_mismatch")
	}
	expectedConfidence, err := MatcherSpecialistConfidence(audit.ScorePPB)
	if err != nil || confidence != expectedConfidence ||
		selectedTMDBID != audit.SelectedTMDBID {
		return fmt.Errorf("specialist_decision_mismatch")
	}
	eligible := 0
	selectedEligible := false
	for _, candidate := range testCase.Input.Candidates {
		if !MatcherSpecialistCandidateEligible(
			testCase.Input.ReleaseName,
			testCase.Input.Extraction,
			candidate,
		) {
			continue
		}
		eligible++
		if candidate.TMDBID == selectedTMDBID {
			selectedEligible = true
		}
	}
	if eligible != audit.EligibleCandidates ||
		(selectedTMDBID > 0 && !selectedEligible) {
		return fmt.Errorf("specialist_eligibility_mismatch")
	}
	return nil
}

func cloneMatcherSpecialistResultAudit(
	audit *MatcherSpecialistResultAudit,
) *MatcherSpecialistResultAudit {
	if audit == nil {
		return nil
	}
	out := *audit
	return &out
}

type contentFilterJSON struct {
	IsEnglish  *bool    `json:"is_english"`
	Confidence *float64 `json:"confidence"`
	Reason     *string  `json:"reason"`
}

func parseContentFilter(raw []byte, dropThreshold float64) (*ContentFilterResult, error) {
	var parsed contentFilterJSON
	if err := decodeStrict(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	if parsed.IsEnglish == nil || parsed.Confidence == nil || parsed.Reason == nil {
		return nil, fmt.Errorf("missing_field")
	}
	if *parsed.Confidence < 0 || *parsed.Confidence > 1 {
		return nil, fmt.Errorf("invalid_range")
	}
	action := ContentFilterActionKeep
	if !*parsed.IsEnglish {
		action = ContentFilterActionAbstain
		if *parsed.Confidence >= dropThreshold {
			action = ContentFilterActionDrop
		}
	}
	isEnglish := parsed.IsEnglish
	if action == ContentFilterActionAbstain {
		// Abstention is the normalized production action. Do not retain a
		// low-confidence false verdict that downstream code could mistake for
		// an actionable drop.
		isEnglish = nil
	}
	return &ContentFilterResult{
		Action: action, IsEnglish: isEnglish,
		Confidence: *parsed.Confidence, ReasonTag: normalizeReasonTag(*parsed.Reason),
	}, nil
}

type junkPurgeJSON struct {
	Verdict    *string  `json:"verdict"`
	Confidence *float64 `json:"confidence"`
}

func parseJunkPurge(raw []byte, junkThreshold float64) (*JunkPurgeResult, error) {
	var parsed junkPurgeJSON
	if err := decodeStrict(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid_json: %w", err)
	}
	if parsed.Verdict == nil || parsed.Confidence == nil {
		return nil, fmt.Errorf("missing_field")
	}
	if *parsed.Confidence < 0 || *parsed.Confidence > 1 {
		return nil, fmt.Errorf("invalid_range")
	}
	verdict := JunkVerdict(strings.ToLower(strings.TrimSpace(*parsed.Verdict)))
	switch verdict {
	case JunkVerdictJunk, JunkVerdictRealMangled, JunkVerdictRealAbsent, JunkVerdictUnsure:
	default:
		return nil, fmt.Errorf("invalid_verdict")
	}
	action := JunkPurgeActionKeep
	if verdict == JunkVerdictUnsure {
		action = JunkPurgeActionAbstain
	}
	if verdict == JunkVerdictJunk {
		action = JunkPurgeActionAbstain
		if *parsed.Confidence >= junkThreshold {
			action = JunkPurgeActionJunk
		}
	}
	return &JunkPurgeResult{
		Action: action, Verdict: verdict, Confidence: *parsed.Confidence,
	}, nil
}

func validateExactOutputSchema(task Task, raw []byte) error {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return fmt.Errorf("invalid_json: %w", err)
	}
	switch task {
	case TaskMatcherExtract:
		var parsed matcherExtractionJSON
		if err := decodeStrict(raw, &parsed); err != nil {
			return fmt.Errorf("invalid_json: %w", err)
		}
		if parsed.Title == nil || parsed.Year == nil || parsed.Type == nil ||
			parsed.Season == nil || parsed.Episode == nil ||
			parsed.IsAnime == nil || parsed.English == nil ||
			parsed.IsPack == nil || parsed.IsAdult == nil {
			return fmt.Errorf("missing_field")
		}
		switch MediaType(*parsed.Type) {
		case MediaTypeMovie, MediaTypeTV:
		default:
			return fmt.Errorf("invalid_type")
		}
		switch EnglishTrack(*parsed.English) {
		case EnglishTrackDub,
			EnglishTrackSub,
			EnglishTrackNone,
			EnglishTrackUnknown:
		default:
			return fmt.Errorf("invalid_english")
		}
		if *parsed.Year < 0 || *parsed.Season < 0 || *parsed.Episode < 0 {
			return fmt.Errorf("invalid_range")
		}
		return nil

	case TaskMatcherRerank:
		var parsed matcherRerankJSON
		if err := decodeStrict(raw, &parsed); err != nil {
			return fmt.Errorf("invalid_json: %w", err)
		}
		if parsed.TMDBID == nil || parsed.Confidence == nil {
			return fmt.Errorf("missing_field")
		}
		if *parsed.TMDBID < 0 ||
			*parsed.Confidence < 0 ||
			*parsed.Confidence > 1 {
			return fmt.Errorf("invalid_range")
		}
		return nil

	case TaskContentFilter:
		var parsed contentFilterJSON
		if err := decodeStrict(raw, &parsed); err != nil {
			return fmt.Errorf("invalid_json: %w", err)
		}
		if parsed.IsEnglish == nil ||
			parsed.Confidence == nil ||
			parsed.Reason == nil {
			return fmt.Errorf("missing_field")
		}
		if *parsed.Confidence < 0 || *parsed.Confidence > 1 {
			return fmt.Errorf("invalid_range")
		}
		return nil

	case TaskJunkPurge:
		var parsed junkPurgeJSON
		if err := decodeStrict(raw, &parsed); err != nil {
			return fmt.Errorf("invalid_json: %w", err)
		}
		if parsed.Verdict == nil || parsed.Confidence == nil {
			return fmt.Errorf("missing_field")
		}
		switch JunkVerdict(*parsed.Verdict) {
		case JunkVerdictJunk,
			JunkVerdictRealMangled,
			JunkVerdictRealAbsent,
			JunkVerdictUnsure:
		default:
			return fmt.Errorf("invalid_verdict")
		}
		if *parsed.Confidence < 0 || *parsed.Confidence > 1 {
			return fmt.Errorf("invalid_range")
		}
		return nil

	default:
		return fmt.Errorf("invalid_json: unsupported task %q", task)
	}
}

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple_json_values")
		}
		return err
	}
	return nil
}

func stripProductionWrappers(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<think>") {
		if end := strings.Index(text, "</think>"); end >= 0 {
			text = strings.TrimSpace(text[end+len("</think>"):])
		}
	}
	if strings.HasPrefix(text, "```") {
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		}
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	return text
}

// normalizeReasonTag prevents free-form model prose from entering canonical
// result artifacts. The reason is diagnostic only; task scoring uses the
// verdict and confidence. Keeping a short ASCII tag also makes prompt-injected
// output incapable of smuggling arbitrary text into the evaluation ledger.
func normalizeReasonTag(reason string) string {
	const maxReasonTagLength = 64

	var out strings.Builder
	lastSeparator := false
	for _, r := range strings.ToLower(strings.TrimSpace(reason)) {
		isASCIIAlphaNumeric := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if isASCIIAlphaNumeric {
			if out.Len() >= maxReasonTagLength {
				break
			}
			out.WriteRune(r)
			lastSeparator = false
			continue
		}
		if out.Len() > 0 && !lastSeparator && out.Len() < maxReasonTagLength {
			out.WriteByte('-')
			lastSeparator = true
		}
	}
	return strings.TrimRight(out.String(), "-")
}

func boundedParseCode(err error) string {
	message := err.Error()
	for _, code := range []string{
		"missing_case",
		"missing_effective_media_type",
		"candidate_not_offered",
		"missing_field",
		"invalid_type",
		"invalid_english",
		"invalid_verdict",
		"invalid_range",
		"invalid_json",
	} {
		if strings.HasPrefix(message, code) {
			return code
		}
	}
	return "schema_error"
}
