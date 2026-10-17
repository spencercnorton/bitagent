package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishedFixtures(t *testing.T) {
	read := func(name string, out any) {
		t.Helper()
		b, e := os.ReadFile(filepath.Join("..", "..", "docs", "processors", "fixtures", name))
		require.NoError(t, e)
		require.NoError(t, Decode(b, out))
	}
	var m Manifest
	read("manifest.json", &m)
	require.NoError(t, m.Validate())
	var j Job
	read("job.json", &j)
	require.NoError(t, j.Validate())
	var r Result
	read("result.json", &r)
	require.NoError(t, r.Validate(j))
	var s ServiceStatus
	read("service-status.json", &s)
	require.NoError(t, s.Validate())
	var status Status
	read("operator-status.json", &status)
	require.Equal(t, StatusContract, status.Contract)
	var page JobPage
	read("operator-jobs.json", &page)
	require.NotEmpty(t, page.Items)
	var admission AdmissionPolicy
	read("admission.json", &admission)
	require.Equal(t, AdmissionContract, admission.Contract)
	require.Equal(t, AdmissionAlgorithm, admission.Algorithm)
	require.Equal(t, 100, admission.BasisPoints)
	var export JobExport
	read("job-export.json", &export)
	require.Equal(t, JobExportContract, export.Contract)
	require.Equal(t, "not_received_by_core", export.Coverage["raw_provider_payloads"])
	var capabilities OperationsCapabilities
	read("operations-capabilities.json", &capabilities)
	require.Equal(t, OperationsContract, capabilities.Contract)
	require.Equal(t, ReprocessingContract, capabilities.Features["reprocessing"])
	var reprocessing ReprocessStatus
	read("reprocessing.json", &reprocessing)
	require.Equal(t, ReprocessingContract, reprocessing.Contract)
	require.Nil(t, reprocessing.ResultJobID)
	var request ReprocessWrite
	read("reprocessing-write.json", &request)
	require.Equal(t, request.RequestID, reprocessing.RequestID)
	for _, outcome := range []Outcome{Abstained, Deferred, Failed} {
		copy := r
		copy.Outcome = outcome
		copy.Proposal = nil
		require.NoError(t, copy.Validate(j))
	}
	copy := r
	copy.Outcome = Unmatched
	copy.Proposal = nil
	require.ErrorContains(t, copy.Validate(j), "unproven_complete_miss")
	// A normal processing receipt does not fabricate a matching receipt.
	j.Stage = Match
	r.Binding = j.Binding
	r.Outcome = Unmatched
	r.Proposal = nil
	require.Error(t, r.Validate(j))
	j.Input.Deterministic.MatchingOutcome = "completed_unmatched"
	require.NoError(t, r.Validate(j))
	r.Processor.ModelVersion = "changed"
	require.ErrorContains(t, r.Validate(j), "binding_mismatch")
}
func TestStageProposalRules(t *testing.T) {
	raw, e := os.ReadFile(filepath.Join("..", "..", "docs", "processors", "fixtures", "job.json"))
	require.NoError(t, e)
	var j Job
	require.NoError(t, json.Unmarshal(raw, &j))
	for _, stage := range Stages {
		t.Run(string(stage), func(t *testing.T) {
			job := j
			job.Stage = stage
			r := Result{Binding: job.Binding, Outcome: Completed, Proposal: &Proposal{ContentType: "movie"}}
			if stage == Classify {
				require.NoError(t, r.Validate(job))
			} else {
				require.Error(t, r.Validate(job))
			}
		})
	}
	j.Stage = Policy
	r := Result{Binding: j.Binding, Outcome: Excluded, Proposal: &Proposal{Exclusion: &Exclusion{Scope: "publication", Reason: "synthetic"}}}
	require.NoError(t, r.Validate(j))
	r.Proposal.Exclusion.Scope = "delete"
	require.Error(t, r.Validate(j))
}
func TestDuplicateAndUnknownFields(t *testing.T) {
	for _, raw := range []string{`{"contract":"one","contract":"two"}`, `{"contract":"x","unexpected":1}`, `{} {}`} {
		var s ServiceStatus
		require.Error(t, Decode([]byte(raw), &s))
	}
	var m Manifest
	require.Error(t, Decode([]byte(`{"configuration_schema":{"type":"object","type":"string"}}`), &m))
}

func TestStackingFeedbackAndCoreOpsValidation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "processors", "fixtures", "job.json"))
	require.NoError(t, err)
	var j Job
	require.NoError(t, json.Unmarshal(raw, &j))
	j.Capabilities = []Stage{Classify, Extract, Language, Match, Policy}
	j.Input.Candidates = []Candidate{{MatchRef: MatchRef{Source: "tmdb", ID: "991051"}, ContentType: "movie", Title: "Synthetic Amber", Year: 2024}}
	require.NoError(t, j.Validate())

	seeders := 12
	stateless := Result{
		Outcome: Completed,
		Proposal: &Proposal{
			ContentType: "movie",
			Extraction:  &Extraction{Title: "Synthetic Amber", Year: 2024, VideoResolution: "1080p", ReleaseGroup: "GROUP"},
			Language:    &LanguageClaim{Audio: []string{"en"}, Subtitles: []string{"en"}, Evidence: "metadata_claim"},
			Match:       &MatchRef{Source: "tmdb", ID: "991051"},
			Liveness:    &LivenessClaim{Seeders: &seeders, Observation: "live", Source: "tracker"},
			AddTags:     []string{"remux-verified"},
			Weights:     map[string]float64{"quality": 1.5},
		},
		RequestFeedback: true,
	}
	stateless.FillDefaultBinding(j)
	require.Equal(t, j.Binding, stateless.Binding)
	require.NoError(t, stateless.Validate(j))

	u := 0.5
	bps := 5000
	minScore := 1.0
	sel := &Selector{
		ContentTypes:      []string{"movie", "tv_show"},
		ExcludeTypes:      []string{"xxx"},
		Extensions:        []string{".mkv", "mp4"},
		MinUncertainty:    &u,
		RequiredTags:      []string{"hdr-candidate"},
		ScoreThresholds:   []ScoreThreshold{{Dimension: "quality", Min: &minScore}},
		SampleBasisPoints: &bps,
	}
	require.False(t, sel.IsEmpty())
	require.NoError(t, sel.Validate())

	badSel := &Selector{ContentTypes: []string{"not_a_type"}}
	require.Error(t, badSel.Validate())

	lc := &LearningConfig{
		Enabled:       true,
		Mode:          "online_calibrate",
		AcceptedTiers: []string{FeedbackTierOperationalReceipt, FeedbackTierVerifiedImport},
		PushEnabled:   true,
	}
	require.NoError(t, lc.Validate())
	require.True(t, lc.Accepts(FeedbackTierVerifiedImport))
	require.False(t, lc.Accepts(FeedbackTierHumanGold))

	var m Manifest
	m.Contract = Contract
	m.Processor.ID = "minimal-webhook"
	m.Capabilities = []Stage{Classify}
	m.ApplyDefaults("minimal-webhook")
	require.NoError(t, m.Validate())
}

func TestValidateInputSourcesAndSwarmPrivacyAndBackwardsCompat(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "processors", "fixtures", "job.json"))
	require.NoError(t, err)
	var j Job
	require.NoError(t, json.Unmarshal(raw, &j))
	require.NoError(t, j.Validate())

	seeds := 42
	j.Input.Sources = []SourceObservation{{Source: "tracker", Seeders: &seeds}}
	j.Input.Swarm = &SwarmInput{
		TrackerScraped: true,
		TrackerKnown:   true,
		TrackerSeeders: &seeds,
		BestTracker:    "udp://open.stealth.si:80/announce",
		LastPositiveAt: "2026-10-17T00:00:00Z",
		LivenessStatus: "alive",
		OutcomePriors:  []PriorObservation{{KeyType: "release_group", KeyValue: "edith", Alpha: 369, Beta: 24}},
	}
	require.NoError(t, j.Validate())

	jPriv := j
	jPriv.Input.Sources = []SourceObservation{{Source: "PassThePopcorn"}}
	require.ErrorContains(t, jPriv.Validate(), "private_or_invalid_source")

	jPasskey := j
	jPasskey.Input.Sources = nil
	jPasskey.Input.Swarm = &SwarmInput{BestTracker: "https://tracker.example.org/announce?passkey=secret"}
	require.ErrorContains(t, jPasskey.Validate(), "private_or_invalid_tracker")
}

func TestValidateInputSwarmBoundsAndPriors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "processors", "fixtures", "job.json"))
	require.NoError(t, err)
	base := func() Job {
		var j Job
		require.NoError(t, json.Unmarshal(raw, &j))
		return j
	}
	neg := -1
	huge := 100000001

	jNeg := base()
	jNeg.Input.Sources = []SourceObservation{{Source: "dht", Seeders: &neg}}
	require.ErrorContains(t, jNeg.Validate(), "invalid_source_counts")

	jHuge := base()
	jHuge.Input.Swarm = &SwarmInput{TrackerSeeders: &huge}
	require.ErrorContains(t, jHuge.Validate(), "invalid_swarm_counts")

	jMany := base()
	for range 33 {
		jMany.Input.Sources = append(jMany.Input.Sources, SourceObservation{Source: "dht"})
	}
	require.ErrorContains(t, jMany.Validate(), "invalid_sources")

	jTime := base()
	jTime.Input.Swarm = &SwarmInput{LastPositiveAt: "not-a-time"}
	require.ErrorContains(t, jTime.Validate(), "invalid_swarm_timestamp")

	jStatus := base()
	jStatus.Input.Swarm = &SwarmInput{LivenessStatus: "zombie"}
	require.ErrorContains(t, jStatus.Validate(), "invalid_liveness_status")

	jPriorType := base()
	jPriorType.Input.Swarm = &SwarmInput{OutcomePriors: []PriorObservation{{KeyType: "indexer_host", KeyValue: "example", Alpha: 2, Beta: 2}}}
	require.ErrorContains(t, jPriorType.Validate(), "invalid_outcome_prior")

	jPriorAlpha := base()
	jPriorAlpha.Input.Swarm = &SwarmInput{OutcomePriors: []PriorObservation{{KeyType: "codec", KeyValue: "x264", Alpha: 0, Beta: 2}}}
	require.ErrorContains(t, jPriorAlpha.Validate(), "invalid_outcome_prior")

	jPriorPrivate := base()
	jPriorPrivate.Input.Swarm = &SwarmInput{OutcomePriors: []PriorObservation{{KeyType: "source", KeyValue: "passthepopcorn", Alpha: 2, Beta: 2}}}
	require.ErrorContains(t, jPriorPrivate.Validate(), "invalid_outcome_prior")

	jAlive := base()
	jAlive.Input.Swarm = &SwarmInput{AliveSource: "BitGrab-Import"}
	require.ErrorContains(t, jAlive.Validate(), "private_or_invalid_alive_source")
}
