package search

import (
	"context"
	"gorm.io/gen"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
)

type TorrentFilesResult = query.GenericResult[model.TorrentFile]

type TorrentFilesSearch interface {
	TorrentFiles(ctx context.Context, options ...query.Option) (TorrentFilesResult, error)
}

func (s search) TorrentFiles(ctx context.Context, options ...query.Option) (TorrentFilesResult, error) {
	return query.GenericQuery[model.TorrentFile](
		ctx,
		s.q,
		query.Options(append([]query.Option{query.SelectAll()}, options...)...),
		model.TableNameTorrentFile,
		func(ctx context.Context, q *dao.Query) query.SubQuery {
			base := q.TorrentFile.WithContext(ctx).ReadDB()
			if s.adultPolicy != nil {
				predicate := s.adultPolicy.TorrentCondition(model.TableNameTorrentFile)
				base = base.Scopes(func(d gen.Dao) gen.Dao {
					do := d.(*gen.DO)
					do.ReplaceDB(do.UnderlyingDB().Where(predicate))
					return d
				})
			}
			return query.GenericSubQuery[dao.ITorrentFileDo]{
				SubQuery: base,
			}
		},
	)
}
