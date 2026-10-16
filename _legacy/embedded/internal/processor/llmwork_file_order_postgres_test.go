package processor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresDeferredTypeSurvivesShuffledFilePreload(t *testing.T) {
	h, work, lease, stage, pool, source, calls := deferredTypeHarness(t, nil, false)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM llm_work_events;DELETE FROM llm_work_tasks`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)VALUES($1,1,'Amber.Signal.2026.eng.srt',1000,now(),now())`, source.InfoHash.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE torrents SET files_count=2 WHERE info_hash=$1`, source.InfoHash.Bytes())
	require.NoError(t, err)
	backend, err := h.Search.Get()
	require.NoError(t, err)
	source = deferredTaskSource(t, backend, source.InfoHash)
	require.Len(t, source.Files, 2)
	source.Files = append([]model.TorrentFile(nil), source.Files...)
	source.Files[0], source.Files[1] = source.Files[1], source.Files[0]
	p := llmstage.WorkPayload{Workflow: "default", Flags: classifier.Flags{"local_search_enabled": false}}
	body, err := json.Marshal(p)
	require.NoError(t, err)
	_, err = work.Enqueue(ctx, llmwork.Draft{Kind: llmwork.Type, InfoHash: source.InfoHash.Bytes(), SourceDigest: llmwork.SourceDigest(source), PolicyDigest: llmwork.Digest(stage.WorkPolicy(p)), InputDigest: stage.WorkInputDigest(source), FamilyDigest: llmwork.Digest("shuffled source"), Payload: body, DailyLimit: 20, MonthlyLimit: 20})
	require.NoError(t, err)
	lease, err = work.Claim(ctx, "cold-source")
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NoError(t, h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task))
	require.EqualValues(t, 1, calls.Load(), "sorted cold reload must accept the original task without another case")
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks`).Scan(&state))
	require.Equal(t, "completed", state)
}
