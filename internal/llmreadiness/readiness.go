// Package llmreadiness prepares blinded, offline review packets and measures
// retained model decisions. It never calls providers or enables runtime modes.
package llmreadiness

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/spencercnorton/bitagent/internal/llmeval"
)

const maxInput = 16 << 20

type Record struct {
	CaptureKey     string          `json:"capture_key"`
	Task           string          `json:"task"`
	Source         string          `json:"source_sha256"`
	Group          string          `json:"group_sha256"`
	InputHash      string          `json:"input_sha256"`
	ContractHash   string          `json:"contract_sha256"`
	Model          string          `json:"model"`
	PromptVersion  string          `json:"prompt_version"`
	Build          string          `json:"build_identity"`
	Contract       string          `json:"contract_id"`
	ModelInput     json.RawMessage `json:"model_input"`
	TaskInput      json.RawMessage `json:"task_input"`
	CapturedAt     string          `json:"captured_at"`
	Privacy        string          `json:"privacy_status"`
	SamplingOrigin string          `json:"sampling_origin"`
	HTTPStatus     *int            `json:"http_status"`
	ErrorClass     *string         `json:"error_class"`
	ResponseBase64 *string         `json:"response_body_base64"`
	ResponseHash   *string         `json:"response_sha256"`
	Decision       json.RawMessage `json:"decision"`
	// Legacy mutable judgments are retained as context only, never scored.
	LegacyJudgment json.RawMessage `json:"mutable_latest_judgment"`
}

type Snapshot struct {
	Schema          string   `json:"schema"`
	SnapshotTime    string   `json:"snapshot_time"`
	ReadOnly        string   `json:"read_only"`
	Isolation       string   `json:"isolation"`
	Role            string   `json:"role"`
	PrivacyExcluded int      `json:"privacy_excluded"`
	Records         []Record `json:"records"`
}

type Freeze struct {
	Schema              string         `json:"schema"`
	SnapshotSHA256      string         `json:"snapshot_sha256"`
	ReviewerA           string         `json:"reviewer_a"`
	ReviewerB           string         `json:"reviewer_b"`
	Cases               int            `json:"cases"`
	Tasks               map[string]int `json:"tasks"`
	Scorable            map[string]int `json:"scorable"`
	IndependentLabels   bool           `json:"contains_independent_labels"`
	ProductionAuthority bool           `json:"grants_production_authority"`
}

// ReviewCase exposes only source inputs, with reviewer-specific opaque IDs.
// Model names, answers, confidence, group membership and policy state are absent.
type ReviewCase struct {
	CaseID      string `json:"case_id"`
	Task        string `json:"task"`
	Input       string `json:"input"`
	InputSHA256 string `json:"input_sha256"`
}

type Label struct {
	CaseID      string `json:"case_id"`
	Label       string `json:"label"`
	InputSHA256 string `json:"input_sha256"`
}

type Submission struct {
	Schema         string `json:"schema"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	ReviewerID     string `json:"reviewer_id"`
	// Kind describes supplied provenance; this offline tool cannot authenticate
	// a human identity or convert agent annotations into human gold.
	Kind   string  `json:"reviewer_kind"`
	Labels []Label `json:"labels"`
}

type Metrics struct {
	Cases          int `json:"cases"`
	Agreed         int `json:"agreed"`
	Disagreements  int `json:"disagreements"`
	Ambiguous      int `json:"ambiguous"`
	Scored         int `json:"scored"`
	Unscorable     int `json:"unscorable"`
	Actions        int `json:"actions"`
	PotentialHarms int `json:"potential_harms"`
	HarmGroups     int `json:"harm_groups"`
	ActionGroups   int `json:"action_groups"`
	// ConditionalActionRiskUpper95 measures potential error among action groups.
	// It is not the population keepworthy-loss rate used by promotion gates.
	HarmUpper95       *float64 `json:"conditional_action_risk_upper_95"`
	ECE               *float64 `json:"ece"`
	Brier             *float64 `json:"brier"`
	CalibrationGroups int      `json:"calibration_groups"`
}

type Report struct {
	Schema              string             `json:"schema"`
	SnapshotSHA256      string             `json:"snapshot_sha256"`
	ReviewerKinds       []string           `json:"reviewer_kinds"`
	Tasks               map[string]Metrics `json:"tasks"`
	ProductionAuthority bool               `json:"grants_production_authority"`
	Limitations         []string           `json:"limitations"`
}

func hash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

// DecodeStrict rejects duplicate keys, unknown fields, nonfinite JSON, trailing
// documents and oversized inputs before any artifacts are created.
func DecodeStrict(raw []byte, dst any) error {
	if len(raw) == 0 || len(raw) > maxInput {
		return fmt.Errorf("invalid bounded JSON size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := checkValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON schema")
	}
	return nil
}

func checkValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("JSON nesting limit exceeded")
	}
	t, err := d.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON")
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return fmt.Errorf("invalid JSON object")
				}
				s, ok := k.(string)
				s = foldJSONKey(s)
				if !ok || keys[s] {
					return fmt.Errorf("duplicate JSON key")
				}
				keys[s] = true
				if e = checkValue(d, depth+1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := checkValue(d, depth+1); e != nil {
					return e
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		if _, err = d.Token(); err != nil {
			return fmt.Errorf("invalid JSON delimiter")
		}
	}
	return nil
}

func foldJSONKey(s string) string {
	var out strings.Builder
	for _, r := range s {
		smallest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < smallest {
				smallest = next
			}
		}
		out.WriteRune(smallest)
	}
	return out.String()
}

func LoadSnapshot(raw []byte) (Snapshot, string, error) {
	var s Snapshot
	if err := DecodeStrict(raw, &s); err != nil {
		return s, "", err
	}
	if s.Schema != "bitagent-shadow-snapshot-v1" || s.ReadOnly != "on" || s.Isolation != "repeatable read" || len(s.Records) == 0 || len(s.Records) > 10000 {
		return s, "", fmt.Errorf("snapshot source contract failed")
	}
	seen := map[string]bool{}
	for _, r := range s.Records {
		if !validHash(r.CaptureKey) || !validHash(r.Group) || !validHash(r.Source) || !validHash(r.InputHash) || !validHash(r.ContractHash) || seen[r.CaptureKey] {
			return s, "", fmt.Errorf("invalid or duplicate capture identity")
		}
		seen[r.CaptureKey] = true
		if r.Privacy != "verified_native_public_qb_rechecked" || r.SamplingOrigin != "natural_capture" {
			return s, "", fmt.Errorf("snapshot admission contract failed")
		}
		model, err := canonicalObject(r.ModelInput)
		if err != nil {
			return s, "", err
		}
		task, err := canonicalObject(r.TaskInput)
		if err != nil {
			return s, "", err
		}
		if digestParts(model, task) != r.InputHash {
			return s, "", fmt.Errorf("request input hash mismatch")
		}
		if _, err := reviewInput(r); err != nil {
			return s, "", err
		}
		if r.ResponseBase64 != nil {
			b, e := base64.StdEncoding.DecodeString(*r.ResponseBase64)
			if e != nil || r.ResponseHash == nil || hash(b) != *r.ResponseHash {
				return s, "", fmt.Errorf("response hash mismatch")
			}
		}
	}
	return s, hash(raw), nil
}

func canonicalObject(raw []byte) ([]byte, error) {
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil || value == nil {
		return nil, fmt.Errorf("request must be a JSON object")
	}
	return json.Marshal(value)
}

func digestParts(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(p)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == hex.EncodeToString(b)
}

func reviewInput(r Record) (string, error) {
	var task map[string]json.RawMessage
	if err := json.Unmarshal(r.TaskInput, &task); err != nil {
		return "", fmt.Errorf("invalid task input")
	}
	key := ""
	switch r.Task {
	case "contentfilter":
		key = "title"
	case "junkpurge":
		key = "torrent_name"
	case "classifier_type":
		var model struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(r.ModelInput, &model); err != nil {
			return "", fmt.Errorf("invalid type model input")
		}
		for _, m := range model.Messages {
			if m.Role == "user" && m.Content != "" && len(m.Content) <= 8192 {
				// Omit only the trusted leading cohort directive. Source title,
				// file paths and size remain intact for independent classification.
				input := m.Content
				if prefix, tail, ok := strings.Cut(input, "\n"); ok && (strings.HasPrefix(prefix, "prompt_version=") || strings.HasPrefix(prefix, "prompt_version: ")) {
					input = tail
				}
				return input, nil
			}
		}
		return "", fmt.Errorf("type capture has no bounded user input")
	default:
		return "", fmt.Errorf("unsupported review task")
	}
	var input string
	if err := json.Unmarshal(task[key], &input); err != nil || input == "" || len(input) > 8192 {
		return "", fmt.Errorf("task has no bounded source input")
	}
	return input, nil
}

func caseID(snapshot, reviewer, key string) string {
	return hash([]byte(snapshot + "\x00" + reviewer + "\x00" + key))
}

func Prepare(s Snapshot, snapshot, a, b string) (Freeze, []ReviewCase, []ReviewCase, error) {
	f := Freeze{Schema: "bitagent-readiness-review-freeze-v1", SnapshotSHA256: snapshot, ReviewerA: a, ReviewerB: b, Tasks: map[string]int{}, Scorable: map[string]int{}}
	if !validHash(snapshot) || a == "" || b == "" || a == b || len(a) > 128 || len(b) > 128 {
		return f, nil, nil, fmt.Errorf("distinct bounded reviewers required")
	}
	var packets [2][]ReviewCase
	for _, r := range s.Records {
		input, err := reviewInput(r)
		if err != nil {
			return f, nil, nil, err
		}
		f.Cases++
		f.Tasks[r.Task]++
		if _, _, _, ok := observation(r); ok {
			f.Scorable[r.Task]++
		}
		for i, id := range []string{a, b} {
			packets[i] = append(packets[i], ReviewCase{CaseID: caseID(snapshot, id, r.CaptureKey), Task: r.Task, Input: input, InputSHA256: hash([]byte(input))})
		}
	}
	for i := range packets {
		sort.Slice(packets[i], func(a, b int) bool { return packets[i][a].CaseID < packets[i][b].CaseID })
	}
	return f, packets[0], packets[1], nil
}

func allowed(task, label string) bool {
	if label == "ambiguous" {
		return true
	}
	switch task {
	case "classifier_type":
		switch label {
		case "movie", "tv", "music", "book", "audiobook", "unknown":
			return true
		}
	case "contentfilter":
		switch label {
		case "english", "non_english", "uncertain":
			return true
		}
	case "junkpurge":
		_, err := llmeval.DeriveJunkDisposition(llmeval.JunkDispositionPolicyV1, llmeval.JunkContentClass(label))
		return err == nil
	}
	return false
}

func labelMap(s Snapshot, h string, sub Submission) (map[string]string, error) {
	if sub.Schema != "bitagent-readiness-labels-v1" || sub.SnapshotSHA256 != h || sub.ReviewerID == "" || len(sub.ReviewerID) > 128 || (sub.Kind != "human" && sub.Kind != "agent_diagnostic" && sub.Kind != "operator_reference") || len(sub.Labels) != len(s.Records) {
		return nil, fmt.Errorf("submission coverage or provenance mismatch")
	}
	wanted := map[string]Record{}
	for _, r := range s.Records {
		wanted[caseID(h, sub.ReviewerID, r.CaptureKey)] = r
	}
	labels := map[string]string{}
	for _, l := range sub.Labels {
		r, ok := wanted[l.CaseID]
		if !ok || labels[r.CaptureKey] != "" || !allowed(r.Task, l.Label) {
			return nil, fmt.Errorf("submission case identity or label mismatch")
		}
		input, _ := reviewInput(r)
		if l.InputSHA256 != hash([]byte(input)) {
			return nil, fmt.Errorf("review input hash mismatch")
		}
		labels[r.CaptureKey] = l.Label
	}
	return labels, nil
}

// observation requires a recorded first response and final decision. Legacy
// mutable judgments are intentionally insufficient for current-policy scoring.
func observation(r Record) (prediction string, confidence float64, action bool, ok bool) {
	if r.HTTPStatus == nil || *r.HTTPStatus != 200 || r.ErrorClass == nil || *r.ErrorClass != "none" || r.ResponseHash == nil || !validHash(*r.ResponseHash) || r.ResponseBase64 == nil || len(r.Decision) == 0 || bytes.Equal(r.Decision, []byte("null")) {
		return
	}
	var d struct {
		Outcome         string   `json:"outcome"`
		Category        string   `json:"category"`
		Confidence      *float64 `json:"confidence"`
		English         *bool    `json:"is_english"`
		WouldApply      *bool    `json:"would_apply"`
		WouldDrop       *bool    `json:"would_drop"`
		Verdict         string   `json:"verdict"`
		WouldQuarantine *bool    `json:"would_quarantine"`
		MinConfidence   *float64 `json:"min_confidence"`
		Live            *bool    `json:"live"`
		Reason          string   `json:"reason"`
		RequestIndex    *int     `json:"request_index"`
		RequestSize     *int     `json:"request_size"`
	}
	if DecodeStrict(r.Decision, &d) != nil || d.Confidence == nil || math.IsNaN(*d.Confidence) || math.IsInf(*d.Confidence, 0) || *d.Confidence < 0 || *d.Confidence > 1 || d.MinConfidence == nil || *d.MinConfidence <= 0 || *d.MinConfidence > 1 || d.Live == nil {
		return
	}
	confidence = *d.Confidence
	var policy struct {
		MinConfidence *float64 `json:"min_confidence"`
		Live          *bool    `json:"live"`
		RequestIndex  *int     `json:"request_index"`
		RequestSize   *int     `json:"request_size"`
	}
	if json.Unmarshal(r.TaskInput, &policy) != nil || policy.MinConfidence == nil || policy.Live == nil || *policy.MinConfidence != *d.MinConfidence || *policy.Live != *d.Live {
		return "", 0, false, false
	}
	switch r.Task {
	case "classifier_type":
		raw, err := base64.StdEncoding.DecodeString(*r.ResponseBase64)
		if err != nil {
			return "", 0, false, false
		}
		answer, err := llmstage.EvaluationParseResponse(raw)
		if err != nil || string(answer.MediaType) != d.Category || answer.Confidence != confidence {
			return "", 0, false, false
		}
		if !allowed(r.Task, d.Category) || d.Category == "ambiguous" || d.WouldApply == nil {
			return "", 0, false, false
		}
		if d.Outcome != "classified" && d.Outcome != "low_confidence" && d.Outcome != "unknown" {
			return "", 0, false, false
		}
		if *d.WouldApply != (d.Outcome == "classified") || *d.WouldApply && (confidence < *d.MinConfidence || d.Category == "unknown") {
			return "", 0, false, false
		}
		prediction = d.Category
		action = *d.WouldApply
	case "contentfilter":
		raw, err := base64.StdEncoding.DecodeString(*r.ResponseBase64)
		if err != nil {
			return "", 0, false, false
		}
		answer, err := contentfilter.EvaluationParseHTTPVerdict(raw, r.Contract)
		if err != nil || d.English == nil || answer.IsEnglish != *d.English || answer.Confidence != confidence || answer.Reason != d.Reason {
			return "", 0, false, false
		}
		if d.English == nil || d.WouldDrop == nil || d.Reason == "" || (d.Outcome != "english" && d.Outcome != "non_english" && d.Outcome != "low_confidence") {
			return "", 0, false, false
		}
		if *d.WouldDrop != (d.Outcome == "non_english") || *d.English != (d.Outcome == "english") || *d.WouldDrop && confidence < *d.MinConfidence {
			return "", 0, false, false
		}
		prediction = "non_english"
		if *d.English {
			prediction = "english"
		}
		action = *d.WouldDrop
	case "junkpurge":
		// Only the new exact grouped request contract has response identity.
		if r.Contract != "junkpurge-sync-chat-request-v3-audited" {
			return "", 0, false, false
		}
		if d.WouldQuarantine == nil {
			return "", 0, false, false
		}
		if d.Outcome != "judged" || d.RequestIndex == nil || d.RequestSize == nil || policy.RequestIndex == nil || policy.RequestSize == nil || *d.RequestIndex != *policy.RequestIndex || *d.RequestSize != *policy.RequestSize || *d.RequestIndex < 1 || *d.RequestIndex > *d.RequestSize || *d.RequestSize > 50 {
			return "", 0, false, false
		}
		raw, err := base64.StdEncoding.DecodeString(*r.ResponseBase64)
		if err != nil {
			return "", 0, false, false
		}
		answers, err := junkpurge.EvaluationParseHTTPJudgments(raw, *d.RequestSize)
		if err != nil || len(answers) != *d.RequestSize || answers[*d.RequestIndex-1].Verdict != d.Verdict || answers[*d.RequestIndex-1].Confidence != confidence {
			return "", 0, false, false
		}
		switch d.Verdict {
		case "junk":
			prediction = "delete"
		case "real_mangled", "real_absent":
			prediction = "keep"
		case "unsure":
			prediction = "abstain"
		default:
			return "", 0, false, false
		}
		action = *d.WouldQuarantine
		if action && (prediction != "delete" || confidence < *d.MinConfidence) {
			return "", 0, false, false
		}
	default:
		return
	}
	ok = prediction != ""
	return
}

// BinomialUpper95 is an exact one-sided Clopper-Pearson bound. Independent
// release-family groups, not duplicate source rows, form its denominator.
func BinomialUpper95(k, n int) *float64 {
	if n <= 0 || k < 0 || k > n {
		return nil
	}
	if k == n {
		x := 1.0
		return &x
	}
	if k == 0 {
		x := -math.Expm1(math.Log(0.05) / float64(n))
		return &x
	}
	lo, hi := float64(k)/float64(n), 1.0
	for range 70 {
		p := (lo + hi) / 2
		sum := 0.0
		for i := 0; i <= k; i++ {
			a, _ := math.Lgamma(float64(n + 1))
			b, _ := math.Lgamma(float64(i + 1))
			c, _ := math.Lgamma(float64(n - i + 1))
			sum += math.Exp(a - b - c + float64(i)*math.Log(p) + float64(n-i)*math.Log1p(-p))
		}
		if sum > 0.05 {
			lo = p
		} else {
			hi = p
		}
	}
	x := (lo + hi) / 2
	return &x
}

func bucketKey(r Record) string {
	// Contract digest binds endpoint, system prompt and build; isolate captured
	// task policy too, because those values may change under one contract ID.
	var policy map[string]json.RawMessage
	_ = json.Unmarshal(r.TaskInput, &policy)
	policyJSON, _ := json.Marshal(map[string]json.RawMessage{"min_confidence": policy["min_confidence"], "live": policy["live"], "openai_data_sharing": policy["openai_data_sharing"]})
	return r.Task + "|" + r.Model + "|" + r.Build + "|" + r.Contract + "|" + r.PromptVersion + "|" + r.ContractHash + "|" + hash(policyJSON)
}

// statisticalGroups joins release families sharing a grouped junk request.
// Connected components also handle a family seen in multiple requests, rather
// than claiming fifty correlated answers are fifty independent observations.
func statisticalGroups(s Snapshot) map[string]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" {
			parent[x] = x
		}
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	join := func(a, b string) {
		a, b = find(a), find(b)
		if a < b {
			parent[b] = a
		} else {
			parent[a] = b
		}
	}
	for _, r := range s.Records {
		family := bucketKey(r) + "|family|" + r.Group
		find(family)
		if r.Task == "junkpurge" {
			body, _ := canonicalObject(r.ModelInput)
			join(family, bucketKey(r)+"|request|"+hash(body))
		}
	}
	out := map[string]string{}
	for _, r := range s.Records {
		out[r.CaptureKey] = find(bucketKey(r) + "|family|" + r.Group)
	}
	return out
}

func Score(s Snapshot, h string, a, b Submission) (Report, error) {
	report := Report{Schema: "bitagent-readiness-diagnostic-report-v1", SnapshotSHA256: h, ReviewerKinds: []string{a.Kind, b.Kind}, Tasks: map[string]Metrics{}, Limitations: []string{"This report does not authenticate reviewers or replace formal gold closure, task-specific harm gates, safety strata, holdout or prospective shadow acceptance.", "Disagreements and ambiguous actions count as potential harm; they never become gold labels.", "Current-model observations are grouped by exact model, build and request contract; statistics must not be pooled across versions."}}
	if a.ReviewerID == b.ReviewerID {
		return report, fmt.Errorf("independent reviewers required")
	}
	la, err := labelMap(s, h, a)
	if err != nil {
		return report, err
	}
	lb, err := labelMap(s, h, b)
	if err != nil {
		return report, err
	}
	type bucket struct {
		n                   int
		confidence, correct float64
	}
	type acc struct {
		m            Metrics
		action, harm map[string]bool
		cal          map[string]struct {
			confidence float64
			correct    bool
		}
		bins  [10]bucket
		brier float64
	}
	by := map[string]*acc{}
	groups := statisticalGroups(s)
	for _, r := range s.Records {
		key := bucketKey(r)
		group := groups[r.CaptureKey]
		v := by[key]
		if v == nil {
			v = &acc{action: map[string]bool{}, harm: map[string]bool{}, cal: map[string]struct {
				confidence float64
				correct    bool
			}{}}
			by[key] = v
		}
		v.m.Cases++
		x, y := la[r.CaptureKey], lb[r.CaptureKey]
		agreed := x == y
		if !agreed {
			v.m.Disagreements++
		} else {
			v.m.Agreed++
		}
		ambiguous := x == "ambiguous" || y == "ambiguous" || r.Task == "contentfilter" && (x == "uncertain" || y == "uncertain")
		if ambiguous {
			v.m.Ambiguous++
		}
		prediction, confidence, action, ok := observation(r)
		if !ok {
			v.m.Unscorable++
			continue
		}
		if action {
			v.m.Actions++
			v.action[group] = true
		}
		if !agreed || ambiguous {
			if action {
				v.m.PotentialHarms++
				v.harm[group] = true
			}
			continue
		}
		truth := x
		if r.Task == "junkpurge" {
			d, e := llmeval.DeriveJunkDisposition(llmeval.JunkDispositionPolicyV1, llmeval.JunkContentClass(x))
			if e != nil {
				return report, e
			}
			truth = string(d)
		}
		correct := prediction == truth
		v.m.Scored++
		if action && !correct {
			v.m.PotentialHarms++
			v.harm[group] = true
		}
		// The worst confidence/error observation wins within a release family,
		// preventing repeated easy titles from hiding a hard wrong decision.
		old, exists := v.cal[group]
		if !exists || (old.correct && !correct) || old.correct == correct && confidence > old.confidence {
			v.cal[group] = struct {
				confidence float64
				correct    bool
			}{confidence, correct}
		}
	}
	for key, v := range by {
		v.m.ActionGroups = len(v.action)
		v.m.HarmGroups = len(v.harm)
		v.m.HarmUpper95 = BinomialUpper95(v.m.HarmGroups, v.m.ActionGroups)
		for _, x := range v.cal {
			correct := 0.0
			if x.correct {
				correct = 1
			}
			i := min(9, int(x.confidence*10))
			v.bins[i].n++
			v.bins[i].confidence += x.confidence
			v.bins[i].correct += correct
			v.brier += (x.confidence - correct) * (x.confidence - correct)
		}
		v.m.CalibrationGroups = len(v.cal)
		if v.m.CalibrationGroups > 0 {
			n := float64(v.m.CalibrationGroups)
			ece := 0.0
			for _, b := range v.bins {
				if b.n > 0 {
					ece += math.Abs(b.confidence-b.correct) / n
				}
			}
			brier := v.brier / n
			v.m.ECE = &ece
			v.m.Brier = &brier
		}
		report.Tasks[key] = v.m
	}
	return report, nil
}
