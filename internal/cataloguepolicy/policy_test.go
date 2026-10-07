package cataloguepolicy

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnglishClaimsSeparateTitleOriginalAndTracks(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name        string
		anime, subs bool
		state       string
	}{
		{"The.French.Connection.1971.1080p", false, false, "unknown"},
		{"Italian.Job.2003.1080p.English.Audio", false, false, "eligible_audio"},
		{"Foreign.Original.2031.1080p.ENG.DUB", false, false, "eligible_audio"},
		{"Translated.Title.2031.1080p.Dual.Audio", false, false, "unknown"},
		{"Show.S01E01.1080p.Multi.Audio.[Audio: Hindi+JPN]", true, true, "unknown"},
		{"Show.S01E01.1080p.[Audio: English+JPN]", true, true, "eligible_audio"},
		{"Show.S01E01.1080p.[English Subs]", true, true, "eligible_anime_subtitles"},
		{"Show.S01E01.1080p.[English Subs]", true, false, "unknown"},
		{"Film.2031.1080p.[English Subs]", false, true, "unknown"},
		{"Show.S01E01.1080p.[Audio only: Japanese; No English subtitles]", true, true, "ineligible_review"},
		{"Film.2031.1080p.[No English audio]", false, false, "ineligible_review"},
		{"Film.2031.1080p.No.English.Dub", false, false, "ineligible_review"},
		{"Film.2031.1080p.No.English.Dubbed", false, false, "ineligible_review"},
		{"Film.2031.1080p.[Audio: no English]", false, false, "ineligible_review"},
		{"Film.2031.1080p.[Audio: without English]", false, false, "ineligible_review"},
		{"Film.2031.1080p.[Audio: English unavailable]", false, false, "ineligible_review"},
		{"Show.S01E01.1080p.[Subtitles: no English]", true, true, "unknown"},
		{"Show.S01E01.1080p.[Audio only: Japanese; Subtitles: no English]", true, true, "ineligible_review"},
		{"Film.2031.1080p.[Audio: no English, English]", false, false, "conflicting"},
		{"Film.2031.1080p.[English Audio; No English audio]", false, false, "conflicting"},
		{"English Audio", false, false, "unknown"},
		{"Show.S01E01.1080p.Dubbed.Multi.Sub", true, true, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ParseEnglishClaims(tc.name, "release_name", now)
			d := EvaluateEnglish([]EnglishEvidence{e}, tc.anime, tc.subs, false)
			require.Equal(t, tc.state, d.State)
			require.Equal(t, ParserVersion, e.ParserVersion)
			require.Equal(t, now, e.ObservedAt)
		})
	}
	english := ParseEnglishClaims("A.2031.1080p.English.Audio", "media_file_name", now)
	missing := ParseEnglishClaims("B.2031.1080p", "media_file_name", now)
	foreign := ParseEnglishClaims("B.2031.1080p.Japanese.Audio.Only", "media_file_name", now)
	require.Equal(t, "mixed_review", EvaluateEnglish([]EnglishEvidence{english, missing}, false, false, false).State)
	require.Equal(t, "mixed_review", EvaluateEnglish([]EnglishEvidence{english, foreign}, false, false, false).State)
	require.Equal(t, "unknown", EvaluateEnglish([]EnglishEvidence{english}, false, false, true).State)
}

func TestAvailabilityFreshnessOutagesAndRecovery(t *testing.T) {
	now := time.Date(2031, 1, 8, 12, 0, 0, 0, time.UTC)
	cfg := DefaultAvailabilityConfig()
	obs := func(id, class string, ago time.Duration, qualified bool) Observation {
		o := newObservation("tracker", now.Add(-ago), class, qualified, cfg)
		o.ID = id
		return o
	}
	one := obs("z1", "zero", time.Hour, true)
	two := obs("z2", "zero", 6*time.Hour, true)
	for _, tc := range []struct {
		name  string
		in    []Observation
		state string
	}{
		{"one zero", []Observation{one}, "fresh_known_zero"},
		{"duplicate source receipt", []Observation{one, one, one}, "fresh_known_zero"},
		{"repeated spaced qualified", []Observation{one, two}, "suspected_unavailable"},
		{"too close", []Observation{one, obs("z3", "zero", 90*time.Minute, true)}, "fresh_known_zero"},
		{"old zero", []Observation{obs("old", "zero", 25*time.Hour, true)}, "unknown_stale"},
		{"outage", []Observation{obs("outage", "zero", time.Minute, false)}, "unknown_stale"},
		{"unknown breaks sequence", []Observation{one, two, obs("outage", "unknown", 30*time.Minute, false)}, "unknown_stale"},
		{"positive revives", []Observation{one, two, obs("positive", "positive", time.Minute, true)}, "fresh_positive"},
		{"recent positive protects", []Observation{one, two, obs("history", "positive", 2*24*time.Hour, true)}, "fresh_known_zero"},
		{"unhealthy client", []Observation{obs("client1", "stall", time.Hour, false), obs("client2", "stall", 6*time.Hour, false)}, "unknown_stale"},
		{"one client stall", []Observation{obs("client1", "stall", time.Hour, true)}, "unknown_stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := EvaluateAvailability(tc.in, now, cfg)
			require.Equal(t, tc.state, d.State)
			require.NotEqual(t, "policy_confirmed_unavailable", d.State)
			require.False(t, d.ExpiresAt.After(now.Add(cfg.FreshFor)))
		})
	}
	private := one
	private.Private = true
	require.Equal(t, "unknown_stale", EvaluateAvailability([]Observation{private}, now, cfg).State)
	require.Equal(t, "unknown_stale", EvaluateAvailability([]Observation{one, two}, now.Add(24*time.Hour), cfg).State)
	future := obs("future", "zero", -time.Hour, true)
	require.Equal(t, "unknown_stale", EvaluateAvailability([]Observation{future}, now, cfg).State)
}

func TestObservationHistoryIsBoundedAndSourceDeduplicated(t *testing.T) {
	now := time.Now().UTC()
	cfg := DefaultAvailabilityConfig()
	var all []Observation
	for i := 0; i < 100; i++ {
		all = append(all, newObservation("tracker", now.Add(-time.Duration(i)*time.Minute), "zero", true, cfg))
	}
	got := mergeObservations(all, all, now, cfg)
	require.Len(t, got, maxObservations)
	require.Equal(t, all[0].ID, got[0].ID)
}

func TestBoundedHistoryPreservesRecentPositiveVeto(t *testing.T) {
	now := time.Date(2031, 1, 8, 12, 0, 0, 0, time.UTC)
	cfg := DefaultAvailabilityConfig()
	negative := make([]Observation, 100)
	for i := range negative {
		negative[i] = newObservation("tracker", now.Add(-time.Duration(i+1)*5*time.Minute), "zero", true, cfg)
		negative[i].ID = fmt.Sprintf("negative:%d", i)
	}
	positive := newObservation("public_qb", now.Add(-36*time.Hour), "positive", true, cfg)
	for _, fromPrevious := range []bool{false, true} {
		t.Run(fmt.Sprint(fromPrevious), func(t *testing.T) {
			current := append([]Observation(nil), negative...)
			var previous []Observation
			if fromPrevious {
				previous = []Observation{positive}
			} else {
				current = append(current, positive)
			}
			got := mergeObservations(current, previous, now, cfg)
			require.Len(t, got, maxObservations)
			require.Equal(t, negative[0].ID, got[0].ID)
			require.Equal(t, positive.ID, got[maxObservations-1].ID)
			d := EvaluateAvailability(got, now, cfg)
			require.Equal(t, "fresh_known_zero", d.State)
			require.Equal(t, "recent_positive_history_protects", d.Reason)
		})
	}
	for _, rejected := range []Observation{
		newObservation("public_qb", now.Add(-8*24*time.Hour), "positive", true, cfg),
		newObservation("public_qb", now.Add(time.Hour), "positive", true, cfg),
		newObservation("public_qb", now.Add(-36*time.Hour), "positive", false, cfg),
	} {
		got := mergeObservations(negative, []Observation{rejected}, now, cfg)
		require.Equal(t, "suspected_unavailable", EvaluateAvailability(got, now, cfg).State)
	}
}
