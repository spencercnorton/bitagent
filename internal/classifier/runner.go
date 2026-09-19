package classifier

import (
	"context"
	"fmt"

	"github.com/google/cel-go/common/types/ref"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protobuf"
)

type runner struct {
	dependencies
	flagDefinitions
	compiledFlags
	workflows map[string]action
}

func (r runner) EvalMatch(ctx context.Context, t model.Torrent, ct model.NullContentType) (MatchDecision, error) {
	// Rebuild the same independent parser evidence that the production action
	// has already accumulated before it reaches the matcher. Without this the
	// canary-only source-title gate sees an empty title and the evaluator
	// declines every candidate, which measures neither the live path nor the
	// gate's real selectivity.
	parsed, _ := parsers.ParseVideoContentWithOptions(
		t,
		classification.Result{ContentAttributes: classification.ContentAttributes{
			ContentType: ct,
		}},
		parsers.ParseOptions{NoiseV2: r.parseNoiseV2},
	)
	decision, err := (matchRunner{
		search:        r.search,
		tmdb:          r.tmdbClient,
		lm:            r.llmMatch,
		resolver:      r.animeResolver,
		parsedTitle:   parsed.BaseTitle.String,
		altTitleMatch: r.altTitleMatch,
	}).decide(ctx, t, ct)
	decision.ParsedTitle = parsed.BaseTitle.String
	return decision, err
}

func (r runner) Run(ctx context.Context, workflow string, flags Flags, t model.Torrent) (classification.Result, error) {
	w, ok := r.workflows[workflow]
	if !ok {
		return classification.Result{}, fmt.Errorf("workflow not found: %s", workflow)
	}

	cfs := make(map[string]ref.Val, len(r.flagDefinitions))

	for k, d := range r.flagDefinitions {
		if runtimeRawVal, ok := flags[k]; ok {
			rcf, err := d.celVal(runtimeRawVal)
			if err != nil {
				return classification.Result{}, fmt.Errorf(
					"invalid value for runtime flag '%s': %w",
					k,
					err,
				)
			}

			cfs[k] = rcf
		} else {
			cfs[k] = r.compiledFlags[k]
		}
	}

	cl := classification.Result{}
	if !t.Hint.IsNil() {
		cl.ApplyHint(t.Hint)
	}
	// if possible, attach the existing content to the result to save some work:
	if !t.Hint.IsNil() && t.Hint.ContentSource.Valid {
		for _, tc := range t.Contents {
			if tc.ContentType.Valid &&
				tc.ContentType.ContentType == t.Hint.ContentType &&
				tc.ContentSource.Valid &&
				tc.ContentSource.String == t.Hint.ContentSource.String &&
				tc.ContentID.String == t.Hint.ContentID.String &&
				tc.Content.Source == tc.ContentSource.String {
				content := tc.Content
				cl.AttachContent(&content)

				break
			}
		}
	}

	exCtx := executionContext{
		Context:      ctx,
		dependencies: r.dependencies,
		workflows:    r.workflows,
		flags:        cfs,
		torrent:      t,
		torrentPb:    protobuf.NewTorrent(t),
		result:       cl,
	}

	return w.run(exCtx)
}
