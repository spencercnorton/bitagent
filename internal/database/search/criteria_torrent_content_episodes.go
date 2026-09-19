package search

import (
	"fmt"
	"strings"

	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TorrentContentEpisodesCriteria filters torrent_contents by season/episode.
//
// The predicate mirrors model.Episodes.HasEpisode: a stored whole-season pack
// (season key present with an empty object) contains *every* episode of that
// season, so it must satisfy both season-only and specific-episode queries.
//
//   - Season-only request ({season}): match any row carrying that season key —
//     whole-season packs, partial/volume packs, and individual episodes alike.
//     Uses jsonb_exists() rather than the `?` operator because DBCriteria.SQL is
//     raw SQL where GORM treats a literal `?` as a bind placeholder.
//   - Episode request ({season: {episodes}}): match rows whose season is a
//     whole-season pack OR that enumerate all requested episodes.
//
// Before this, season-only queries matched only exact whole-season packs
// (excluding partial packs and single episodes) and episode queries excluded
// whole-season packs — both contradicting HasEpisode and silently under-serving
// Sonarr/Prowlarr season and episode searches.
func TorrentContentEpisodesCriteria(episodes model.Episodes) query.Criteria {
	return query.GenCriteria(func(query.DBContext) (query.Criteria, error) {
		and := make([]query.Criteria, 0, len(episodes))

		for _, s := range episodes.SeasonEntries() {
			if len(s.Episodes) == 0 {
				and = append(and, query.DBCriteria{
					SQL: fmt.Sprintf(
						"jsonb_exists(torrent_contents.episodes, '%d')",
						s.Season,
					),
				})
			} else {
				keyParts := make([]string, 0, len(s.Episodes))
				for _, e := range s.Episodes {
					keyParts = append(keyParts, fmt.Sprintf("\"%d\":{}", e))
				}

				and = append(and, query.DBCriteria{
					SQL: fmt.Sprintf(
						"(torrent_contents.episodes #> '{%d}' = '{}'::jsonb "+
							"OR torrent_contents.episodes #> '{%d}' @> '{%s}'::jsonb)",
						s.Season,
						s.Season,
						strings.Join(keyParts, ","),
					),
				})
			}
		}

		return query.AndCriteria{
			Criteria: and,
		}, nil
	})
}
