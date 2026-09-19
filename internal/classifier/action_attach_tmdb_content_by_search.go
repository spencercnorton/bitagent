package classifier

import (
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

const attachTmdbContentBySearchName = "attach_tmdb_content_by_search"

type attachTmdbContentBySearchAction struct{}

func (attachTmdbContentBySearchAction) name() string {
	return attachTmdbContentBySearchName
}

var attachTmdbContentBySearchPayloadSpec = payloadLiteral[string]{
	literal:     attachTmdbContentBySearchName,
	description: "Attempt to attach content from the TMDB API with a search on the torrent name",
}

func (attachTmdbContentBySearchAction) compileAction(ctx compilerContext) (action, error) {
	if _, err := attachTmdbContentBySearchPayloadSpec.Unmarshal(ctx); err != nil {
		return action{}, ctx.error(err)
	}

	return action{
		run: func(ctx executionContext) (classification.Result, error) {
			cl := ctx.result
			if !cl.BaseTitle.Valid {
				return cl, classification.ErrUnmatched
			}

			// Deterministic anime alias resolution, on the FREE path. The
			// backbone was previously reachable only from the LLM matcher, so
			// with CLASSIFIER_LLM_MATCH_ENABLED=false none of its 32,881
			// catalogued aliases were consulted at all — and a romaji base
			// title ("Sousou no Frieren") is exactly what a plain TMDB search
			// handles worst.
			//
			// Trust follows the contract Alias already declares: only a
			// human-curated seed may DirectAttach; data-built rows guide the
			// SEARCH QUERY and are still picked by the existing ranker. The
			// Adult gate previously existed only on the LLM path, so on this
			// path a known adult title had no gate whatsoever.
			query := cl.BaseTitle.String
			if ctx.animeResolver != nil {
				if alias, ok := ctx.animeResolver.Lookup(
					cl.BaseTitle.String, "", ctx.torrent.Name); ok {
					if alias.Adult {
						return cl, classification.ErrUnmatched
					}
					if alias.Display != "" {
						query = alias.Display
					}
					if alias.DirectAttach && alias.TMDBID > 0 {
						var (
							c   model.Content
							err error
						)
						if alias.TMDBType == model.ContentTypeTvShow {
							c, err = ctx.tmdbGetTVShowByTMDBID(alias.TMDBID)
						} else {
							c, err = ctx.tmdbGetMovieByTMDBID(alias.TMDBID)
						}
						if err == nil {
							cl.AttachContent(&c)
							return cl, nil
						}
						// A lookup failure is not a match failure: fall
						// through to the ordinary search on the alias title.
					}
				}
			}

			var content *model.Content
			switch cl.ContentType.ContentType {
			case model.ContentTypeTvShow:
				result, searchErr := ctx.tmdbSearchTVShow(query, cl.Date.Year)
				if searchErr != nil {
					return cl, searchErr
				}
				content = &result
			default:
				if len(cl.Episodes) > 0 {
					return cl, classification.ErrUnmatched
				}
				result, searchErr := ctx.tmdbSearchMovie(query, cl.Date.Year)
				if searchErr != nil {
					return cl, searchErr
				}
				content = &result
			}
			cl.AttachContent(content)
			return cl, nil
		},
	}, nil
}

func (attachTmdbContentBySearchAction) JSONSchema() JSONSchema {
	return attachTmdbContentBySearchPayloadSpec.JSONSchema()
}
