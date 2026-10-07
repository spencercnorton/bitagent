package releasefields

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestPostgresRepairNormalizesIndependentPrivacyBeforeFreezeAndAfterPlan(t *testing.T) {
	for _, source := range []string{"qbittorrent", " \tQBITTORRENT\u3000"} {
		for _, category := range []string{" \tPRIVATE\u3000", "\u0085BiTgRaB\u202f"} {
			for _, late := range []bool{false, true} {
				t.Run(source+category, func(t *testing.T) {
					pool, hash := repairFixture(t)
					ctx := context.Background()
					var plan Plan
					var err error
					if late {
						plan, err = Freeze(ctx, pool, []protocol.ID{hash}, false)
						require.NoError(t, err)
					}
					_, err = pool.Exec(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength)
VALUES($1,'synthetic-privacy','fixture','synthetic-owner', $2,$3,now(),1)`, source, hash.Bytes(), category)
					require.NoError(t, err)
					snapshot := func() map[string]string {
						out := map[string]string{}
						for _, table := range []string{"torrents", "torrent_files", "torrent_contents", "torrent_tags", "label_evidence", "llm_work_tasks", "llm_work_applications", "llm_capture_dispatch_attempts", "llm_capture_dispatch_aliases", "release_field_repair_journal"} {
							var raw string
							require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text FROM `+pgx.Identifier{table}.Sanitize()+` r`).Scan(&raw))
							out[table] = raw
						}
						return out
					}
					before := snapshot()
					_, err = Freeze(ctx, pool, []protocol.ID{hash}, false)
					require.ErrorIs(t, err, ErrChanged)
					if late {
						for _, write := range []bool{false, true} {
							_, err = Apply(ctx, pool, plan, write)
							require.ErrorIs(t, err, ErrChanged, "independent late privacy cannot be waived by unchanged source digest")
						}
					}
					require.Equal(t, before, snapshot(), "privacy refusal retains raw data, fields, evidence, applications and permanent fences")
				})
			}
		}
	}
}

func TestPostgresRepairRechecksNewNamePolicyBeforeApplyingFrozenPlan(t *testing.T) {
	pool, hash := repairFixture(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE torrents SET name='Synthetic.测试.2026.WEB-DL.HEVC.mkv' WHERE info_hash=$1`, hash.Bytes())
	require.NoError(t, err)
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err, "default-off policy preserves original repair behavior")
	policy, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	_, err = Freeze(ctx, pool, []protocol.ID{hash}, false, policy)
	require.ErrorIs(t, err, namepolicy.ErrExcluded)
	for _, write := range []bool{false, true} {
		_, err = Apply(ctx, pool, plan, write, policy)
		require.ErrorIs(t, err, namepolicy.ErrExcluded, "configuration activation must deny an already frozen source-identical plan")
	}
	var journal int
	var codec, attrs []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM release_field_repair_journal),to_jsonb(video_codec),release_attributes FROM torrent_contents WHERE info_hash=$1`, hash.Bytes()).Scan(&journal, &codec, &attrs))
	require.Zero(t, journal)
	require.Nil(t, codec)
	require.Nil(t, attrs)
}
