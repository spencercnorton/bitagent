package search

import (
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/maps"
	"github.com/spencercnorton/bitagent/internal/model"
	"gorm.io/gen/field"
)

// TorrentContentAnimeCriteria matches torrent-content rows flagged anime by
// the persisted torrent_contents.is_anime column. That flag is computed once
// at classify time from the FULL deterministic detector
// (internal/anime.Detect(name).IsAnime()): a known fansub-group bracket
// ANYWHERE in the name, or an unlisted leading Latin group tag alongside an
// absolute "Title - NNN" episode / romaji season marker / English-track
// marker. It supersedes the earlier leading-bracket prefix-LIKE approximation
// on torrents.name, which missed anime whose group bracket was not leading and
// could only prefix-match — this reads the same signal the Torznab adapter
// emits as category 5070, from one stored source of truth.
//
// It is the server-side companion to the Torznab adapter's per-result 5070
// (TV/Anime) emission: combined with a tv_show type filter it powers a bare
// `cat=5070` browse (return anime only). A query-scoped `cat=5070&q=<title>`
// already works via the emitted category on each result, so this narrows the
// no-query browse case.
func TorrentContentAnimeCriteria() query.Criteria {
	return query.DaoCriteria{
		Conditions: func(ctx query.DBContext) ([]field.Expr, error) {
			q := ctx.Query()
			return []field.Expr{
				q.TorrentContent.IsAnime.Is(true),
			}, nil
		},
		Joins: maps.NewInsertMap(
			maps.MapEntry[string, struct{}]{Key: model.TableNameTorrentContent},
		),
	}
}
