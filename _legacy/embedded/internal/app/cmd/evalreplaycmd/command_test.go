package evalreplaycmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

type replayTestRunner struct {
	result classification.Result
	err    error
}

func (r replayTestRunner) Run(
	context.Context,
	string,
	classifier.Flags,
	model.Torrent,
) (classification.Result, error) {
	return r.result, r.err
}

func (replayTestRunner) EvalMatch(
	context.Context,
	model.Torrent,
	model.NullContentType,
) (classifier.MatchDecision, error) {
	return classifier.MatchDecision{}, nil
}

func TestReplayOneScoresEveryExpectedOutcome(t *testing.T) {
	testCases := []struct {
		name        string
		result      classification.Result
		err         error
		missing     bool
		wantOutcome string
		wantAgree   bool
	}{
		{
			name: "matched correct",
			result: classification.Result{Content: &model.Content{
				Type: model.ContentTypeMovie, Source: "tmdb", ID: "603", Title: "The Matrix",
			}},
			wantOutcome: "matched",
			wantAgree:   true,
		},
		{
			name: "matched wrong id",
			result: classification.Result{Content: &model.Content{
				Type: model.ContentTypeMovie, Source: "tmdb", ID: "604", Title: "Wrong Film",
			}},
			wantOutcome: "matched",
		},
		{
			name: "matched wrong type",
			result: classification.Result{Content: &model.Content{
				Type: model.ContentTypeTvShow, Source: "tmdb", ID: "603", Title: "Wrong Type",
			}},
			wantOutcome: "matched",
		},
		{
			name: "matched wrong source",
			result: classification.Result{Content: &model.Content{
				Type: model.ContentTypeMovie, Source: "tvdb", ID: "603", Title: "Wrong Namespace",
			}},
			wantOutcome: "matched",
		},
		{
			name: "typed only",
			result: classification.Result{ContentAttributes: classification.ContentAttributes{
				ContentType: model.NewNullContentType(model.ContentTypeMovie),
			}},
			wantOutcome: "typed_only",
		},
		{
			name:        "unmatched",
			wantOutcome: "unmatched",
		},
		{
			name:        "delete",
			err:         classification.ErrDeleteTorrent,
			wantOutcome: "delete",
		},
		{
			name:        "failed",
			err:         errors.New("classifier unavailable"),
			wantOutcome: "failed",
		},
		{
			name:        "missing torrent",
			missing:     true,
			wantOutcome: "missing_torrent",
		},
	}

	ctx := newReplayTestContext(t)
	p := Params{ClassifierConfig: classifier.Config{Workflow: "test"}}
	st := summary{attachedBy: map[string]int{}}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			row, torrents := replayTestInput(byte(i+1), !tc.missing, map[string]any{
				"tmdb_id":   "603",
				"tmdb_type": "movie",
			})

			dec := p.replayOne(ctx, replayTestRunner{result: tc.result, err: tc.err}, nil, row, torrents, &st)
			st.total++

			require.Equal(t, tc.wantOutcome, dec.Outcome)
			require.NotNil(t, dec.Agree)
			require.Equal(t, tc.wantAgree, *dec.Agree)

			encoded, err := json.Marshal(dec)
			require.NoError(t, err)

			var emitted map[string]any
			require.NoError(t, json.Unmarshal(encoded, &emitted))
			require.Equal(t, tc.wantAgree, emitted["agree"], "agree must be emitted even when false")
		})
	}

	require.Equal(t, summary{
		total:           9,
		missing:         1,
		matched:         4,
		typedOnly:       1,
		unmatched:       1,
		deleted:         1,
		failed:          1,
		agree:           1,
		disagree:        8,
		expectedTotal:   9,
		expectedMatched: 4,
		attachedBy:      map[string]int{"": 4},
	}, st)
}

func TestReplayOneWithoutResolvedExpectationOmitsAgree(t *testing.T) {
	ctx := newReplayTestContext(t)
	p := Params{ClassifierConfig: classifier.Config{Workflow: "test"}}
	st := summary{attachedBy: map[string]int{}}
	row, torrents := replayTestInput(1, false, map[string]any{
		"media_id": "sonarr:42",
	})

	dec := p.replayOne(ctx, replayTestRunner{}, nil, row, torrents, &st)
	require.Equal(t, "missing_torrent", dec.Outcome)
	require.Nil(t, dec.Agree)
	require.Zero(t, st.expectedTotal)
	require.Zero(t, st.agree)
	require.Zero(t, st.disagree)

	encoded, err := json.Marshal(dec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"agree"`)
}

func newReplayTestContext(t *testing.T) *cli.Context {
	t.Helper()

	flags := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	flags.Bool("canonicalPreempt", false, "")
	ctx := cli.NewContext(cli.NewApp(), flags, nil)
	ctx.Context = context.Background()

	return ctx
}

func replayTestInput(
	hashByte byte,
	includeTorrent bool,
	expected map[string]any,
) (frozenRow, map[protocol.ID]*torrentRecord) {
	infoHash := make([]byte, 20)
	for i := range infoHash {
		infoHash[i] = hashByte
	}

	row := frozenRow{infoHash: infoHash, name: "The.Matrix.1999", expected: expected}
	torrents := make(map[protocol.ID]*torrentRecord)
	if includeTorrent {
		var id protocol.ID
		copy(id[:], infoHash)
		torrents[id] = &torrentRecord{torrent: model.Torrent{InfoHash: id, Name: row.name}}
	}

	return row, torrents
}
