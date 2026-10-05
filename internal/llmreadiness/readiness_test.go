package llmreadiness

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func syntheticSnapshot(t *testing.T) (Snapshot, string, []byte) {
	t.Helper()
	status := 200
	class := "none"
	body := []byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"category\":\"movie\",\"confidence\":0.9}"}}]}`)
	response := base64.StdEncoding.EncodeToString(body)
	responseHash := hash(body)
	model := json.RawMessage(`{"messages":[{"role":"user","content":"Example.Movie.2020.1080p"}]}`)
	task := json.RawMessage(`{"min_confidence":0.75,"live":false}`)
	m, _ := canonicalObject(model)
	p, _ := canonicalObject(task)
	r := Record{CaptureKey: hash([]byte("capture")), Task: "classifier_type", Source: hash([]byte("source")), Group: hash([]byte("family")), InputHash: digestParts(m, p), ContractHash: hash([]byte("contract")), Model: "fixture-model", PromptVersion: "v1", Build: "fixture-build", Contract: "type-fixture", ModelInput: model, TaskInput: task, Privacy: "verified_native_public_qb_rechecked", SamplingOrigin: "natural_capture", HTTPStatus: &status, ErrorClass: &class, ResponseBase64: &response, ResponseHash: &responseHash, Decision: json.RawMessage(`{"outcome":"classified","category":"movie","confidence":0.9,"min_confidence":0.75,"would_apply":true,"live":false}`)}
	s := Snapshot{Schema: "bitagent-shadow-snapshot-v1", ReadOnly: "on", Isolation: "repeatable read", Records: []Record{r}}
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	return s, hash(raw), raw
}

func submissions(t *testing.T, s Snapshot, h, kind, labelA, labelB string) (Submission, Submission) {
	t.Helper()
	f, a, b, err := Prepare(s, h, "alice", "bob")
	require.NoError(t, err)
	require.False(t, f.ProductionAuthority)
	makeSub := func(id, label string, cases []ReviewCase) Submission {
		sub := Submission{Schema: "bitagent-readiness-labels-v1", SnapshotSHA256: h, ReviewerID: id, Kind: kind}
		for _, c := range cases {
			sub.Labels = append(sub.Labels, Label{CaseID: c.CaseID, Label: label, InputSHA256: c.InputSHA256})
		}
		return sub
	}
	return makeSub("alice", labelA, a), makeSub("bob", labelB, b)
}

func TestSnapshotAndBlindedPacketIntegrity(t *testing.T) {
	s, h, raw := syntheticSnapshot(t)
	loaded, got, err := LoadSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, h, got)
	require.Equal(t, s.Records[0].InputHash, loaded.Records[0].InputHash)
	_, a, b, err := Prepare(s, h, "a", "b")
	require.NoError(t, err)
	require.NotEqual(t, a[0].CaseID, b[0].CaseID)
	packet, _ := json.Marshal(a)
	for _, secret := range []string{"fixture-model", "fixture-build", "confidence", "would_apply", "family"} {
		require.NotContains(t, string(packet), secret)
	}
	s.Records[0].TaskInput = json.RawMessage(`{"min_confidence":0.1,"live":true}`)
	raw, _ = json.Marshal(s)
	_, _, err = LoadSnapshot(raw)
	require.ErrorContains(t, err, "input hash mismatch")
}

func TestStrictInputsRejectDuplicateAndTrailingKeys(t *testing.T) {
	var s Snapshot
	for _, raw := range []string{`{"schema":"a","schema":"b"}`, `{"schema":"a","Schema":"b"}`, `{} {}`, `{"unknown":1}`, `{"records":[{"task_input":{"live":true,"live":false}}]}`} {
		require.Error(t, DecodeStrict([]byte(raw), &s))
	}
	require.Error(t, DecodeStrict([]byte(strings.Repeat("[", 35)+strings.Repeat("]", 35)), &s))
}

func TestGroupedRequestsJoinConnectedReleaseFamilies(t *testing.T) {
	s, _, _ := syntheticSnapshot(t)
	r := s.Records[0]
	r.Task = "junkpurge"
	r.Group = hash([]byte("family-a"))
	r.ModelInput = json.RawMessage(`{"names":["A","B"]}`)
	s.Records = []Record{r}
	r.CaptureKey = hash([]byte("grouped-b"))
	r.Group = hash([]byte("family-b"))
	s.Records = append(s.Records, r)
	r.CaptureKey = hash([]byte("bridge-b"))
	r.ModelInput = json.RawMessage(`{"names":["B","C"]}`)
	s.Records = append(s.Records, r)
	r.CaptureKey = hash([]byte("grouped-c"))
	r.Group = hash([]byte("family-c"))
	s.Records = append(s.Records, r)
	groups := statisticalGroups(s)
	for _, r := range s.Records {
		require.Equal(t, groups[s.Records[0].CaptureKey], groups[r.CaptureKey], "shared requests and repeated families must be one cluster")
	}
}

func TestResponseAndDecisionMustBeComplete(t *testing.T) {
	s, _, _ := syntheticSnapshot(t)
	_, _, _, ok := observation(s.Records[0])
	require.True(t, ok)
	r := s.Records[0]
	r.ResponseBase64 = nil
	_, _, _, ok = observation(r)
	require.False(t, ok)
	r = s.Records[0]
	r.Decision = json.RawMessage(`{"category":"movie","confidence":0.9,"would_apply":true}`)
	_, _, _, ok = observation(r)
	require.False(t, ok)
	r = s.Records[0]
	r.Task = "contentfilter"
	r.Decision = json.RawMessage(`{"outcome":"invalid_response","is_english":false,"confidence":0,"reason":"","min_confidence":0.9,"would_drop":false,"live":false}`)
	_, _, _, ok = observation(r)
	require.False(t, ok)
	r = s.Records[0]
	r.Task = "junkpurge"
	r.Contract = "junkpurge-sync-chat-request-v2-openai-data-sharing"
	r.Decision = json.RawMessage(`{"verdict":"junk","confidence":0.9,"min_confidence":0.9,"would_quarantine":true,"live":false}`)
	_, _, _, ok = observation(r)
	require.False(t, ok)
}

func TestDisagreementCountsPotentialHarmAndDuplicateGroupsDoNotInflate(t *testing.T) {
	s, h, _ := syntheticSnapshot(t)
	second := s.Records[0]
	second.CaptureKey = hash([]byte("second"))
	s.Records = append(s.Records, second)
	a, b := submissions(t, s, h, "agent_diagnostic", "movie", "tv")
	report, err := Score(s, h, a, b)
	require.NoError(t, err)
	require.False(t, report.ProductionAuthority)
	for _, m := range report.Tasks {
		require.Equal(t, 2, m.PotentialHarms)
		require.Equal(t, 1, m.HarmGroups)
		require.Equal(t, 1, m.ActionGroups)
		require.Equal(t, 0, m.CalibrationGroups)
		require.Equal(t, 1.0, *m.HarmUpper95)
	}
	b.ReviewerID = a.ReviewerID
	_, err = Score(s, h, a, b)
	require.Error(t, err)
}

func TestPromptContractAndPolicyAreNeverPooled(t *testing.T) {
	s, h, _ := syntheticSnapshot(t)
	for _, mutation := range []func(*Record){func(r *Record) { r.PromptVersion = "v2" }, func(r *Record) { r.ContractHash = hash([]byte("other-endpoint")) }, func(r *Record) { r.TaskInput = json.RawMessage(`{"min_confidence":0.8,"live":false}`) }} {
		r := s.Records[0]
		r.CaptureKey = hash([]byte(string(rune(len(s.Records)))))
		mutation(&r)
		s.Records = append(s.Records, r)
	}
	a, b := submissions(t, s, h, "human", "movie", "movie")
	report, err := Score(s, h, a, b)
	require.NoError(t, err)
	require.Len(t, report.Tasks, 4)
	require.False(t, report.ProductionAuthority, "self-reported human identity grants no authority")
	a.Labels[0].InputSHA256 = hash([]byte("tampered"))
	_, err = Score(s, h, a, b)
	require.ErrorContains(t, err, "input hash mismatch")
}

func TestExactBinomialBound(t *testing.T) {
	require.Nil(t, BinomialUpper95(0, 0))
	require.Nil(t, BinomialUpper95(2, 1))
	require.InDelta(t, 1-math.Pow(.05, 1.0/1000), *BinomialUpper95(0, 1000), 1e-12)
	require.InDelta(t, .3941633024365, *BinomialUpper95(1, 10), 1e-10)
}
