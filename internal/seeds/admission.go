package seeds

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// privacySQL is independent of the optional release-name policy. Public UDP
// trackers never receive known private hashes, including qualified bitgrab tags.
func privacySQL(root, whitespace string) string {
	return root + `.private=FALSE
 AND NOT EXISTS(SELECT 1 FROM label_evidence e WHERE e.info_hash=` + root + `.info_hash AND ` + catalogueguard.QBPrivacySQL("e.source", "e.category", whitespace) + `)
 AND NOT EXISTS(SELECT 1 FROM torrent_tags g WHERE g.info_hash=` + root + `.info_hash AND ` + catalogueguard.PrivacyTagSQL("g.name", whitespace) + `)
 AND NOT EXISTS(SELECT 1 FROM torrent_canonical_labels c WHERE c.info_hash=` + root + `.info_hash AND lower(btrim(c.category,` + whitespace + `)) IN('private','bitgrab'))`
}

func (s *Store) selectionAdmission() (string, []any) {
	sql := privacySQL("t", "$3")
	args := []any{catalogueguard.TagWhitespace}
	if s.names.Enabled() {
		allowed, nameArgs := s.names.AllowsSQL("t.name", "t.info_hash")
		// AllowsSQL uses question placeholders; this query uses pgx's numbered
		// parameters after minAge, limit and the shared whitespace argument.
		for i := range nameArgs {
			allowed = strings.Replace(allowed, "?", fmt.Sprintf("$%d", i+4), 1)
		}
		sql += " AND (" + allowed + ") AND NOT EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=t.info_hash AND c.content_type='xxx')"
		args = append(args, nameArgs...)
	}
	return sql, args
}

// withAdmission freezes only the minimum current facts around an egress or
// persistence operation. Table SHARE locks exclude independently inserted
// privacy and classification facts; parent SHARE locks exclude name/private
// changes. A source lookup failure is returned, never stamped as tracker-unknown.
// Network callers bound the complete transaction with the packet timeout.
func (s *Store) withAdmission(ctx context.Context, hashes [][]byte, write bool, use func(pgx.Tx, [][]byte) error) error {
	if len(hashes) == 0 {
		return nil
	}
	for _, hash := range hashes {
		if len(hash) != len(protocol.ID{}) {
			return fmt.Errorf("seeds admission: expected 20-byte hash")
		}
	}
	pool, err := s.pool.Get()
	if err != nil {
		return fmt.Errorf("seeds admission: acquire pool: %w", err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='2s'; LOCK TABLE label_evidence,torrent_canonical_labels,torrent_tags IN SHARE MODE`); err != nil {
		return err
	}
	if s.names.Enabled() {
		// Row locks cannot exclude a newly inserted XXX sibling. Keep the
		// existing classification table stable through this bounded operation.
		mode := "SHARE"
		if write {
			mode = "SHARE ROW EXCLUSIVE"
		}
		if _, err = tx.Exec(ctx, `LOCK TABLE torrent_contents IN `+mode+` MODE`); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT t.info_hash,t.name,`+privacySQL("t", "$2")+`,
 EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=t.info_hash AND c.content_type='xxx')
 FROM torrents t WHERE t.info_hash=ANY($1::bytea[]) ORDER BY t.info_hash FOR SHARE OF t`, hashes, catalogueguard.TagWhitespace)
	if err != nil {
		return err
	}
	allowedSet := map[string]bool{}
	for rows.Next() {
		var hash []byte
		var name string
		var public, adult bool
		if err = rows.Scan(&hash, &name, &public, &adult); err != nil {
			rows.Close()
			return err
		}
		var id protocol.ID
		copy(id[:], hash)
		classification := ""
		if adult {
			classification = "xxx"
		}
		decision := s.names.EvaluateClassified(id, name, classification)
		if !public {
			decision = namepolicy.Decision{Eligible: false, Reason: namepolicy.ReasonNotServed, Version: namepolicy.Version}
		}
		stage := "seeds_dispatch"
		if write {
			stage = "seeds_persist"
		}
		s.names.Observe(stage, decision)
		if decision.Eligible {
			allowedSet[string(hash)] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	allowed := make([][]byte, 0, len(allowedSet))
	for _, hash := range hashes {
		if allowedSet[string(hash)] {
			allowed = append(allowed, hash)
		}
	}
	if err = use(tx, allowed); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
