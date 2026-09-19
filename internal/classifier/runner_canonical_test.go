package classifier

import (
	"context"
	"errors"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// fakeStore is a minimal CanonicalStore for testing. Returns the
// configured label and error; tracks the last info_hash requested.
type fakeStore struct {
	label *evidence.CanonicalLabel
	err   error
	last  []byte
}

func (s *fakeStore) CanonicalForInfoHash(_ context.Context, ih []byte) (*evidence.CanonicalLabel, error) {
	s.last = append([]byte(nil), ih...)
	return s.label, s.err
}

// fakeRunner records whether it was invoked. Tests assert this to
// verify preemption short-circuited or fell through correctly.
type fakeRunner struct {
	called  bool
	result  classification.Result
	err     error
	torrent model.Torrent
}

func (r *fakeRunner) Run(_ context.Context, _ string, _ Flags, torrent model.Torrent) (classification.Result, error) {
	r.called = true
	r.torrent = torrent
	return r.result, r.err
}

func (r *fakeRunner) EvalMatch(_ context.Context, _ model.Torrent, _ model.NullContentType) (MatchDecision, error) {
	return MatchDecision{}, nil
}

// torrentWithHash is a convenience for tests — returns a minimal model
// torrent whose info_hash is a well-known byte pattern so assertions
// on the store's seen value are readable.
func torrentWithHash(b byte) model.Torrent {
	var ih protocol.ID
	for i := range ih {
		ih[i] = b
	}
	return model.Torrent{InfoHash: ih}
}

func TestCanonicalMovieConstrainsInner(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		ResolvedSource: evidence.SourceRadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x01))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("movie labels must continue through identity enrichment")
	}
	if inner.torrent.Hint.ContentType != model.ContentTypeMovie {
		t.Fatalf("expected authoritative movie hint, got %+v", inner.torrent.Hint)
	}
}

func TestCanonicalTvShowConstrainsInner(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeTV,
		ResolvedSource: evidence.SourceSonarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x02))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("TV labels must continue through identity enrichment")
	}
	if inner.torrent.Hint.ContentType != model.ContentTypeTvShow {
		t.Fatalf("expected authoritative TV hint, got %+v", inner.torrent.Hint)
	}
}

func TestCanonicalCatalogIDReplacesExistingHint(t *testing.T) {
	torrent := torrentWithHash(0x06)
	torrent.Hint = model.TorrentHint{
		ContentType:   model.ContentTypeMovie,
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("111"),
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		MediaID:        " TVDB : 222 ",
		ResolvedSource: evidence.SourceRadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if got := inner.torrent.Hint.ContentSource.String; got != model.SourceTvdb {
		t.Fatalf("expected canonical source tvdb, got %q", got)
	}
	if got := inner.torrent.Hint.ContentID.String; got != "222" {
		t.Fatalf("expected canonical ID 222, got %q", got)
	}
}

func TestCanonicalArrLocalIDPreservesCompatibleIdentity(t *testing.T) {
	torrent := torrentWithHash(0x07)
	torrent.Hint = model.TorrentHint{
		ContentType:   model.ContentTypeTvShow,
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("333"),
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeTV,
		MediaID:        "sonarr:42",
		ResolvedSource: evidence.SourceSonarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if got := inner.torrent.Hint.ContentSource.String; got != model.SourceTmdb {
		t.Fatalf("arr-local ID must not replace catalog identity, got source %q", got)
	}
	if got := inner.torrent.Hint.ContentID.String; got != "333" {
		t.Fatalf("arr-local ID must preserve compatible content ID, got %q", got)
	}
}

func TestCanonicalTypeConflictClearsPriorIdentity(t *testing.T) {
	torrent := torrentWithHash(0x08)
	torrent.Hint = model.TorrentHint{
		ContentType:   model.ContentTypeMovie,
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("444"),
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeTV,
		ResolvedSource: evidence.SourceSonarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if inner.torrent.Hint.ContentType != model.ContentTypeTvShow {
		t.Fatalf("expected canonical TV type, got %+v", inner.torrent.Hint)
	}
	if inner.torrent.Hint.ContentSource.Valid || inner.torrent.Hint.ContentID.Valid {
		t.Fatalf("cross-type prior identity must be cleared, got %+v", inner.torrent.Hint)
	}
}

func TestCanonicalMoviePreservesExistingAttachmentEndToEnd(t *testing.T) {
	content := model.Content{
		Type:   model.ContentTypeMovie,
		Source: model.SourceTmdb,
		ID:     "555",
		Title:  "Existing Match",
	}
	torrent := torrentWithHash(0x09)
	torrent.Contents = []model.TorrentContent{{
		ContentType:   model.NewNullContentType(model.ContentTypeMovie),
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("555"),
		Content:       content,
	}}
	inner := runner{
		workflows: map[string]action{
			"test": {run: func(ctx executionContext) (classification.Result, error) {
				return ctx.result, nil
			}},
		},
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		MediaID:        "radarr:12",
		ResolvedSource: evidence.SourceRadarr,
	}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	res, err := r.Run(context.Background(), "test", Flags{}, torrent)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == nil || res.Content.ID != "555" {
		t.Fatalf("canonical rematch downgraded existing attachment: %+v", res.Content)
	}
}

func TestCanonicalAmbiguousExistingAttachmentsAreNotChosen(t *testing.T) {
	torrent := torrentWithHash(0x0B)
	torrent.Contents = []model.TorrentContent{
		{
			ContentType:   model.NewNullContentType(model.ContentTypeMovie),
			ContentSource: model.NewNullString(model.SourceTmdb),
			ContentID:     model.NewNullString("1"),
		},
		{
			ContentType:   model.NewNullContentType(model.ContentTypeMovie),
			ContentSource: model.NewNullString(model.SourceTmdb),
			ContentID:     model.NewNullString("2"),
		},
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		MediaID:        "radarr:12",
		ResolvedSource: evidence.SourceRadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if inner.torrent.Hint.ContentSource.Valid || inner.torrent.Hint.ContentID.Valid {
		t.Fatalf("ambiguous existing identities must not be selected: %+v", inner.torrent.Hint)
	}
}

func TestCanonicalNonVideoStillPreempts(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeBook,
		ResolvedSource: evidence.SourceReadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	res, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x0A))
	if err != nil {
		t.Fatal(err)
	}
	if inner.called {
		t.Fatal("non-video type-only labels should retain the inexpensive preempt path")
	}
	if res.ContentType.ContentType != model.ContentTypeEbook {
		t.Fatalf("expected ebook, got %+v", res.ContentType)
	}
}

func TestFallThroughOnNoLabel(t *testing.T) {
	store := &fakeStore{label: nil}
	inner := &fakeRunner{result: classification.Result{}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x03))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("inner must be called when no canonical label exists")
	}
}

func TestFallThroughOnLookupError(t *testing.T) {
	store := &fakeStore{err: errors.New("pool down")}
	inner := &fakeRunner{result: classification.Result{}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x04))
	if err != nil {
		t.Fatal("availability must dominate: a store error should not fail classification")
	}
	if !inner.called {
		t.Fatal("inner must be called when canonical lookup fails")
	}
}

func TestFallThroughOnUnknownMediaType(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeUnknown,
		ResolvedSource: evidence.SourceQBittorrent,
	}}
	inner := &fakeRunner{result: classification.Result{}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x05))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("inner must run when canonical media_type is unknown")
	}
}

func TestInfoHashBytesForwardedToStore(t *testing.T) {
	store := &fakeStore{label: nil}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, _ = r.Run(context.Background(), "", Flags{}, torrentWithHash(0xAB))
	if len(store.last) != 20 {
		t.Fatalf("expected 20-byte infohash forwarded to store, got %d", len(store.last))
	}
	for _, b := range store.last {
		if b != 0xAB {
			t.Fatalf("unexpected byte in forwarded infohash: %x", b)
		}
	}
}
