package qbpoller

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// synthetic is a generated single-file release: its .torrent, info hash and
// the piece table a stored row must equal.
type synthetic struct {
	torrent     []byte
	hash        []byte
	pieceLength int64
	pieces      []byte
}

func newSynthetic(t *testing.T, name string, private bool) synthetic {
	t.Helper()
	const pieceLength = 16384
	content := bytes.Repeat([]byte(name), 40000/len(name)+1)
	var pieces []byte
	for off := 0; off < len(content); off += pieceLength {
		sum := sha1.Sum(content[off:min(off+pieceLength, len(content))])
		pieces = append(pieces, sum[:]...)
	}
	fields := map[string]any{"name": name + ".mkv", "length": len(content), "piece length": pieceLength, "pieces": pieces}
	if private {
		fields["private"] = 1
	}
	info, err := bencode.Marshal(fields)
	require.NoError(t, err)
	torrent, err := bencode.Marshal(map[string]any{"announce": "udp://tracker.invalid:6969", "info": bencode.Bytes(info)})
	require.NoError(t, err)
	hash := sha1.Sum(info)
	return synthetic{torrent: torrent, hash: hash[:], pieceLength: pieceLength, pieces: pieces}
}

// fakeQB answers torrents/export from a hash -> response table and counts
// export requests per hash.
type fakeQB struct {
	mu       sync.Mutex
	bodies   map[string][]byte
	statuses map[string]int
	exports  map[string]int
}

func (f *fakeQB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v2/torrents/export" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	hash := r.URL.Query().Get("hash")
	f.exports[hash]++
	if status := f.statuses[hash]; status != 0 {
		w.WriteHeader(status)
		return
	}
	body, ok := f.bodies[hash]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	_, _ = w.Write(body)
}

func newFakeQB(t *testing.T) (*fakeQB, *client) {
	t.Helper()
	f := &fakeQB{bodies: map[string][]byte{}, statuses: map[string]int{}, exports: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := newClient(evidence.ConfiguredQBInstance{Name: "qb-test", BaseURL: srv.URL})
	require.NoError(t, err)
	return f, c
}

func TestExportTorrentStatuses(t *testing.T) {
	f, c := newFakeQB(t)
	f.bodies["aa"] = []byte("d4:infod4:name1:xee")
	f.statuses["bb"] = http.StatusConflict
	ctx := context.Background()

	body, err := c.exportTorrent(ctx, "aa")
	require.NoError(t, err)
	require.Equal(t, []byte("d4:infod4:name1:xee"), body)

	_, err = c.exportTorrent(ctx, "bb")
	require.ErrorIs(t, err, errNoMetadata, "409 means the metadata is not downloaded yet")

	_, err = c.exportTorrent(ctx, "cc")
	require.Error(t, err)
	require.False(t, errors.Is(err, errNoMetadata), "404 is a real failure, not a pending export")
}

// retainFixture builds a disposable schema with the real torrent_pieces
// migration and only the columns the retention and privacy queries read.
func retainFixture(t *testing.T) (*poller, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("qbpoller_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		admin.Close()
	})
	// torrents keeps the pre-00013 piece columns so the migration applies unchanged.
	_, err = pool.Exec(ctx, `CREATE TABLE torrents(info_hash bytea PRIMARY KEY, private boolean NOT NULL DEFAULT false,
   piece_length bigint, pieces bytea, created_at timestamptz NOT NULL DEFAULT now());
 CREATE TABLE label_evidence(info_hash bytea, source text NOT NULL, source_kind text NOT NULL, category text);
 CREATE TABLE torrent_tags(info_hash bytea NOT NULL, name text NOT NULL);
 CREATE TABLE torrent_canonical_labels(info_hash bytea PRIMARY KEY, category text);`)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "migrations", "00013_torrent_pieces.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, strings.Split(string(body), "-- +goose Down")[0])
	require.NoError(t, err)
	store := evidence.NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	return &poller{store: store}, pool
}

func TestRetainPiecesPostgres(t *testing.T) {
	p, pool := retainFixture(t)
	f, c := newFakeQB(t)
	ctx := context.Background()
	log := zap.NewNop().Sugar()

	grabbed := newSynthetic(t, "Grabbed.Release", false)
	imported := newSynthetic(t, "Imported.Release", false)
	unrequested := newSynthetic(t, "Unrequested.Release", false)
	privateCat := newSynthetic(t, "Bitgrab.Release", false)
	privateTag := newSynthetic(t, "Tagged.Release", false)
	tampered := newSynthetic(t, "Tampered.Release", false)
	other := newSynthetic(t, "Other.Release", false)
	uncatalogued := newSynthetic(t, "Uncatalogued.Release", false)
	privateFlag := newSynthetic(t, "Flagged.Release", true)

	catalogue := []synthetic{grabbed, imported, unrequested, privateCat, privateTag, tampered, privateFlag}
	for _, s := range catalogue {
		// privateFlag's row claims public on purpose: its own info dictionary must win.
		_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash) VALUES($1)`, s.hash)
		require.NoError(t, err)
	}
	arr := func(s synthetic, kind evidence.Kind) {
		_, err := pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,source_kind) VALUES($1,'sonarr',$2)`, s.hash, string(kind))
		require.NoError(t, err)
	}
	arr(grabbed, evidence.KindWebhookGrab)
	arr(imported, evidence.KindPollHistory)
	arr(privateCat, evidence.KindWebhookImport)
	arr(privateTag, evidence.KindWebhookGrab)
	arr(tampered, evidence.KindWebhookGrab)
	arr(uncatalogued, evidence.KindWebhookGrab)
	arr(privateFlag, evidence.KindWebhookGrab)
	_, err := pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,source_kind,category) VALUES
   ($1,'qbittorrent','poll_categories','tv-sonarr'), ($2,'qbittorrent','poll_categories',' Bitgrab ')`, unrequested.hash, privateCat.hash)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_tags(info_hash,name) VALUES($1,'bitgrab:manual')`, privateTag.hash)
	require.NoError(t, err)

	held := []synthetic{grabbed, imported, unrequested, privateCat, privateTag, tampered, uncatalogued, privateFlag}
	torrents := make([]qbTorrent, 0, len(held))
	for _, s := range held {
		torrents = append(torrents, qbTorrent{Hash: hex.EncodeToString(s.hash)})
		f.bodies[hex.EncodeToString(s.hash)] = s.torrent
	}
	f.bodies[hex.EncodeToString(tampered.hash)] = other.torrent // valid .torrent, wrong info hash
	f.statuses[hex.EncodeToString(imported.hash)] = http.StatusConflict

	stored := func() map[string][]byte {
		rows, err := pool.Query(ctx, `SELECT info_hash, piece_length, pieces FROM torrent_pieces`)
		require.NoError(t, err)
		out := map[string][]byte{}
		for rows.Next() {
			var hash, pieces []byte
			var pieceLength int64
			require.NoError(t, rows.Scan(&hash, &pieceLength, &pieces))
			require.Equal(t, int64(16384), pieceLength)
			out[hex.EncodeToString(hash)] = pieces
		}
		require.NoError(t, rows.Err())
		return out
	}

	p.retainPieces(ctx, c, torrents, log)
	got := stored()
	require.Equal(t, map[string][]byte{hex.EncodeToString(grabbed.hash): grabbed.pieces}, got,
		"only the public, catalogued, *arr-grabbed torrent with a matching .torrent is kept")
	for _, s := range []synthetic{unrequested, privateCat, privateTag, uncatalogued} {
		require.Zero(t, f.exports[hex.EncodeToString(s.hash)], "never exported: not an *arr grab, private or not catalogued")
	}

	// Next cycle: the pending metadata has arrived; the kept row is not re-exported.
	delete(f.statuses, hex.EncodeToString(imported.hash))
	p.retainPieces(ctx, c, torrents, log)
	got = stored()
	require.Len(t, got, 2)
	require.Equal(t, imported.pieces, got[hex.EncodeToString(imported.hash)])
	require.Equal(t, 1, f.exports[hex.EncodeToString(grabbed.hash)])
	require.Equal(t, 2, f.exports[hex.EncodeToString(tampered.hash)], "a mismatched .torrent is retried, never stored")
	require.Equal(t, 2, f.exports[hex.EncodeToString(privateFlag.hash)], "a private info dictionary is never stored")
}
