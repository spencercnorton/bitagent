package releasefields

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestPostgresModernNullCodecRepairFiltersIdentityAndRollback(t *testing.T) {
	for _, claim := range []string{"HEVC", "H265", "H.265", "AV1"} {
		t.Run(claim, func(t *testing.T) {
			pool, hash := repairFixture(t)
			ctx := context.Background()
			name := "Amber.Signal.2026.2160p.WEB-DL." + claim + "-GROUP.mkv"
			_, err := pool.Exec(ctx, `UPDATE torrents SET name=$1`, name)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at)VALUES('movie','tmdb','42','Amber Signal',2026,to_tsvector('simple','Amber Signal'),now(),now());UPDATE torrent_contents SET content_source='tmdb',content_id='42',video_resolution='V1080p',release_group='RETAINED',tsv=to_tsvector('simple','Amber Signal')`)
			require.NoError(t, err)
			plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
			require.NoError(t, err)
			expected := model.VideoCodecHEVC
			if claim == "AV1" {
				expected = model.VideoCodecAV1
			}
			require.False(t, plan.Entries[0].Before.VideoCodec.Valid)
			require.Equal(t, model.NewNullVideoCodec(expected), plan.Entries[0].After.VideoCodec)
			out, err := Apply(ctx, pool, plan, false)
			require.NoError(t, err)
			require.Equal(t, "would_apply", out[0].State)
			var codec model.NullVideoCodec
			require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec FROM torrent_contents`).Scan(&codec))
			require.False(t, codec.Valid, "dry-run may not fill the codec")
			_, err = Apply(ctx, pool, plan, true)
			require.NoError(t, err)
			out, err = Apply(ctx, pool, plan, true)
			require.NoError(t, err)
			require.Equal(t, "already_applied", out[0].State)
			var contentID, resolution, group string
			var title, indexed bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec,content_id,video_resolution,release_group,tsv@@plainto_tsquery('simple','Amber Signal'),tsv@@plainto_tsquery('simple',$1) FROM torrent_contents`, expected.String()).Scan(&codec, &contentID, &resolution, &group, &title, &indexed))
			require.Equal(t, model.NewNullVideoCodec(expected), codec)
			require.Equal(t, "42", contentID)
			require.Equal(t, "V1080p", resolution)
			require.Equal(t, "RETAINED", group)
			require.True(t, title)
			require.True(t, indexed)
			var gql bytes.Buffer
			codec.MarshalGQL(&gql)
			require.Equal(t, fmt.Sprintf("%q", expected.String()), gql.String())

			sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
			defer sqlDB.Close()
			gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
			require.NoError(t, err)
			backend, err := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return dao.Use(gdb), nil })}).Search.Get()
			require.NoError(t, err)
			filtered, err := backend.TorrentContent(ctx, query.WithFacet(search.VideoCodecFacet(query.FacetHasFilter(query.FacetFilter{expected.String(): {}}))))
			require.NoError(t, err)
			require.Len(t, filtered.Items, 1)
			require.Equal(t, hash, filtered.Items[0].InfoHash)
			require.Equal(t, codec, filtered.Items[0].VideoCodec)
			_, err = Rollback(ctx, pool, plan, true)
			require.NoError(t, err)
			filtered, err = backend.TorrentContent(ctx, query.WithFacet(search.VideoCodecFacet(query.FacetHasFilter(query.FacetFilter{expected.String(): {}}))))
			require.NoError(t, err)
			require.Empty(t, filtered.Items, "rollback removes the codec filter membership")
			require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec,content_id,tsv@@plainto_tsquery('simple','Amber Signal') FROM torrent_contents`).Scan(&codec, &contentID, &title))
			require.False(t, codec.Valid)
			require.Equal(t, "42", contentID)
			require.True(t, title)
		})
	}
}

func TestPostgresCodecRepairRetainsEveryExistingValueAndRejectsForgedChanges(t *testing.T) {
	for _, codec := range model.VideoCodecValues() {
		t.Run(codec.String(), func(t *testing.T) {
			pool, hash := repairFixture(t)
			ctx := context.Background()
			_, err := pool.Exec(ctx, `UPDATE torrent_contents SET video_codec=$1`, codec.String())
			require.NoError(t, err)
			plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
			require.NoError(t, err)
			require.Equal(t, model.NewNullVideoCodec(codec), plan.Entries[0].After.VideoCodec)
			_, err = Apply(ctx, pool, plan, true)
			require.NoError(t, err)
			var stored model.NullVideoCodec
			require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec FROM torrent_contents`).Scan(&stored))
			require.Equal(t, model.NewNullVideoCodec(codec), stored)
			forged := plan
			forged.Entries = append([]Entry(nil), plan.Entries...)
			forged.Entries[0].After.VideoCodec = model.NullVideoCodec{}
			_, err = Apply(ctx, pool, forged, true)
			require.ErrorIs(t, err, ErrChanged)
		})
	}
}

func TestPostgresCodecRepairContradictionsSidecarsAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		held       bool
		codec      model.VideoCodec
	}{
		{"Amber.Signal.2026.HEVC.AV1.mkv", "", false, ""},
		{"Amber.Signal.2026.HEVC.mkv", "episode.x264.mkv", true, ""},
		{"Amber.Signal.2026.HEVC.mkv", "episode.H265.m4v", false, model.VideoCodecHEVC},
		{"Amber.Signal.2026.HEVC.mkv", "episode.x265.webm", false, model.VideoCodecHEVC},
		{"Amber.Signal.2026.HEVC.mkv", "episode.H264.rmvb", true, ""},
		{"Amber.Signal.2026.HEVC.mkv", "episode.AV1.ogm", true, ""},
		{"Amber.Signal.2026.HEVC.mkv", "notes.x264.txt", false, model.VideoCodecHEVC},
		{"Amber.Signal.2026.HEVC.mkv", "x264/episode.mkv", false, model.VideoCodecHEVC},
		{"Amber.Signal.2026.mkv", "episode.AV1.mkv", false, ""},
	} {
		t.Run(tc.name+"/"+tc.file, func(t *testing.T) {
			pool, hash := repairFixture(t)
			ctx := context.Background()
			_, err := pool.Exec(ctx, `UPDATE torrents SET name=$1`, tc.name)
			require.NoError(t, err)
			if tc.file != "" {
				_, err = pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)VALUES($1,0,$2,734003200,now(),now())`, hash.Bytes(), tc.file)
				require.NoError(t, err)
			}
			plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
			if tc.held {
				require.ErrorIs(t, err, ErrChanged)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.codec, plan.Entries[0].After.VideoCodec.VideoCodec)
				_, err = Apply(ctx, pool, plan, true)
				require.NoError(t, err)
			}
			var stored model.NullVideoCodec
			require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec FROM torrent_contents`).Scan(&stored))
			require.Equal(t, tc.codec, stored.VideoCodec)
		})
	}
}

func TestPostgresCodecRepairRejectsFileCodecAndApplicationDriftAtomically(t *testing.T) {
	for _, mutation := range []string{
		`INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)VALUES($1,0,'Changed.HEVC.mkv',1,now(),now())`,
		`UPDATE torrent_contents SET video_codec='AV1'`,
		`INSERT INTO torrent_tags(info_hash,name,created_at,updated_at)VALUES($1,'manual',now(),now())`,
		`UPDATE torrents SET private=true`,
		`UPDATE llm_work_applications SET applied_snapshot=jsonb_set(applied_snapshot,'{videoCodec}','"AV1"'::jsonb)`,
	} {
		t.Run(mutation, func(t *testing.T) {
			pool, hash := repairFixture(t)
			ctx := context.Background()
			task := digest("synthetic codec application")
			_, err := pool.Exec(ctx, `INSERT INTO llm_work_tasks(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,priority,time_bucket,daily_limit,monthly_limit,state,expires_at,completed_at)VALUES($1,'classifier_type',$2,$1,$1,$1,$1,'{}',0,0,0,0,'completed',now()+interval '1 day',now())`, task, hash.Bytes())
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot)VALUES($1,$2,$1,$1,'{"schema":"llm-application-v1","videoSource":"WEBDL","videoCodec":null}')`, task, hash.Bytes())
			require.NoError(t, err)
			plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
			require.NoError(t, err)
			if strings.Contains(mutation, "$1") {
				_, err = pool.Exec(ctx, mutation, hash.Bytes())
			} else {
				_, err = pool.Exec(ctx, mutation)
			}
			require.NoError(t, err)
			_, err = Apply(ctx, pool, plan, true)
			require.ErrorIs(t, err, ErrChanged)
			var journal int
			var attrs []byte
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM release_field_repair_journal`).Scan(&journal))
			require.Zero(t, journal)
			require.NoError(t, pool.QueryRow(ctx, `SELECT release_attributes FROM torrent_contents`).Scan(&attrs))
			require.Empty(t, attrs)
		})
	}
}

func TestPostgresCodecRepairApplicationJournalAndConditionalRollback(t *testing.T) {
	pool, hash := repairFixture(t)
	ctx := context.Background()
	task := digest("synthetic codec rollback application")
	original := `{"schema":"llm-application-v1","videoSource":"WEBDL","videoCodec":null,"contentId":"42"}`
	_, err := pool.Exec(ctx, `INSERT INTO llm_work_tasks(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,priority,time_bucket,daily_limit,monthly_limit,state,expires_at,completed_at)VALUES($1,'classifier_type',$2,$1,$1,$1,$1,'{}',0,0,0,0,'completed',now()+interval '1 day',now())`, task, hash.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot)VALUES($1,$2,$1,$1,$3::jsonb)`, task, hash.Bytes(), original)
	require.NoError(t, err)
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	_, err = Apply(ctx, pool, plan, true)
	require.NoError(t, err)
	var before, after, current []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT application_before->$1,application_after->$1 FROM release_field_repair_journal`, fmt.Sprintf("%x", task)).Scan(&before, &after))
	require.JSONEq(t, original, string(before))
	require.Contains(t, string(after), `"videoCodec": "HEVC"`)
	require.Contains(t, string(after), `"contentId": "42"`)
	_, err = Rollback(ctx, pool, plan, true)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT applied_snapshot FROM llm_work_applications`).Scan(&current))
	require.JSONEq(t, original, string(current))
	var codec model.NullVideoCodec
	require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec FROM torrent_contents`).Scan(&codec))
	require.False(t, codec.Valid)

	plan, err = Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	_, err = Apply(ctx, pool, plan, true)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_work_applications SET applied_snapshot=jsonb_set(applied_snapshot,'{videoCodec}','"AV1"'::jsonb)`)
	require.NoError(t, err)
	_, err = Rollback(ctx, pool, plan, true)
	require.ErrorIs(t, err, ErrChanged)
	require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec FROM torrent_contents`).Scan(&codec))
	require.Equal(t, model.NewNullVideoCodec(model.VideoCodecHEVC), codec, "failed rollback must leave all committed fields intact")
}

func TestPostgresCodecRepairHoldsUnownedSourceAndRejectsInventedCodec(t *testing.T) {
	pool, hash := repairFixture(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE torrent_contents SET video_source='WEBRip'`)
	require.NoError(t, err)
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	require.Equal(t, model.NewNullVideoCodec(model.VideoCodecHEVC), plan.Entries[0].After.VideoCodec)
	_, err = Apply(ctx, pool, plan, true)
	require.ErrorIs(t, err, ErrChanged, "codec repair cannot authorize a simultaneous source correction")
	plan.Entries[0].SourceOwnershipEvidence = "reviewed synthetic source"
	plan.Entries[0].After.VideoCodec = model.NewNullVideoCodec(model.VideoCodecAV1)
	_, err = Apply(ctx, pool, plan, true)
	require.ErrorIs(t, err, ErrChanged, "a reviewed source does not permit an invented codec")
	var codec model.NullVideoCodec
	require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec FROM torrent_contents`).Scan(&codec))
	require.False(t, codec.Valid)
}
