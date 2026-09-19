package processor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// fakePrivacy reports a fixed answer or a fixed error and counts
// invocations so tests can verify the gate fires.
type fakePrivacy struct {
	isPrivate bool
	err       error
	calls     atomic.Int32
}

func (p *fakePrivacy) IsPrivateInfoHash(_ context.Context, _ []byte) (bool, error) {
	p.calls.Add(1)
	return p.isPrivate, p.err
}

// makeTorrent builds a single-file torrent stub. The hash is the only
// value the privacy gate cares about; a zero hash is fine because the
// fake doesn't inspect it.
func makeTorrent(name, ext string) model.Torrent {
	return model.Torrent{
		InfoHash:  protocol.ID{},
		Name:      name,
		Extension: model.NewNullString(ext),
	}
}

// classifyResult builds a classification.Result that the gate's
// signature accepts. Currently the gate ignores it (privacy is keyed
// on the infohash alone), but plumbing it through the call site keeps
// future signal-based extensions backward-compatible.
func classifyResult() classification.Result {
	return classification.Result{}
}

func llmMatchedResult() classification.Result {
	return classification.Result{
		Tags: map[string]struct{}{
			llmMatchedTagName: {},
		},
	}
}

func TestShouldSkipContentFilter_LLMMatchedSkipsBeforePrivacy(t *testing.T) {
	// The LLM matcher already applied its own privacy and anime English-track
	// gates. Running the generic post-classifier language filter afterward can
	// delete valid English-subbed anime because TMDB records original_language=ja.
	priv := &fakePrivacy{isPrivate: false}
	p := processor{privacy: priv}

	skip := p.shouldSkipContentFilter(context.Background(), makeTorrent("[SubsPlease] Anime - 01", "mkv"), llmMatchedResult())
	if !skip {
		t.Fatalf("llm-matched result must skip the post-classifier content filter")
	}
	if priv.calls.Load() != 0 {
		t.Fatalf("llm-matched skip should happen before privacy lookup, got %d calls", priv.calls.Load())
	}
}

func TestShouldSkipContentFilter_NilStoreDoesNotSkip(t *testing.T) {
	// In partial test wirings the privacy store may be absent. The
	// gate must default to "do not skip" so the rest of the hook
	// (deterministic ladder, LLM tier) still runs as before. Real
	// production wiring always supplies a store.
	p := processor{}
	skip := p.shouldSkipContentFilter(context.Background(), makeTorrent("Some Movie 2024", "mkv"), classifyResult())
	if skip {
		t.Fatalf("nil privacy store must default to NO skip, got skip=true")
	}
}

func TestShouldSkipContentFilter_NativePrivateContinuesToDeterministicFilter(t *testing.T) {
	// The native flag is enforced inside contentfilter.Input at the optional
	// LLM boundary. It must not use this outer skip, because that would also
	// bypass the local deterministic policy ladder.
	for _, tc := range []struct {
		name    string
		privacy PrivacyStore
	}{
		{name: "missing evidence"},
		{name: "stale public evidence", privacy: &fakePrivacy{isPrivate: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := processor{privacy: tc.privacy}
			tor := makeTorrent("Private Release 2026", "mkv")
			tor.Private = true
			if p.shouldSkipContentFilter(context.Background(), tor, classifyResult()) {
				t.Fatal("native private flag must reach the deterministic filter; only its LLM tier is blocked")
			}
		})
	}
}

func TestShouldSkipContentFilter_PrivateHashSkips(t *testing.T) {
	// Ground-truth case: the evidence store reports the infohash is
	// from a private tracker (qB category in {private, bitgrab}).
	// The hook MUST be skipped — the contentfilter LLM tier would
	// otherwise send the title to OpenAI.
	priv := &fakePrivacy{isPrivate: true}
	p := processor{privacy: priv}

	skip := p.shouldSkipContentFilter(context.Background(), makeTorrent("Some Movie 2024", "mkv"), classifyResult())
	if !skip {
		t.Fatalf("private hash must cause skip=true")
	}
	if priv.calls.Load() != 1 {
		t.Fatalf("privacy store must be consulted exactly once, got %d", priv.calls.Load())
	}
}

func TestShouldSkipContentFilter_PublicHashDoesNotSkip(t *testing.T) {
	// Public infohash — the gate doesn't fire and the hook proceeds
	// with the contentfilter ladder + LLM as designed. Verifies the
	// gate isn't a "skip everything" foot-gun.
	priv := &fakePrivacy{isPrivate: false}
	p := processor{privacy: priv}

	skip := p.shouldSkipContentFilter(context.Background(), makeTorrent("Some Movie 2024", "mkv"), classifyResult())
	if skip {
		t.Fatalf("public hash must NOT cause skip, got skip=true")
	}
	if priv.calls.Load() != 1 {
		t.Fatalf("privacy store must still be consulted on public, got %d", priv.calls.Load())
	}
}

func TestShouldSkipContentFilter_StoreErrorFailsClosed(t *testing.T) {
	// A transient DB error means we cannot verify privacy. The safe
	// default is to SKIP the contentfilter — better an under-curated
	// public torrent than a leaked private title to OpenAI. Mirrors
	// classifier/llmstage.Stage.Run which fails closed on the same
	// kind of error.
	priv := &fakePrivacy{err: errors.New("pgpool: connection refused")}
	p := processor{privacy: priv}

	skip := p.shouldSkipContentFilter(context.Background(), makeTorrent("Some Movie 2024", "mkv"), classifyResult())
	if !skip {
		t.Fatalf("privacy store error must fail closed (skip=true), got skip=false")
	}
}
