package classifier

import (
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/model"
)

const parseVideoContentName = "parse_video_content"

type parseVideoContentAction struct{}

func (parseVideoContentAction) name() string {
	return parseVideoContentName
}

var parseVideoContentPayloadSpec = payloadLiteral[string]{
	literal:     parseVideoContentName,
	description: "Parse video-related attributes from the name of the current torrent",
}

func (parseVideoContentAction) compileAction(ctx compilerContext) (action, error) {
	if _, err := parseVideoContentPayloadSpec.Unmarshal(ctx); err != nil {
		return action{}, ctx.error(err)
	}

	return action{
		run: func(ctx executionContext) (classification.Result, error) {
			parsed, err := parsers.ParseVideoContentWithOptions(ctx.torrent, ctx.result, parsers.ParseOptions{
				NoiseV2: ctx.parseNoiseV2,
			})
			cl := ctx.result
			if err != nil {
				return cl, err
			}
			cl.Merge(parsed)

			// Season-pack size backstop: a season-only parse (season key present
			// but no episode numbers) on a small torrent is almost certainly a
			// single episode mis-classified as a complete-season pack due to an
			// unrecognised episode notation.  Genuine season packs are typically
			// ≥ 10 GiB; a ceiling of 4 GiB (configurable) is safely below that.
			//
			// When the backstop fires we clear Episodes so the release is no
			// longer presented as a season pack.  The episode number itself stays
			// unknown (acceptable per spec); the important correction is that it
			// is not classified as a season-level pack.
			//
			// The backstop is disabled when SingleEpisodeMaxBytes == 0.
			if ctx.singleEpisodeMaxBytes > 0 &&
				isSeasonOnlyPack(cl.Episodes) &&
				int64(ctx.torrent.Size) > 0 &&
				int64(ctx.torrent.Size) <= ctx.singleEpisodeMaxBytes {
				cl.Episodes = nil
			}

			return cl, nil
		},
	}, nil
}

// isSeasonOnlyPack reports true when episodes encodes at least one season key
// but every inner episode map is empty (i.e. the parse captured a whole-season
// token but no specific episode numbers).  This is the signature of a season
// pack or — falsely — a single episode whose notation was not recognised.
func isSeasonOnlyPack(episodes model.Episodes) bool {
	if len(episodes) == 0 {
		return false
	}
	for _, epMap := range episodes {
		if len(epMap) > 0 {
			// at least one season has specific episode numbers → not season-only
			return false
		}
	}
	return true
}

func (parseVideoContentAction) JSONSchema() JSONSchema {
	return parseVideoContentPayloadSpec.JSONSchema()
}
