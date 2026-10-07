package query

import (
	"fmt"
	"strings"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// groupedPrefixItems tests an ordered, fully eligible candidate prefix rather
// than sorting every release before returning a small grouped page. A candidate
// is returned only when it is the exact highest-seeder/hash-ascending eligible
// representative of its group. Winners outside the prefix are never returned.
// A complete requested page is exact: every unseen representative is itself an
// unseen candidate, after this prefix. A short prefix is never exhaustion proof;
// expand or let the original whole-corpus query establish the final short page.
func (gq *genericQuery[T]) groupedPrefixItems() ([]T, bool, error) {
	b, ok := gq.builder.(optionBuilder)
	if !ok || b.grouping == nil || !b.grouping.candidatePrefix || b.tableName != model.TableNameTorrentContent || !b.limit.Valid ||
		b.limit.Uint == 0 || b.limit.Uint > 1000 || b.offset > 10000 || len(b.orderBy) == 0 || len(b.groupBy) > 0 {
		return nil, false, nil
	}
	keyNames := []string{"content_type", "content_source", "content_id"}
	qualifiedKeys := make([]string, len(keyNames))
	for i, key := range keyNames {
		qualifiedKeys[i] = b.tableName + "." + key
	}
	keySQL := strings.Join(qualifiedKeys, ", ")
	if b.grouping.distinctOnSQL != keySQL || b.grouping.innerOrderSQL != keySQL+
		", COALESCE("+b.tableName+".seeders, -1) DESC, "+b.tableName+".info_hash" {
		return nil, false, nil
	}
	// Joins with multiple rows per release could duplicate candidates. The
	// existing flat/grouped paths remain authoritative for those query shapes.
	required := b.requiredJoins.Copy()
	criteria, err := b.createFacetsFilterCriteria()
	if err != nil {
		return nil, false, err
	}
	raw, err := criteria.Raw(b)
	if err != nil {
		return nil, false, err
	}
	required.SetEntries(raw.Joins.Entries()...)
	for _, ob := range b.orderBy {
		for _, table := range ob.RequiredJoins {
			required.Set(table, struct{}{})
		}
	}
	joins, err := extractRequiredJoins(b.tableName, b.joins, required)
	if err != nil {
		return nil, false, err
	}
	for _, join := range joins {
		if !join.AtMostOne {
			return nil, false, nil
		}
		switch join.Table.TableName() {
		case model.TableNameTorrent, model.TableNameContent, model.TableNameMetadataSource:
		default:
			return nil, false, nil
		}
	}
	flat := b
	flat.grouping = nil
	flat.selections = []clause.Expr{{SQL: b.tableName + ".*"}}
	sq := gq.factory(gq.ctx, gq.daoQ)
	if err = flat.applySelect(sq.UnderlyingDB(), true); err != nil {
		return nil, false, err
	}
	if err = flat.applyPre(sq, true); err != nil {
		return nil, false, err
	}
	// A raw scope/factory join has no cardinality declaration. Only the
	// resolved, declared joins above may participate in the prefix path.
	// GORM defers scopes (including declared gen joins) until rendering.
	// Inspect that completed dry-run statement, never the pre-scope builder.
	var rendered *gorm.DB
	eligibleSQL := sq.UnderlyingDB().ToSQL(func(tx *gorm.DB) *gorm.DB {
		rendered = tx.Find(&[]interface{}{})
		return rendered
	})
	if rendered.Error != nil {
		return nil, false, rendered.Error
	}
	statement := rendered.Statement
	from, _ := statement.Clauses["FROM"].Expression.(clause.From)
	if len(statement.Joins) != 0 || len(from.Joins) != len(joins) {
		return nil, false, nil
	}
	winnerOrder := "COALESCE(winner.seeders,-1) DESC,winner.info_hash"
	// Separate NULL branches let each winner lookup use the ordered covering
	// group index, while retaining SQL NULL grouping semantics for every key.
	branches := make([]string, 0, 2)
	for _, sourceNull := range []bool{false, true} {
		conditions := []string{"winner.content_type=keys.content_type", "winner.content_id=keys.content_id"}
		if sourceNull {
			conditions = append(conditions, "keys.content_source IS NULL", "winner.content_source IS NULL")
		} else {
			conditions = append(conditions, "keys.content_source IS NOT NULL", "winner.content_source=keys.content_source")
		}
		branches = append(branches, "(SELECT winner.id FROM eligible winner WHERE "+
			strings.Join(conditions, " AND ")+" ORDER BY "+winnerOrder+" LIMIT 1)")
	}
	fresh := func() *gorm.DB {
		return gq.factory(gq.ctx, gq.daoQ).UnderlyingDB().Session(&gorm.Session{NewDB: true})
	}
	outer := fresh().Table("selected")
	if err = b.applyPost(outer); err != nil {
		return nil, false, err
	}
	outerSQL := dao.ToSQL(outer)
	wanted := int(b.limit.Uint)
	if b.nextPage {
		wanted++
	}
	prefix := 512
	if b.orderBy[0].Column.Table == model.TableNameTorrent && b.orderBy[0].Column.Name == "name" {
		prefix = 4096
		if b.offset > 0 {
			prefix = 8192
		}
	}
	for ; prefix <= 32768; prefix *= 2 {
		candidateBuilder := b
		candidateBuilder.offset = 0
		candidateBuilder.limit = model.NewNullUint(uint(prefix))
		candidateBuilder.nextPage = false
		candidateBuilder.preloads = nil
		candidates := fresh().Table("eligible")
		if err = candidateBuilder.applyPost(candidates); err != nil {
			return nil, false, err
		}
		keys := strings.Join(keyNames, ",")
		sql := "WITH eligible AS NOT MATERIALIZED (" + eligibleSQL +
			"), candidates AS MATERIALIZED (" + dao.ToSQL(candidates) +
			"), keys AS MATERIALIZED (SELECT DISTINCT " + keys + " FROM candidates), " +
			"winners AS MATERIALIZED (SELECT representative.id FROM keys JOIN LATERAL (" +
			strings.Join(branches, " UNION ALL ") + ") representative ON TRUE), " +
			"selected AS (SELECT candidate.* FROM candidates candidate JOIN winners ON winners.id=candidate.id) " + outerSQL
		var items []T
		if err = fresh().Raw(sql).Scan(&items).Error; err != nil {
			return nil, false, fmt.Errorf("grouped candidate prefix: %w", err)
		}
		if len(items) >= wanted {
			return items, true, nil
		}
		if err = gq.ctx.Err(); err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}
