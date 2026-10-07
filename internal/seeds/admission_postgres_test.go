package seeds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/types/infohash"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/evidence/liveness"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func admissionHash(i byte) []byte { return bytes.Repeat([]byte{i}, 20) }

func admissionRunner(t *testing.T, pool *pgxpool.Pool, enabled bool, excluded ...string) *Runner {
	t.Helper()
	p, err := namepolicy.New(namepolicy.Config{Enabled: enabled, ExcludedInfoHashes: excluded})
	require.NoError(t, err)
	cfg := NewDefaultConfig()
	cfg.BatchSize = 32
	cfg.MinRescrapeAge = 0
	cfg.TrackerUrls = []string{"udp://synthetic.invalid:80/announce"}
	cfg.ScrapeTimeout = 3 * time.Second
	cfg.PerTrackerInterval = 0
	lp := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	return NewRunner(cfg, lp, nil, liveness.NewStore(lp), zap.NewNop().Sugar(), p)
}

func admissionInsert(t *testing.T, pool *pgxpool.Pool, i byte, name string) []byte {
	t.Helper()
	h := admissionHash(i)
	_, err := pool.Exec(context.Background(), `insert into torrents(info_hash,name) values($1,$2)`, h, name)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `insert into torrent_contents(id,info_hash,content_type,seeders,leechers) values($1,$2,'movie',90,91)`, fmt.Sprint(i), h)
	require.NoError(t, err)
	return h
}

func admissionSnapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"torrents", "torrent_contents", "torrent_tracker_seeds", "torrents_torrent_sources", "torrent_liveness", "label_evidence", "torrent_tags", "torrent_canonical_labels"} {
		var body string
		err := pool.QueryRow(context.Background(), `select coalesce(jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text),'[]')::text from `+table+` r`).Scan(&body)
		require.NoError(t, err)
		out[table] = body
	}
	return out
}

func TestPostgresSeedsSelectionAndPacketAdmission(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			pool := seedsTestPool(t)
			names := []string{"Allowed.Release", "Synthetic.电影.ENG", "Synthetic.Фильм.English", "FetishXXX.Synthetic", "Owner.Configured.Exclusion", " \u2005 ", "Private.Raw", "Private.QB", "Bitgrab.QB", "Bitgrab.Tag", "Private.Canonical", "Adult.Sibling", "Public.Wanted", "Public.Manual"}
			for i, name := range names {
				admissionInsert(t, pool, byte(i+1), name)
			}
			ctx := context.Background()
			_, err := pool.Exec(ctx, `update torrents set private=true where info_hash=$1`, admissionHash(7))
			require.NoError(t, err)
			for _, row := range []struct {
				h                byte
				source, category string
			}{{8, "\u2005 QBittorrent \u2005", " PRIVATE "}, {9, "QBittorrent", "\u00a0 BITGRAB \u00a0"}} {
				_, err = pool.Exec(ctx, `insert into label_evidence values($1,$2,$3)`, admissionHash(row.h), row.source, row.category)
				require.NoError(t, err)
			}
			_, err = pool.Exec(ctx, `insert into torrent_tags values($1,' BITGRAB:user '),($2,'wanted:auto'),($3,'manual:user')`, admissionHash(10), admissionHash(13), admissionHash(14))
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `insert into torrent_canonical_labels values($1,' private '),($2,'movie')`, admissionHash(11), admissionHash(14))
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `insert into torrent_contents values('adult-sibling',$1,'xxx',100,100)`, admissionHash(12))
			require.NoError(t, err)
			var excluded protocol.ID
			copy(excluded[:], admissionHash(5))
			r := admissionRunner(t, pool, enabled, excluded.String())
			var sent [][]byte
			r.scraper.scrape = func(_ context.Context, _ string, hashes []infohash.T) ([]scrapeItem, error) {
				items := make([]scrapeItem, len(hashes))
				for i, h := range hashes {
					sent = append(sent, append([]byte(nil), h[:]...))
					items[i] = scrapeItem{Leechers: 2}
				}
				return items, nil
			}
			before := admissionSnapshot(t, pool)
			st, err := r.RunBatch(ctx, false)
			require.NoError(t, err)
			want := [][]byte{admissionHash(1), admissionHash(13), admissionHash(14)}
			if !enabled {
				for _, i := range []byte{2, 3, 4, 5, 6, 12} {
					want = append(want, admissionHash(i))
				}
			}
			require.ElementsMatch(t, want, sent)
			require.Equal(t, len(want), st.Selected)
			require.Equal(t, before, admissionSnapshot(t, pool), "dry-run and denied rows cannot gain new facts")
		})
	}
}

func TestPostgresSeedsLateAdmissionLeavesNoNewFacts(t *testing.T) {
	changes := []struct{ name, sql string }{
		{"name", `update torrents set name='Synthetic.电影' where info_hash=$1`},
		{"raw private", `update torrents set private=true where info_hash=$1`},
		{"normalized qB", `insert into label_evidence values($1,' QBittorrent ',' PRIVATE ')`},
		{"bitgrab tag", `insert into torrent_tags values($1,chr(8197)||' bitgrab:owned '||chr(8197))`},
		{"ANY adult sibling", `insert into torrent_contents values('late-xxx',$1,'xxx',80,80)`},
	}
	for _, when := range []string{"before packet", "after packet"} {
		for _, change := range changes {
			t.Run(when+"/"+change.name, func(t *testing.T) {
				pool := seedsTestPool(t)
				hash := admissionInsert(t, pool, 1, "Allowed.Release")
				r := admissionRunner(t, pool, true)
				var calls atomic.Int32
				r.scraper.scrape = func(context.Context, string, []infohash.T) ([]scrapeItem, error) {
					calls.Add(1)
					return []scrapeItem{{Leechers: 2}}, nil
				}
				original := r.scraper.admit
				var changed bool
				var expected map[string]string
				mutate := func() {
					_, err := pool.Exec(context.Background(), change.sql, hash)
					require.NoError(t, err)
					expected = admissionSnapshot(t, pool)
					changed = true
				}
				r.scraper.admit = func(ctx context.Context, h [][]byte, use func([][]byte) error) error {
					if when == "before packet" && !changed {
						mutate()
					}
					err := original(ctx, h, use)
					if when == "after packet" && !changed {
						mutate()
					}
					return err
				}
				st, err := r.RunBatch(context.Background(), true)
				require.NoError(t, err)
				wantCalls := int32(0)
				if when == "after packet" {
					wantCalls = 1
				}
				require.Equal(t, wantCalls, calls.Load())
				require.Equal(t, 1, st.Denied)
				require.Zero(t, st.SourcesUpserted)
				require.Zero(t, st.DenormSynced)
				require.Zero(t, st.LivenessSuspect)
				require.Zero(t, st.LivenessRevived)
				require.Equal(t, expected, admissionSnapshot(t, pool), "late authority change survives, but no scrape/derived/liveness fact is written")
			})
		}
	}
}

type failingLiveness struct{}

func (failingLiveness) MarkAliveBatchTx(context.Context, pgx.Tx, [][]byte, time.Time, string) (int64, error) {
	return 0, nil
}
func (failingLiveness) RecordSuspectBatchTx(context.Context, pgx.Tx, [][]byte, time.Time) (int64, error) {
	return 0, errors.New("synthetic liveness failure")
}

func TestPostgresSeedsAtomicPersistenceAndPositiveHistory(t *testing.T) {
	pool := seedsTestPool(t)
	hash := admissionInsert(t, pool, 1, "Allowed.Public.Wanted")
	_, err := pool.Exec(context.Background(), `insert into torrent_tags values($1,'wanted:auto')`, hash)
	require.NoError(t, err)
	r := admissionRunner(t, pool, true)
	r.scraper.scrape = func(context.Context, string, []infohash.T) ([]scrapeItem, error) {
		return []scrapeItem{{Leechers: 2}}, nil
	}
	before := admissionSnapshot(t, pool)
	r.liveness = failingLiveness{}
	st, err := r.RunBatch(context.Background(), true)
	require.Error(t, err)
	require.Zero(t, st.SourcesUpserted)
	require.Equal(t, before, admissionSnapshot(t, pool), "liveness failure rolls back ledger/source/denorm together")
	r.liveness = liveness.NewStore(r.store.pool)
	st, err = r.RunBatch(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, 1, st.SourcesUpserted)
	require.Equal(t, 1, st.DenormSynced)
	var checked time.Time
	var positive *time.Time
	require.NoError(t, pool.QueryRow(context.Background(), `select checked_at,last_positive_at from torrent_tracker_seeds where info_hash=$1`, hash).Scan(&checked, &positive))
	require.NotNil(t, positive)
	require.Equal(t, checked, *positive)
	var seeders, leechers int
	require.NoError(t, pool.QueryRow(context.Background(), `select seeders,leechers from torrent_contents where info_hash=$1`, hash).Scan(&seeders, &leechers))
	require.Zero(t, seeders)
	require.Equal(t, 2, leechers)
}

func TestPostgresPacketAdmissionHoldsCurrentFactsUntilSendCompletes(t *testing.T) {
	for _, change := range []struct{ name, sql string }{
		{"privacy", `insert into label_evidence values($1,'qbittorrent','private')`},
		{"new adult sibling", `insert into torrent_contents values('packet-xxx',$1,'xxx',4,4)`},
		{"name", `update torrents set name='Synthetic.电影' where info_hash=$1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			pool := seedsTestPool(t)
			hash := admissionInsert(t, pool, 1, "Allowed.Release")
			r := admissionRunner(t, pool, true)
			writer, err := pool.Acquire(context.Background())
			require.NoError(t, err)
			defer writer.Release()
			pid := writer.Conn().PgConn().PID()
			done := make(chan error, 1)
			var expected map[string]string
			r.scraper.scrape = func(context.Context, string, []infohash.T) ([]scrapeItem, error) {
				go func() { _, e := writer.Exec(context.Background(), change.sql, hash); done <- e }()
				require.Eventually(t, func() bool {
					var blocked bool
					e := pool.QueryRow(context.Background(), `select coalesce(wait_event_type='Lock',false) from pg_stat_activity where pid=$1`, pid).Scan(&blocked)
					return e == nil && blocked
				}, time.Second, 10*time.Millisecond)
				select {
				case e := <-done:
					t.Fatalf("source writer escaped the packet admission lock: %v", e)
				default:
				}
				return []scrapeItem{{Leechers: 2}}, nil
			}
			admit := r.scraper.admit
			r.scraper.admit = func(ctx context.Context, h [][]byte, use func([][]byte) error) error {
				err := admit(ctx, h, use)
				select {
				case e := <-done:
					require.NoError(t, e)
				case <-time.After(time.Second):
					t.Fatal("source writer did not resume after packet admission")
				}
				expected = admissionSnapshot(t, pool)
				return err
			}
			st, err := r.RunBatch(context.Background(), true)
			require.NoError(t, err)
			require.Equal(t, 1, st.Denied)
			require.Equal(t, expected, admissionSnapshot(t, pool), "fact committed after send must stop all later persistence")
		})
	}
}

func TestPostgresAdmissionFailureDoesNotBecomeUnknownObservation(t *testing.T) {
	pool := seedsTestPool(t)
	admissionInsert(t, pool, 1, "Allowed.Release")
	r := admissionRunner(t, pool, true)
	r.scraper.admit = func(context.Context, [][]byte, func([][]byte) error) error {
		return errors.New("synthetic source lookup failure")
	}
	r.scraper.scrape = func(context.Context, string, []infohash.T) ([]scrapeItem, error) {
		t.Fatal("failed source admission dispatched a packet")
		return nil, nil
	}
	before := admissionSnapshot(t, pool)
	st, err := r.RunBatch(context.Background(), true)
	require.Error(t, err)
	require.Zero(t, st.Unknown)
	require.Equal(t, before, admissionSnapshot(t, pool))
}

func TestCancelledScrapeBatchReturnsErrorBeforeNetwork(t *testing.T) {
	s := NewScraper(NewDefaultConfig(), nil, zap.NewNop().Sugar())
	s.scrape = func(context.Context, string, []infohash.T) ([]scrapeItem, error) {
		t.Fatal("cancelled batch made a packet call")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.ScrapeBatch(ctx, [][]byte{admissionHash(1)})
	require.ErrorIs(t, err, context.Canceled)
}

func TestPostgresQueuedPrivacyWriterPrecedesNextPacket(t *testing.T) {
	pool := seedsTestPool(t)
	hash := admissionInsert(t, pool, 1, "Allowed.Release")
	r := admissionRunner(t, pool, true)
	ctx := context.Background()
	writer, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer writer.Release()
	pid := writer.Conn().PgConn().PID()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- r.store.withAdmission(ctx, [][]byte{hash}, false, func(pgx.Tx, [][]byte) error { close(firstEntered); <-releaseFirst; return nil })
	}()
	<-firstEntered
	writeDone := make(chan error, 1)
	go func() {
		_, e := writer.Exec(ctx, `insert into label_evidence values($1,'qbittorrent','private')`, hash)
		writeDone <- e
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		e := pool.QueryRow(ctx, `select coalesce(wait_event_type='Lock',false) from pg_stat_activity where pid=$1`, pid).Scan(&blocked)
		return e == nil && blocked
	}, time.Second, 10*time.Millisecond)
	secondDone := make(chan error, 1)
	var secondPackets atomic.Int32
	go func() {
		secondDone <- r.store.withAdmission(ctx, [][]byte{hash}, false, func(_ pgx.Tx, allowed [][]byte) error {
			if len(allowed) > 0 {
				secondPackets.Add(1)
			}
			return nil
		})
	}()
	select {
	case e := <-secondDone:
		t.Fatalf("packet2 passed queued writer before packet1 released: %v", e)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-writeDone)
	require.NoError(t, <-secondDone)
	require.Zero(t, secondPackets.Load(), "fresh packet2 must see the queued privacy fact")
}

func TestPostgresDisjointPacketAdmissionsCanOverlap(t *testing.T) {
	pool := seedsTestPool(t)
	a := admissionInsert(t, pool, 1, "Allowed.A")
	b := admissionInsert(t, pool, 2, "Allowed.B")
	r := admissionRunner(t, pool, true)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	for _, hash := range [][]byte{a, b} {
		go func(h []byte) {
			done <- r.store.withAdmission(context.Background(), [][]byte{h}, false, func(pgx.Tx, [][]byte) error { entered <- struct{}{}; <-release; return nil })
		}(hash)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("disjoint packet admissions serialized")
		}
	}
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
}

func TestPostgresPacketCancellationReleasesSourceLocks(t *testing.T) {
	pool := seedsTestPool(t)
	hash := admissionInsert(t, pool, 1, "Allowed.Release")
	r := admissionRunner(t, pool, true)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.store.withAdmission(ctx, [][]byte{hash}, false, func(pgx.Tx, [][]byte) error { close(entered); <-ctx.Done(); return ctx.Err() })
	}()
	<-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	writerCtx, writerCancel := context.WithTimeout(context.Background(), time.Second)
	defer writerCancel()
	_, err := pool.Exec(writerCtx, `update torrents set private=true where info_hash=$1`, hash)
	require.NoError(t, err)
	var count int
	require.NoError(t, pool.QueryRow(writerCtx, `select count(*) from torrent_tracker_seeds`).Scan(&count))
	require.Zero(t, count)
}

func TestConcurrentLaterDenialCannotBeUndoneByEarlierMerge(t *testing.T) {
	pool := seedsTestPool(t)
	hash := admissionInsert(t, pool, 1, "Allowed.Release")
	r := admissionRunner(t, pool, true)
	a := NewScraper(r.cfg, nil, zap.NewNop().Sugar())
	b := NewScraper(r.cfg, nil, zap.NewNop().Sugar())
	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	a.admit = func(ctx context.Context, h [][]byte, use func([][]byte) error) error {
		err := r.scraper.admit(ctx, h, use)
		close(ready)
		<-release
		return err
	}
	b.admit = r.scraper.admit
	out := map[string]*ScrapeOutcome{string(hash): {}}
	withheld := map[string]bool{}
	var mu sync.Mutex
	var id infohash.T
	copy(id[:], hash)
	hs := []infohash.T{id}
	hashes := [][]byte{hash}
	success := func(context.Context, []infohash.T) ([]scrapeItem, error) { return []scrapeItem{{Leechers: 2}}, nil }
	gate := packetAdmission{withheld: withheld, fail: func(err error) { t.Error(err) }}
	go func() {
		defer close(done)
		a.scrapeTrackerChunks(context.Background(), "synthetic-a", hs, hashes, out, &mu, success, gate)
	}()
	<-ready
	_, err := pool.Exec(context.Background(), `update torrents set private=true where info_hash=$1`, hash)
	require.NoError(t, err)
	b.scrapeTrackerChunks(context.Background(), "synthetic-b", hs, hashes, out, &mu, func(context.Context, []infohash.T) ([]scrapeItem, error) {
		t.Fatal("denied packet dispatched")
		return nil, nil
	}, gate)
	close(release)
	<-done
	require.True(t, withheld[string(hash)])
	require.NotContains(t, out, string(hash))
}

func TestPostgresSlowSameHashTrackerDoesNotCancelHealthyFanout(t *testing.T) {
	pool := seedsTestPool(t)
	hash := admissionInsert(t, pool, 1, "Allowed.Release")
	r := admissionRunner(t, pool, true)
	r.cfg.Concurrency = 2
	r.scraper.cfg.Concurrency = 2
	r.scraper.cfg.ScrapeTimeout = 3 * time.Second
	r.scraper.cfg.TrackerUrls = []string{"synthetic-slow", "synthetic-next"}
	firstEntered := make(chan struct{})
	var calls atomic.Int32
	r.scraper.scrape = func(_ context.Context, _ string, _ []infohash.T) ([]scrapeItem, error) {
		if calls.Add(1) == 1 {
			close(firstEntered)
			time.Sleep(2200 * time.Millisecond)
		}
		return []scrapeItem{{Leechers: 2}}, nil
	}
	// Both trackers use the actual SAME hash packet; a slow valid first call
	// exceeds the former fixed2s admission wait without exceeding its3s RPC cap.
	outcomes, err := r.scraper.ScrapeBatch(context.Background(), [][]byte{hash})
	require.NoError(t, err)
	<-firstEntered
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, "positive", outcomes[string(hash)].Class())
}
