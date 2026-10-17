package processors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

type filterListConfig struct {
	BlockPatterns        []string `json:"block_patterns,omitempty"`
	AllowPatterns        []string `json:"allow_patterns,omitempty"`
	BlockedExtensions    []string `json:"blocked_extensions,omitempty"`
	BlockedReleaseGroups []string `json:"blocked_release_groups,omitempty"`
	MinSizeBytes         int64    `json:"min_size_bytes,omitempty"`
	MaxSizeBytes         int64    `json:"max_size_bytes,omitempty"`
	Reason               string   `json:"reason,omitempty"`
	TagOnly              bool     `json:"tag_only,omitempty"`
	Tags                 []string `json:"tags,omitempty"`
}

type regexWeightRule struct {
	ID              string   `json:"id"`
	Pattern         string   `json:"pattern"`
	Target          string   `json:"target,omitempty"` // name, files, any
	Dimension       string   `json:"dimension,omitempty"`
	Weight          float64  `json:"weight"`
	AddTags         []string `json:"add_tags,omitempty"`
	ContentType     string   `json:"content_type,omitempty"`
	VideoResolution string   `json:"video_resolution,omitempty"`
	VideoSource     string   `json:"video_source,omitempty"`
	VideoCodec      string   `json:"video_codec,omitempty"`
	ReleaseGroup    string   `json:"release_group,omitempty"`
}

type regexWeightsConfig struct {
	Rules            []regexWeightRule `json:"rules"`
	ExcludeDimension string            `json:"exclude_dimension,omitempty"`
	ExcludeThreshold *float64          `json:"exclude_threshold,omitempty"`
	ExcludeReason    string            `json:"exclude_reason,omitempty"`
}

type trackerLivenessConfig struct {
	MinSeeders      int     `json:"min_seeders,omitempty"`
	ExcludeDead     bool    `json:"exclude_dead,omitempty"`
	ExcludeReason   string  `json:"exclude_reason,omitempty"`
	TagLive         string  `json:"tag_live,omitempty"`
	TagDead         string  `json:"tag_dead,omitempty"`
	WeightDimension string  `json:"weight_dimension,omitempty"`
	LiveWeight      float64 `json:"live_weight,omitempty"`
	DeadWeight      float64 `json:"dead_weight,omitempty"`
}

type builtinLLMConfig struct {
	Model        string     `json:"model"`
	Endpoint     string     `json:"endpoint,omitempty"`
	SystemPrompt string     `json:"system_prompt,omitempty"`
	Temperature  *float64   `json:"temperature,omitempty"`
	Capabilities []v1.Stage `json:"capabilities,omitempty"`
}

type regexLearnedState struct {
	RuleAdjustments map[string]float64 `json:"rule_adjustments,omitempty"`
	FeedbackCount   int64              `json:"feedback_count,omitempty"`
}

func (s *Store) inspectBuiltin(_ context.Context, id string, w v1.RegistryWrite) (v1.Manifest, v1.ServiceStatus, error) {
	var m v1.Manifest
	var st v1.ServiceStatus
	if len(w.Configuration) == 0 || len(w.Configuration) > 4096 {
		return m, st, errors.New("invalid_configuration")
	}
	var rawMap map[string]any
	if err := v1.Decode(w.Configuration, &rawMap); err != nil {
		return m, st, err
	}
	if rawMap == nil || containsCredentials(rawMap) {
		return m, st, errors.New("configuration_contains_credentials")
	}
	cfgPolicy := "cfg-" + digest(canonical(w.Configuration))[:12]
	procID := v1.Identity{
		ID:             id,
		Version:        "1.0.0",
		ModelVersion:   "none",
		PolicyVersion:  cfgPolicy,
		ArtifactDigest: "none",
	}
	var caps []v1.Stage
	switch w.Driver {
	case v1.DriverBuiltinFilter:
		var cfg filterListConfig
		if err := v1.Decode(w.Configuration, &cfg); err != nil {
			return m, st, err
		}
		for _, pat := range append(append([]string{}, cfg.BlockPatterns...), cfg.AllowPatterns...) {
			if len(pat) == 0 || len(pat) > 512 {
				return m, st, errors.New("invalid_pattern")
			}
			if _, err := regexp.Compile(pat); err != nil {
				return m, st, errors.New("invalid_regex")
			}
		}
		caps = []v1.Stage{v1.Policy}
	case v1.DriverBuiltinRegex:
		var cfg regexWeightsConfig
		if err := v1.Decode(w.Configuration, &cfg); err != nil {
			return m, st, err
		}
		if len(cfg.Rules) == 0 || len(cfg.Rules) > 64 {
			return m, st, errors.New("invalid_rules")
		}
		hasClassify, hasExtract := false, false
		seenID := map[string]bool{}
		for _, r := range cfg.Rules {
			if r.ID == "" || len(r.ID) > 64 || seenID[r.ID] || len(r.Pattern) == 0 || len(r.Pattern) > 512 {
				return m, st, errors.New("invalid_rule")
			}
			seenID[r.ID] = true
			if _, err := regexp.Compile(r.Pattern); err != nil {
				return m, st, errors.New("invalid_regex")
			}
			if r.ContentType != "" {
				if !slices.Contains(v1.ValidContentTypes, r.ContentType) {
					return m, st, errors.New("invalid_content_type")
				}
				hasClassify = true
			}
			if r.VideoResolution != "" || r.VideoSource != "" || r.VideoCodec != "" || r.ReleaseGroup != "" {
				hasExtract = true
			}
		}
		if hasClassify {
			caps = append(caps, v1.Classify)
		}
		if hasExtract {
			caps = append(caps, v1.Extract)
		}
		caps = append(caps, v1.Policy)
	case v1.DriverBuiltinLiveness:
		var cfg trackerLivenessConfig
		if err := v1.Decode(w.Configuration, &cfg); err != nil {
			return m, st, err
		}
		caps = []v1.Stage{v1.Policy}
	case v1.DriverBuiltinLLM:
		var cfg builtinLLMConfig
		if err := v1.Decode(w.Configuration, &cfg); err != nil {
			return m, st, err
		}
		if cfg.Model == "" || len(cfg.Model) > 96 {
			return m, st, errors.New("invalid_model")
		}
		endpoint := w.Endpoint
		if endpoint == "" {
			endpoint = cfg.Endpoint
		}
		if err := validateEndpoint(endpoint, w.AuthEnv); err != nil {
			return m, st, err
		}
		procID.ModelVersion = sanitizeToken(cfg.Model)
		caps = cfg.Capabilities
		if len(caps) == 0 {
			caps = []v1.Stage{v1.Classify, v1.Extract, v1.Language, v1.Match}
		}
	default:
		return m, st, errors.New("unsupported_driver")
	}
	if w.ExpectedProcessor != nil {
		procID = *w.ExpectedProcessor
	}
	m = v1.Manifest{
		Contract:            v1.Contract,
		Processor:           procID,
		Capabilities:        caps,
		ConfigurationSchema: json.RawMessage(`{"type":"object"}`),
		Qualification:       "qualified",
	}
	if err := m.Validate(); err != nil {
		return m, st, err
	}
	st = v1.ServiceStatus{
		Contract:   v1.Contract,
		Processor:  procID,
		State:      "ready",
		InstanceID: "builtin:" + id,
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	return m, st, nil
}

func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '+' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), "._+-")
	if out == "" {
		return "model"
	}
	if len(out) > 95 {
		out = out[:95]
	}
	return out
}

func (s *Store) processBuiltin(ctx context.Context, entry Entry, j v1.Job) (v1.Result, error) {
	start := time.Now()
	var r v1.Result
	var err error
	switch entry.Driver {
	case v1.DriverBuiltinFilter:
		r, err = s.runFilterList(ctx, j)
	case v1.DriverBuiltinRegex:
		r, err = s.runRegexWeights(ctx, entry, j)
	case v1.DriverBuiltinLiveness:
		r, err = s.runTrackerLiveness(ctx, j)
	case v1.DriverBuiltinLLM:
		r, err = s.runBuiltinLLM(ctx, entry, j)
	default:
		return r, errors.New("unsupported_driver")
	}
	if err != nil {
		return r, err
	}
	ms := float64(time.Since(start).Microseconds()) / 1000.0
	r.Telemetry.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	r.Telemetry.InferenceMS = &ms
	r.FillDefaultBinding(j)
	return r, r.Validate(j)
}

func (s *Store) runFilterList(ctx context.Context, j v1.Job) (v1.Result, error) {
	var cfg filterListConfig
	if err := json.Unmarshal(j.Configuration, &cfg); err != nil {
		return v1.Result{}, err
	}
	for _, pat := range cfg.AllowPatterns {
		re, err := regexp.Compile(pat)
		if err == nil && re.MatchString(j.Input.Name) {
			return v1.Result{Binding: j.Binding, Outcome: v1.Abstained, Reasons: []string{"allow_pattern_matched"}}, nil
		}
	}
	var matchedReason string
	if cfg.MinSizeBytes > 0 && j.Input.Size > 0 && j.Input.Size < cfg.MinSizeBytes {
		matchedReason = "below_min_size"
	}
	if matchedReason == "" && cfg.MaxSizeBytes > 0 && j.Input.Size > cfg.MaxSizeBytes {
		matchedReason = "above_max_size"
	}
	if matchedReason == "" && len(cfg.BlockedExtensions) > 0 {
		checkExt := func(ext string) bool {
			n := normalizeExt(ext)
			for _, blocked := range cfg.BlockedExtensions {
				if n != "" && n == normalizeExt(blocked) {
					return true
				}
			}
			return false
		}
		if j.Input.Extension != nil && checkExt(*j.Input.Extension) {
			matchedReason = "blocked_extension"
		}
		for _, f := range j.Input.Files {
			if idx := strings.LastIndex(f.Path, "."); idx != -1 && checkExt(f.Path[idx+1:]) {
				matchedReason = "blocked_file_extension"
				break
			}
		}
	}
	if matchedReason == "" && len(cfg.BlockedReleaseGroups) > 0 {
		parsed, err := s.CoreParse(ctx, v1.CoreParseRequest{Name: j.Input.Name, Size: j.Input.Size, Files: j.Input.Files})
		if err == nil && parsed.Extraction.ReleaseGroup != "" {
			for _, grp := range cfg.BlockedReleaseGroups {
				if strings.EqualFold(parsed.Extraction.ReleaseGroup, strings.TrimSpace(grp)) {
					matchedReason = "blocked_release_group"
					break
				}
			}
		}
	}
	if matchedReason == "" {
		for _, pat := range cfg.BlockPatterns {
			re, err := regexp.Compile(pat)
			if err != nil {
				continue
			}
			if re.MatchString(j.Input.Name) {
				matchedReason = "block_pattern_matched"
				break
			}
			for _, f := range j.Input.Files {
				if re.MatchString(f.Path) {
					matchedReason = "block_file_pattern_matched"
					break
				}
			}
			if matchedReason != "" {
				break
			}
		}
	}
	if matchedReason == "" {
		return v1.Result{Binding: j.Binding, Outcome: v1.Abstained}, nil
	}
	reason := cfg.Reason
	if reason == "" {
		reason = matchedReason
	}
	if cfg.TagOnly && len(cfg.Tags) > 0 {
		return v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Completed,
			Proposal: &v1.Proposal{AddTags: cfg.Tags},
			Reasons:  []string{reason},
		}, nil
	}
	return v1.Result{
		Binding:  j.Binding,
		Outcome:  v1.Excluded,
		Proposal: &v1.Proposal{Exclusion: &v1.Exclusion{Scope: "publication", Reason: reason}},
		Reasons:  []string{reason},
	}, nil
}

func (s *Store) runRegexWeights(ctx context.Context, entry Entry, j v1.Job) (v1.Result, error) {
	var cfg regexWeightsConfig
	if err := json.Unmarshal(j.Configuration, &cfg); err != nil {
		return v1.Result{}, err
	}
	var learned regexLearnedState
	if p, err := s.pool.Get(); err == nil {
		var raw []byte
		if err := p.QueryRow(ctx, `SELECT learned_state FROM processor_registry WHERE id=$1`, entry.ID).Scan(&raw); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &learned)
		}
	}
	weights := map[string]float64{}
	var addTags []string
	seenTag := map[string]bool{}
	var matchedRules []string
	var contentType string
	var extraction *v1.Extraction

	for _, rule := range cfg.Rules {
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			continue
		}
		matched := false
		target := rule.Target
		if target == "" || target == "name" || target == "any" {
			matched = re.MatchString(j.Input.Name)
		}
		if !matched && (target == "files" || target == "any") {
			for _, f := range j.Input.Files {
				if re.MatchString(f.Path) {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		matchedRules = append(matchedRules, rule.ID)
		dim := rule.Dimension
		if dim == "" {
			dim = "score"
		}
		adj := 0.0
		if learned.RuleAdjustments != nil {
			adj = learned.RuleAdjustments[rule.ID]
		}
		w := rule.Weight + adj
		if w != 0 {
			weights[dim] += w
		}
		for _, t := range rule.AddTags {
			if !seenTag[t] {
				seenTag[t] = true
				addTags = append(addTags, t)
			}
		}
		if rule.ContentType != "" && contentType == "" {
			contentType = rule.ContentType
		}
		if rule.VideoResolution != "" || rule.VideoSource != "" || rule.VideoCodec != "" || rule.ReleaseGroup != "" {
			if extraction == nil {
				extraction = &v1.Extraction{}
			}
			if rule.VideoResolution != "" && extraction.VideoResolution == "" {
				extraction.VideoResolution = rule.VideoResolution
			}
			if rule.VideoSource != "" && extraction.VideoSource == "" {
				extraction.VideoSource = rule.VideoSource
			}
			if rule.VideoCodec != "" && extraction.VideoCodec == "" {
				extraction.VideoCodec = rule.VideoCodec
			}
			if rule.ReleaseGroup != "" && extraction.ReleaseGroup == "" {
				extraction.ReleaseGroup = rule.ReleaseGroup
			}
		}
	}
	if len(matchedRules) == 0 {
		return v1.Result{Binding: j.Binding, Outcome: v1.Abstained}, nil
	}
	traceBytes, _ := json.Marshal(map[string]any{"matched_rules": matchedRules})
	prop := &v1.Proposal{
		ContentType: contentType,
		Extraction:  extraction,
		AddTags:     addTags,
	}
	if len(weights) > 0 {
		prop.Weights = weights
	}
	outcome := v1.Completed
	if cfg.ExcludeDimension != "" && cfg.ExcludeThreshold != nil {
		total := weights[cfg.ExcludeDimension]
		if j.Input.AccumulatedScores != nil {
			total += j.Input.AccumulatedScores[cfg.ExcludeDimension]
		}
		if total >= *cfg.ExcludeThreshold {
			reason := cfg.ExcludeReason
			if reason == "" {
				reason = "regex_weight_threshold_exceeded"
			}
			prop.Exclusion = &v1.Exclusion{Scope: "publication", Reason: reason}
			outcome = v1.Excluded
		}
	}
	return v1.Result{
		Binding:  j.Binding,
		Outcome:  outcome,
		Proposal: prop,
		Reasons:  matchedRules[:min(8, len(matchedRules))],
		Trace:    traceBytes,
	}, nil
}

func (s *Store) runTrackerLiveness(ctx context.Context, j v1.Job) (v1.Result, error) {
	var cfg trackerLivenessConfig
	if err := json.Unmarshal(j.Configuration, &cfg); err != nil {
		return v1.Result{}, err
	}
	liveResp, err := s.CoreLiveness(ctx, v1.CoreLivenessRequest{InfoHash: j.Input.InfoHash})
	if err != nil {
		return v1.Result{}, err
	}
	liv := liveResp.Liveness
	seeders := 0
	hasSeeders := liv.Seeders != nil
	if hasSeeders {
		seeders = *liv.Seeders
	}
	isLive := liv.Observation == "alive" || liv.Observation == "live" || (hasSeeders && seeders >= max(1, cfg.MinSeeders))
	isDead := liv.Observation == "dead" || (hasSeeders && seeders == 0 && liv.Observation != "unknown")
	if !isLive && !isDead && !hasSeeders {
		return v1.Result{Binding: j.Binding, Outcome: v1.Abstained, Reasons: []string{"liveness_unknown"}}, nil
	}
	prop := &v1.Proposal{Liveness: &liv}
	dim := cfg.WeightDimension
	if dim == "" {
		dim = "liveness"
	}
	if isLive {
		if cfg.TagLive != "" {
			prop.AddTags = append(prop.AddTags, cfg.TagLive)
		}
		if cfg.LiveWeight != 0 {
			prop.Weights = map[string]float64{dim: cfg.LiveWeight}
		}
	} else if isDead {
		if cfg.TagDead != "" {
			prop.AddTags = append(prop.AddTags, cfg.TagDead)
		}
		if cfg.DeadWeight != 0 {
			prop.Weights = map[string]float64{dim: cfg.DeadWeight}
		}
		if cfg.ExcludeDead {
			reason := cfg.ExcludeReason
			if reason == "" {
				reason = "tracker_swarm_dead"
			}
			prop.Exclusion = &v1.Exclusion{Scope: "publication", Reason: reason}
			return v1.Result{
				Binding:  j.Binding,
				Outcome:  v1.Excluded,
				Proposal: prop,
				Reasons:  []string{reason},
			}, nil
		}
	}
	return v1.Result{
		Binding:  j.Binding,
		Outcome:  v1.Completed,
		Proposal: prop,
		Reasons:  []string{"liveness_" + liv.Observation},
	}, nil
}

func (s *Store) runBuiltinLLM(ctx context.Context, entry Entry, j v1.Job) (v1.Result, error) {
	var cfg builtinLLMConfig
	if err := json.Unmarshal(j.Configuration, &cfg); err != nil {
		return v1.Result{}, err
	}
	endpoint := entry.Endpoint
	if endpoint == "" {
		endpoint = cfg.Endpoint
	}
	sysPrompt := cfg.SystemPrompt
	if sysPrompt == "" {
		sysPrompt = `You are a BitAgent torrent metadata processor. Respond with a single JSON object containing {"outcome":"completed"|"abstained","proposal":{...},"uncertainty":0.0..1.0,"reasons":[...]}.`
	}
	userBytes, _ := json.Marshal(j.Input)
	temp := 0.0
	if cfg.Temperature != nil {
		temp = *cfg.Temperature
	}
	reqBody, _ := json.Marshal(map[string]any{
		"model":       cfg.Model,
		"temperature": temp,
		"response_format": map[string]string{
			"type": "json_object",
		},
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": string(userBytes)},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return v1.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if entry.AuthEnv != "" {
		secret := os.Getenv(entry.AuthEnv)
		if secret == "" && s.transport.CredentialDirectory != "" {
			if data, readErr := os.ReadFile(filepath.Join(s.transport.CredentialDirectory, entry.AuthEnv)); readErr == nil && len(data) <= 4096 {
				secret = strings.TrimSpace(string(data))
			}
		}
		if secret == "" {
			return v1.Result{}, errors.New("transport_credential_unavailable")
		}
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := s.transport.client.Do(req)
	if err != nil {
		return v1.Result{}, errors.New("transport_failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return v1.Result{}, errors.New("service_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, v1.MaxBody))
	if err != nil {
		return v1.Result{}, errors.New("response_read_failed")
	}
	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chatResp); err != nil || len(chatResp.Choices) == 0 {
		return v1.Result{}, errors.New("malformed_response")
	}
	var llmOut struct {
		Outcome     v1.Outcome   `json:"outcome"`
		Proposal    *v1.Proposal `json:"proposal,omitempty"`
		Uncertainty *float64     `json:"uncertainty,omitempty"`
		Reasons     []string     `json:"reasons,omitempty"`
	}
	if err := json.Unmarshal([]byte(chatResp.Choices[0].Message.Content), &llmOut); err != nil {
		return v1.Result{}, errors.New("malformed_response")
	}
	trace, _ := json.Marshal(map[string]any{"model": cfg.Model})
	return v1.Result{
		Binding:     j.Binding,
		Outcome:     llmOut.Outcome,
		Proposal:    llmOut.Proposal,
		Uncertainty: llmOut.Uncertainty,
		Reasons:     llmOut.Reasons,
		Trace:       trace,
	}, nil
}

func calibrateBuiltinRegex(rawState []byte, trace json.RawMessage, prediction *v1.Result, gt v1.FeedbackGroundTruth) ([]byte, bool) {
	if len(trace) == 0 {
		return rawState, false
	}
	var tr struct {
		MatchedRules []string `json:"matched_rules"`
	}
	if err := json.Unmarshal(trace, &tr); err != nil || len(tr.MatchedRules) == 0 {
		return rawState, false
	}
	reward := 0.0
	switch gt.Outcome {
	case "applied", "import_succeeded":
		reward = 1.0
	case "import_failed", "rejected_policy", "rejected_ambiguous":
		reward = -1.0
	case "teacher_consensus", "operator_override":
		if prediction != nil && prediction.Proposal != nil && gt.ContentType != "" && prediction.Proposal.ContentType != "" {
			if prediction.Proposal.ContentType == gt.ContentType {
				reward = 1.0
			} else {
				reward = -1.0
			}
		} else if gt.Excluded != nil && prediction != nil {
			predExcluded := prediction.Outcome == v1.Excluded
			if predExcluded == *gt.Excluded {
				reward = 1.0
			} else {
				reward = -1.0
			}
		} else {
			reward = 1.0
		}
	}
	if reward == 0 {
		return rawState, false
	}
	var state regexLearnedState
	if len(rawState) > 0 {
		_ = json.Unmarshal(rawState, &state)
	}
	if state.RuleAdjustments == nil {
		state.RuleAdjustments = map[string]float64{}
	}
	const stepSize = 0.05
	const maxAdjustment = 2.0
	for _, id := range tr.MatchedRules {
		next := state.RuleAdjustments[id] + stepSize*reward
		next = math.Max(-maxAdjustment, math.Min(maxAdjustment, math.Round(next*1000)/1000))
		state.RuleAdjustments[id] = next
	}
	state.FeedbackCount++
	out, err := json.Marshal(state)
	if err != nil {
		return rawState, false
	}
	return out, true
}
