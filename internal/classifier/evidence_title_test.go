package classifier

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/evaltrace"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTitleLabels struct {
	mu     sync.Mutex
	labels []evidence.TitleLabel
	err    error
	calls  int
}

func (f *fakeTitleLabels) CanonicalTitleLabels(context.Context) ([]evidence.TitleLabel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.labels, f.err
}

func (f *fakeTitleLabels) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeTitleLabels) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func hashOf(b byte) protocol.ID {
	var ih protocol.ID
	for i := range ih {
		ih[i] = b
	}
	return ih
}

func tvLabel(b byte, name, mediaID string) evidence.TitleLabel {
	ih := hashOf(b)
	return evidence.TitleLabel{InfoHash: ih.Bytes(), Name: name, MediaType: evidence.MediaTypeTV, MediaID: mediaID}
}

func torrentNamed(b byte, name string) model.Torrent {
	return model.Torrent{InfoHash: hashOf(b), Name: name}
}

func TestTitleEvidenceKey(t *testing.T) {
	t.Parallel()

	// Different release spellings of one show collapse to one key.
	a := titleEvidenceKey("Married.At.First.Sight.S16E02.720p.WEB.h264-BAE[rarbg]")
	b := titleEvidenceKey("Married at First Sight S20E19 Forever or Goodbye 1080p PCOK WEB-DL")
	require.NotEmpty(t, a)
	assert.Equal(t, a, b)
	assert.Equal(t,
		titleEvidenceKey("Diners.Drive.Ins.And.Dives.S01E10.720p.HDTV.x264-W4F"),
		titleEvidenceKey("Diners, Drive-Ins and Dives S24E09 South Beach 1080p WEB"))

	// Movies keep their year, so a remake never borrows the original's votes.
	assert.NotEqual(t, titleEvidenceKey("Dune.2021.2160p.WEB-DL"), titleEvidenceKey("Dune.1984.1080p.BluRay"))
	// A year-less release can never borrow a TV series' votes.
	assert.NotEqual(t, titleEvidenceKey("Fargo.1080p.BluRay.x264"), titleEvidenceKey("Fargo.S05E01.1080p.WEB"))
}

func TestTitleEvidencePreferred(t *testing.T) {
	t.Parallel()

	const us, au = "tmdb:60989", "tmdb:62705"
	src := &fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "Married.At.First.Sight.S16E02.720p.WEB.h264-BAE", us),
		tvLabel(2, "Married.at.First.Sight.S03E02.The.Weddings.720p.WEB", us),
		tvLabel(3, "Married At First Sight S20E19 1080p PCOK WEB-DL", us),
		tvLabel(4, "Married.at.First.Sight.S11E03.720p.WEB.h264-ROBOTS", au),
	}}
	e := NewTitleEvidence(src)
	ctx := context.Background()

	// A new episode: 3 of 4 labels say US — 75% ≥ 2/3.
	lbl, out := e.Preferred(ctx, torrentNamed(9, "Married.At.First.Sight.S17E01.1080p.WEB.h264-EDITH"))
	require.Equal(t, titleApplied, out)
	assert.Equal(t, us, lbl.MediaID)
	assert.Equal(t, evidence.MediaTypeTV, lbl.MediaType)

	// Leave-one-out: torrent 1 does not vote for itself; the other three
	// split 2 US / 1 AU = 67% — still a majority.
	lbl, out = e.Preferred(ctx, torrentNamed(1, "Married.At.First.Sight.S16E02.720p.WEB.h264-BAE"))
	require.Equal(t, titleApplied, out)
	assert.Equal(t, us, lbl.MediaID)

	// A title nobody has grabbed has no preference.
	_, out = e.Preferred(ctx, torrentNamed(9, "Some.Other.Show.S01E01.1080p.WEB"))
	assert.Equal(t, titleNoPreference, out)

	assert.Equal(t, 1, src.count(), "the index is built once, not per lookup")
}

func TestTitleEvidenceNeedsTwoVotesAndAMajority(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	newEp := torrentNamed(9, "Kitchen.Nightmares.S08E01.1080p.WEB.h264")

	// One label is never enough.
	one := NewTitleEvidence(&fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "Kitchen.Nightmares.S01E05.1080p.WEB", "tmdb:11294"),
	}})
	_, out := one.Preferred(ctx, newEp)
	assert.Equal(t, titleWeak, out)

	// A 2–2 split is never a majority.
	split := NewTitleEvidence(&fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "Kitchen.Nightmares.S01E05.1080p.WEB", "tmdb:11294"),
		tvLabel(2, "Kitchen.Nightmares.S02E01.1080p.WEB", "tmdb:11294"),
		tvLabel(3, "Kitchen.Nightmares.S08E03.1080p.WEB", "tmdb:235884"),
		tvLabel(4, "Kitchen.Nightmares.S08E04.1080p.WEB", "tmdb:235884"),
	}})
	_, out = split.Preferred(ctx, newEp)
	assert.Equal(t, titleWeak, out)

	// Leave-one-out can drop a title below two votes: torrent 1 of a
	// two-label title has only torrent 2 left to speak for it.
	pair := NewTitleEvidence(&fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "Kitchen.Nightmares.S01E05.1080p.WEB", "tmdb:11294"),
		tvLabel(2, "Kitchen.Nightmares.S02E01.1080p.WEB", "tmdb:11294"),
	}})
	_, out = pair.Preferred(ctx, torrentNamed(1, "Kitchen.Nightmares.S01E05.1080p.WEB"))
	assert.Equal(t, titleWeak, out)
}

func TestTitleEvidenceSkipsArrLocalIDs(t *testing.T) {
	t.Parallel()

	// sonarr:/radarr: ids name nothing in the catalogue and must not vote.
	e := NewTitleEvidence(&fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "Lioness.S01E01.1080p.WEB", "sonarr:12"),
		tvLabel(2, "Lioness.S01E02.1080p.WEB", "sonarr:12"),
	}})
	_, out := e.Preferred(context.Background(), torrentNamed(9, "Lioness.S02E01.1080p.WEB"))
	assert.Equal(t, titleNoPreference, out)
}

func TestTitleEvidenceFailsOpen(t *testing.T) {
	t.Parallel()

	e := NewTitleEvidence(&fakeTitleLabels{err: errors.New("db down")})
	_, out := e.Preferred(context.Background(), torrentNamed(9, "Anything.S01E01.WEB"))
	assert.Equal(t, titleNoIndex, out)
}

func TestCanonicalRunnerTitleEvidence(t *testing.T) {
	t.Parallel()

	labels := &fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "Diners.Drive.Ins.And.Dives.S01E10.720p.HDTV", "tvdb:79099"),
		tvLabel(2, "Diners, Drive-Ins and Dives S24E09 1080p WEB", "tvdb:79099"),
	}}
	newEp := torrentNamed(9, "Diners Drive-ins and Dives S40E06 Mouthwatering Meat 1080p MAX WEB-DL")

	t.Run("no label of its own: the title's identity is hinted", func(t *testing.T) {
		inner := &fakeRunner{}
		r := NewCanonicalRunner(inner, &fakeStore{}, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels)))
		_, err := r.Run(context.Background(), "", Flags{}, newEp)
		require.NoError(t, err)
		h := inner.torrent.Hint
		assert.Equal(t, model.ContentTypeTvShow, h.ContentType)
		assert.Equal(t, "tvdb", h.ContentSource.String)
		assert.Equal(t, "79099", h.ContentID.String)
	})

	t.Run("replay skips the exact preempt but still measures title evidence", func(t *testing.T) {
		inner := &fakeRunner{}
		store := &fakeStore{label: &evidence.CanonicalLabel{MediaType: evidence.MediaTypeTV, MediaID: "tvdb:1"}}
		r := NewCanonicalRunner(inner, store, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels)))
		trace := &evaltrace.Trace{SkipCanonicalPreempt: true}
		ctx := evaltrace.With(context.Background(), trace)
		_, err := r.Run(ctx, "", Flags{}, newEp)
		require.NoError(t, err)
		assert.Nil(t, store.last, "the torrent's own label must not be consulted in replay")
		assert.Equal(t, "79099", inner.torrent.Hint.ContentID.String)
		assert.Contains(t, trace.Stages(), "evidence_title")
	})

	t.Run("an exact label still wins", func(t *testing.T) {
		inner := &fakeRunner{}
		store := &fakeStore{label: &evidence.CanonicalLabel{MediaType: evidence.MediaTypeTV, MediaID: "tvdb:555"}}
		r := NewCanonicalRunner(inner, store, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels)))
		_, err := r.Run(context.Background(), "", Flags{}, newEp)
		require.NoError(t, err)
		assert.Equal(t, "555", inner.torrent.Hint.ContentID.String)
	})

	t.Run("off by default: the torrent passes through untouched", func(t *testing.T) {
		inner := &fakeRunner{}
		r := NewCanonicalRunner(inner, &fakeStore{}, NewPreemptMetrics())
		_, err := r.Run(context.Background(), "", Flags{}, newEp)
		require.NoError(t, err)
		assert.True(t, inner.torrent.Hint.IsNil())
	})
}

// A failing refresh must back off: a database outage is asked once a minute,
// not once per classification (a review finding on this feature).
func TestTitleEvidenceFailedRefreshBacksOff(t *testing.T) {
	t.Parallel()

	src := &fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "Kitchen.Nightmares.S01E05.1080p.WEB", "tmdb:11294"),
	}}
	clock := time.Unix(1_000_000, 0)
	var mu sync.Mutex
	e := NewTitleEvidence(src)
	e.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	lookup := func() {
		e.Preferred(context.Background(), torrentNamed(9, "Kitchen.Nightmares.S08E01.WEB"))
		require.Eventually(t, func() bool { return !e.building.Load() }, time.Second, time.Millisecond)
	}

	lookup()
	require.Equal(t, 1, src.count(), "first use builds synchronously")

	src.set(errors.New("db down"))
	advance(titleEvidenceTTL)
	lookup()
	require.Equal(t, 2, src.count(), "a stale index triggers one refresh")

	for range 50 {
		lookup()
	}
	assert.Equal(t, 2, src.count(), "a failed refresh is not retried per lookup")

	advance(titleEvidenceRetry)
	lookup()
	assert.Equal(t, 3, src.count(), "it is retried once the back-off has passed")
}
