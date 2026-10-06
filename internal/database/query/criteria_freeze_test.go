package query

import (
	"sync"
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCriteriaFreezePreservesEmptyNestedAndDeferredWhere(t *testing.T) {
	db := newDryRunDB(t).Model(&model.Torrent{}).Scopes(func(tx *gorm.DB) *gorm.DB {
		return tx.Where("torrents.private = ?", false)
	})
	q := dao.Use(db)
	ctx := dbContext{q: q, tableName: "torrents"}
	for _, criteria := range []Criteria{
		And(), Or(), Not(),
		And(Or(DBCriteria{SQL: "torrents.size > ?", Args: []interface{}{100}}, DBCriteria{SQL: "torrents.size < ?", Args: []interface{}{20}}), Not(DBCriteria{SQL: "torrents.name = ?", Args: []interface{}{"SyntheticExcluded"}})),
	} {
		raw, err := criteria.Raw(ctx)
		require.NoError(t, err)
		_, mutable := raw.Query.(*gorm.DB)
		require.False(t, mutable)
		sql := db.Session(&gorm.Session{DryRun: true, NewDB: true}).Model(&model.Torrent{}).Where(raw.Query, raw.Args...).Find(&[]model.Torrent{}).Statement.SQL.String()
		require.Contains(t, sql, "torrents.private")
		if _, nested := criteria.(AndCriteria); nested && len(criteria.(AndCriteria).Criteria) > 0 {
			require.Contains(t, sql, "torrents.size >")
			require.Contains(t, sql, "torrents.size <")
			require.Contains(t, sql, "NOT torrents.name")
		}
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fresh := db.Session(&gorm.Session{DryRun: true, NewDB: true}).Model(&model.Torrent{}).Where(raw.Query, raw.Args...).Find(&[]model.Torrent{})
				require.NoError(t, fresh.Error)
				require.Equal(t, sql, fresh.Statement.SQL.String())
			}()
		}
		wg.Wait()
	}
}
