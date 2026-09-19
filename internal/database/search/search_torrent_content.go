package search

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
	"gorm.io/gen/field"
	"gorm.io/gorm/clause"
)

type TorrentContentResultItem struct {
	query.ResultItem
	model.TorrentContent
}

type TorrentContentResult = query.GenericResult[TorrentContentResultItem]

type TorrentContentSearch interface {
	TorrentContent(ctx context.Context, options ...query.Option) (TorrentContentResult, error)
}

func (s search) TorrentContent(ctx context.Context, options ...query.Option) (TorrentContentResult, error) {
	return query.GenericQuery[TorrentContentResultItem](
		ctx,
		s.q,
		query.Options(append([]query.Option{query.SelectAll()}, options...)...),
		model.TableNameTorrentContent,
		func(ctx context.Context, q *dao.Query) query.SubQuery {
			return query.GenericSubQuery[dao.ITorrentContentDo]{
				SubQuery: q.TorrentContent.WithContext(ctx).ReadDB(),
			}
		},
	)
}

func TorrentContentDefaultOption() query.Option {
	return query.Options(
		query.DefaultOption(),
		TorrentContentDefaultHydrate(),
		TorrentContentCoreJoins(),
		query.OrderBy(
			query.OrderByColumn{
				OrderByColumn: clause.OrderByColumn{
					Column: clause.Column{
						Table: clause.CurrentTable,
						Name:  "published_at",
					},
					Desc: true,
				},
			},
		),
	)
}

func TorrentContentCoreJoins() query.Option {
	return query.Options(
		query.Join(func(q *dao.Query) []query.TableJoin {
			return []query.TableJoin{
				{
					Table: q.Torrent,
					On: []field.Expr{
						q.TorrentContent.InfoHash.EqCol(q.Torrent.InfoHash),
					},
					Type: query.TableJoinTypeInner,
				},
				{
					Table: q.Content,
					On: []field.Expr{
						q.TorrentContent.ContentType.EqCol(q.Content.Type),
						q.TorrentContent.ContentSource.EqCol(q.Content.Source),
						q.TorrentContent.ContentID.EqCol(q.Content.ID),
					},
					Type: query.TableJoinTypeLeft,
				},
			}
		}),
		ContentCoreJoins(),
	)
}

func TorrentContentDefaultHydrate() query.Option {
	return query.Options(
		HydrateTorrentContentTorrent(),
		HydrateTorrentContentContent(),
	)
}

// torrentContentGroupKeySQL is the (content_type, content_source, content_id)
// grouping key — one distinct value per matched title.
const torrentContentGroupKeySQL = model.TableNameTorrentContent + ".content_type, " +
	model.TableNameTorrentContent + ".content_source, " +
	model.TableNameTorrentContent + ".content_id"

// torrentContentGroupInnerOrderSQL selects the representative torrent for each
// title: the group key (leading, as DISTINCT ON requires), then highest seeders
// (NULL treated as -1 so null-seeder torrents sort last, matching the existing
// seeders comparator in order_torrent_content.go), then info_hash as the final
// deterministic tiebreak (unique within a group, so the representative is a pure
// function of the current snapshot and offset pagination cannot dup/skip a
// title on account of a seeder tie).
const torrentContentGroupInnerOrderSQL = torrentContentGroupKeySQL +
	", COALESCE(" + model.TableNameTorrentContent + ".seeders, -1) DESC, " +
	model.TableNameTorrentContent + ".info_hash"

// TorrentContentGroupByContentOption collapses the search to one row per title
// (content), the representative being the highest-seeded torrent. It is
// matched-only: rows with no resolved content (content_id IS NULL) are excluded
// because grouping by a content key is undefined for them — the WHERE below is a
// global scope, so it also constrains the totalCount and the facet aggregations
// to the same matched corpus. Pair with the torrent_contents_content_group_idx
// covering index (migration 00036), whose column order mirrors the inner ORDER
// BY so Postgres feeds the Unique node from an ordered index scan.
func TorrentContentGroupByContentOption() query.Option {
	return query.Options(
		query.GroupByDistinctOn(torrentContentGroupKeySQL, torrentContentGroupInnerOrderSQL),
		query.Where(query.DBCriteria{
			SQL: model.TableNameTorrentContent + ".content_id IS NOT NULL",
		}),
	)
}
