package classifier

import (
	"context"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TitleLabelSource yields every *arr canonical label that carries a catalogue
// identity, with the name of the torrent it was grabbed under.
// *evidence.Store implements it.
type TitleLabelSource interface {
	CanonicalTitleLabels(ctx context.Context) ([]evidence.TitleLabel, error)
}

// TitleEvidence answers, for a torrent with no *arr label of its own: which
// identity have the *arrs given OTHER torrents released under the same title?
// It is the canonical preempt generalised from one infohash to a title.
//
// It exists for the one class of error a deterministic ladder cannot fix.
// Several catalogue entries share a name — Married at First Sight US and AU,
// iCarly 2007 and 2021, Kitchen Nightmares UK and the US revival — and title
// similarity alone cannot choose between them. The *arrs already chose: every
// grab records the series the operator actually monitors. Sonarr then searches
// Torznab by that id, so a release attached to the wrong same-name entry is
// invisible to it.
//
// Measured before it was built (docs/project/benchmarks.md, 2026-09-22):
// leave-one-out over 3,344 gold rows, +141 correct over the deployed v2.9.2
// matcher (93.7% -> 97.9%), 0 broken.
type TitleEvidence struct {
	src TitleLabelSource
	now func() time.Time

	idx      atomic.Pointer[titleIndex]
	first    sync.Mutex
	building atomic.Bool
	// nextRefresh (unix nanos) schedules the next rebuild: TTL after a success,
	// titleEvidenceRetry after a failure. Kept apart from the index so a failing
	// database is asked once a minute, not once per classification.
	nextRefresh atomic.Int64
}

const (
	// ponytail: fixed thresholds, calibrated on one library's benchmark (against
	// the pre-deploy approximation of the fixed matcher, 2 votes fixed 150 rows
	// and 3 votes 141; both broke 0). A share above 1⁄2 also means a tied vote
	// can never pass. Make these config when a second library disagrees.
	titleEvidenceMinVotes = 2
	titleEvidenceMinShare = 2.0 / 3.0

	// Canonical labels arrive a few dozen a day; a quarter-hour of staleness
	// costs nothing.
	titleEvidenceTTL   = 15 * time.Minute
	titleEvidenceRetry = time.Minute
)

// titleOutcome labels bitagent_classifier_preempt_evidence_title_total.
type titleOutcome string

const (
	titleApplied      titleOutcome = "applied"
	titleNoKey        titleOutcome = "no_key"        // name did not parse to a title
	titleNoPreference titleOutcome = "no_preference" // no other labelled torrent under this title
	titleWeak         titleOutcome = "weak"          // too few votes, or no 2⁄3 majority
	titleNoIndex      titleOutcome = "no_index"      // labels not loadable
)

type titleVote struct {
	mediaType evidence.MediaType
	source    string
	id        string
}

type titleSelf struct {
	key  string
	vote titleVote
}

type titleIndex struct {
	votes map[string]map[titleVote]int
	self  map[string]titleSelf // hex infohash → that torrent's own vote
}

// NewTitleEvidence returns an index over src's labels, loaded on first use and
// refreshed in the background once stale.
func NewTitleEvidence(src TitleLabelSource) *TitleEvidence {
	return &TitleEvidence{src: src, now: time.Now}
}

// titleEvidenceKey is the one function that keys both sides: the index is
// built with it from *arr-labelled torrent names and looked up with it from
// the torrent being classified, so the two can never disagree about a title.
//
// The key carries the release's shape. An episode-shaped name keys as "tv";
// a name with a year and no episodes keys as a movie and keeps the year, so a
// Dune (2021) grab never steers Dune (1984). TV drops the year: release names
// rarely carry the premiere year, and the votes are what separate a revival
// from its original. Anything else keys as "other", so a year-less movie
// release can never borrow a TV series' votes.
func titleEvidenceKey(name string) string {
	title, year, episodes, _, err := parsers.ParseTitleYearEpisodes(model.NullContentType{}, name)
	if err != nil {
		return ""
	}
	norm := normalizeTitleForMatch(foldTitlePunct(title))
	if norm == "" {
		return ""
	}
	switch {
	case len(episodes) > 0:
		return "tv|" + norm
	case year != 0:
		return "movie|" + norm + "|" + strconv.Itoa(int(year))
	default:
		return "other|" + norm
	}
}

// Preferred returns the identity the *arrs agree on for torrents released under
// t's title, not counting t's own label. The label is usable only when the
// outcome is titleApplied.
func (e *TitleEvidence) Preferred(ctx context.Context, t model.Torrent) (*evidence.CanonicalLabel, titleOutcome) {
	idx := e.index(ctx)
	if idx == nil || idx.votes == nil {
		return nil, titleNoIndex
	}
	key := titleEvidenceKey(t.Name)
	if key == "" {
		return nil, titleNoKey
	}
	votes := idx.votes[key]
	own, hasOwn := idx.self[hex.EncodeToString(t.InfoHash.Bytes())]

	var (
		best         titleVote
		bestN, total int
	)
	for v, n := range votes {
		if hasOwn && own.key == key && own.vote == v {
			n-- // leave-one-out: a torrent is never its own evidence
		}
		total += n
		if n > bestN {
			best, bestN = v, n
		}
	}
	switch {
	case total == 0:
		return nil, titleNoPreference
	case bestN < titleEvidenceMinVotes || float64(bestN) < titleEvidenceMinShare*float64(total):
		return nil, titleWeak
	}

	return &evidence.CanonicalLabel{
		MediaType: best.mediaType,
		MediaID:   best.source + ":" + best.id,
	}, titleApplied
}

func (e *TitleEvidence) index(ctx context.Context) *titleIndex {
	cur := e.idx.Load()
	if cur == nil {
		e.first.Lock()
		if e.idx.Load() == nil {
			e.rebuild(ctx)
		}
		e.first.Unlock()

		return e.idx.Load()
	}
	if e.now().UnixNano() >= e.nextRefresh.Load() && e.building.CompareAndSwap(false, true) {
		go func() {
			defer e.building.Store(false)
			e.rebuild(context.Background())
		}()
	}

	return cur
}

func (e *TitleEvidence) rebuild(ctx context.Context) {
	labels, err := e.src.CanonicalTitleLabels(ctx)
	if err != nil {
		// Fail open to the ordinary matcher: keep serving the previous index
		// (an empty one if there is none) and try again in a minute.
		if e.idx.Load() == nil {
			e.idx.Store(&titleIndex{})
		}
		e.nextRefresh.Store(e.now().Add(titleEvidenceRetry).UnixNano())

		return
	}

	idx := &titleIndex{
		votes: make(map[string]map[titleVote]int),
		self:  make(map[string]titleSelf, len(labels)),
	}
	for _, l := range labels {
		contentType, ok := canonicalToContentType(l.MediaType)
		if !ok || !canonicalNeedsIdentityEnrichment(contentType) {
			continue
		}
		ref, ok := canonicalContentRef(l.MediaID, contentType)
		if !ok {
			continue // *arr-local ids (sonarr:/radarr:) name nothing in the catalogue
		}
		key := titleEvidenceKey(l.Name)
		if key == "" {
			continue
		}
		v := titleVote{mediaType: l.MediaType, source: ref.Source, id: ref.ID}
		if idx.votes[key] == nil {
			idx.votes[key] = make(map[titleVote]int)
		}
		idx.votes[key][v]++
		idx.self[hex.EncodeToString(l.InfoHash)] = titleSelf{key: key, vote: v}
	}
	e.idx.Store(idx)
	e.nextRefresh.Store(e.now().Add(titleEvidenceTTL).UnixNano())
}
