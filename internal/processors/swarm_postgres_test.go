package processors

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func seedSwarm(t *testing.T, p *pgxpool.Pool, h []byte, bestTracker string) {
	t.Helper()
	_, err := p.Exec(testContext, `
INSERT INTO torrents_torrent_sources(source,info_hash,seeders,leechers,created_at,updated_at) VALUES
 ('dht',$1,7,2,now(),now()),
 ('tracker',$1,42,3,now(),now());
INSERT INTO torrent_tracker_seeds(info_hash,tracker_known,seeders,leechers,completed,prev_seeders,peak_seeders,best_tracker,last_positive_at,checked_at)
 VALUES($1,true,42,3,100,40,55,$2,now(),now());
INSERT INTO torrent_liveness(info_hash,status,last_qb_state,suspect_observations,last_observed_at,alive_source)
 VALUES($1,'alive','stalledUP',0,now(),'tracker_scrape');
INSERT INTO grab_outcome_priors(key_type,key_value,alpha,beta) VALUES
 ('codec','x264',10,2),
 ('quality_tag','webdl',20,3),
 ('release_group','group',369,24),
 ('resolution','1080p',30,4)
ON CONFLICT(key_type,key_value) DO UPDATE SET alpha=excluded.alpha,beta=excluded.beta;`, h, bestTracker)
	require.NoError(t, err)
}

func TestPostgresSwarmEnrichesPreparedJobAndCoreLiveness(t *testing.T) {
	s, p, _ := fixture(t)
	service := newService(t, "swarm-enrich", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, service)
	h := seedTorrent(t, p, 97, nil)
	seedSwarm(t, p, h, "udp://tracker.opentrackr.org:1337/announce")
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	var storedRevision string
	require.NoError(t, p.QueryRow(testContext, `SELECT input_revision FROM processor_jobs`).Scan(&storedRevision))

	lease, err := s.Claim(testContext)
	require.NoError(t, err)
	require.NotNil(t, lease)
	// Volatile swarm counts never enter the stored source revision.
	require.Equal(t, storedRevision, lease.InputRevision)
	j, err := s.Prepare(testContext, *lease)
	require.NoError(t, err)
	require.NoError(t, j.Validate())
	require.Len(t, j.Input.Sources, 2)
	require.NotNil(t, j.Input.Swarm)
	sw := j.Input.Swarm
	require.True(t, sw.TrackerScraped)
	require.True(t, sw.TrackerKnown)
	require.NotNil(t, sw.TrackerSeeders)
	require.Equal(t, 42, *sw.TrackerSeeders)
	require.NotNil(t, sw.DHTSeeders)
	require.Equal(t, 7, *sw.DHTSeeders)
	require.Equal(t, "udp://tracker.opentrackr.org:1337/announce", sw.BestTracker)
	require.Equal(t, "alive", sw.LivenessStatus)
	require.Equal(t, "tracker_scrape", sw.AliveSource)
	require.NotEmpty(t, sw.OutcomePriors)
	priorKeys := make(map[string]bool)
	for _, pr := range sw.OutcomePriors {
		priorKeys[pr.KeyType+":"+pr.KeyValue] = true
		require.NotEqual(t, "source", pr.KeyType)
	}
	require.True(t, priorKeys["release_group:group"])
	require.True(t, priorKeys["codec:x264"])

	resp, err := s.CoreLiveness(testContext, v1.CoreLivenessRequest{InfoHash: hex.EncodeToString(h)})
	require.NoError(t, err)
	require.Equal(t, "alive", resp.Liveness.Observation)
	require.NotNil(t, resp.Liveness.Seeders)
	require.Equal(t, 42, *resp.Liveness.Seeders)
	require.NotNil(t, resp.Liveness.Completed)
	require.Equal(t, 100, *resp.Liveness.Completed)
	require.Equal(t, 1, resp.TrackerCount)
}

func TestPostgresSwarmOmitsPrivateTrackerAndStillValidates(t *testing.T) {
	s, p, _ := fixture(t)
	service := newService(t, "swarm-private", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, service)
	h := seedTorrent(t, p, 98, nil)
	seedSwarm(t, p, h, "https://tracker.example.org/announce?passkey=secret-token")
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	lease, err := s.Claim(testContext)
	require.NoError(t, err)
	j, err := s.Prepare(testContext, *lease)
	require.NoError(t, err)
	require.NoError(t, j.Validate())
	require.NotNil(t, j.Input.Swarm)
	require.Empty(t, j.Input.Swarm.BestTracker)
	raw, err := json.Marshal(j)
	require.NoError(t, err)
	body := strings.ToLower(string(raw))
	require.NotContains(t, body, "passkey")
	require.NotContains(t, body, "secret-token")
	require.NotContains(t, body, "passthepopcorn")
}

func TestPostgresSwarmChurnKeepsFrozenJob(t *testing.T) {
	s, p, _ := fixture(t)
	service := newService(t, "swarm-churn", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, service)
	h := seedTorrent(t, p, 99, nil)
	seedSwarm(t, p, h, "udp://tracker.opentrackr.org:1337/announce")
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	lease, err := s.Claim(testContext)
	require.NoError(t, err)
	first, err := s.Prepare(testContext, *lease)
	require.NoError(t, err)

	// A background scrape refresh must not stale the frozen disclosure.
	_, err = p.Exec(testContext, `UPDATE torrent_tracker_seeds SET seeders=99,leechers=9 WHERE info_hash=$1`, h)
	require.NoError(t, err)
	second, err := s.Prepare(testContext, *lease)
	require.NoError(t, err)
	require.Equal(t, first.JobID, second.JobID)
	require.Equal(t, first.InputRevision, second.InputRevision)
	require.Equal(t, 42, *second.Input.Swarm.TrackerSeeders)

	r := result(second, "movie")
	require.NoError(t, s.Finish(testContext, *lease, second, r, false))
	var state string
	require.NoError(t, p.QueryRow(testContext, `SELECT state FROM processor_jobs`).Scan(&state))
	require.Equal(t, "completed", state)
}

func TestPostgresCoreLivenessFallsBackWhenTrackerUnknown(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 100, nil)
	_, err := p.Exec(testContext, `
UPDATE torrent_contents SET seeders=5,leechers=1 WHERE info_hash=$1;
INSERT INTO torrent_tracker_seeds(info_hash,tracker_known,seeders,leechers,completed,best_tracker,checked_at)
 VALUES($1,false,NULL,NULL,NULL,NULL,now());
INSERT INTO torrent_liveness(info_hash,status,last_observed_at) VALUES($1,'suspect',now());`, h)
	require.NoError(t, err)
	resp, err := s.CoreLiveness(testContext, v1.CoreLivenessRequest{InfoHash: hex.EncodeToString(h)})
	require.NoError(t, err)
	require.Equal(t, "suspect", resp.Liveness.Observation)
	require.NotNil(t, resp.Liveness.Seeders)
	require.Equal(t, 5, *resp.Liveness.Seeders)
	require.Equal(t, 0, resp.TrackerCount)
}

func TestPostgresSwarmWithoutPriorsTableStillPrepares(t *testing.T) {
	s, p, _ := fixture(t)
	service := newService(t, "swarm-nopriors", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, service)
	h := seedTorrent(t, p, 101, nil)
	seedSwarm(t, p, h, "udp://tracker.opentrackr.org:1337/announce")
	_, err := p.Exec(testContext, `DROP TABLE grab_outcome_priors`)
	require.NoError(t, err)
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	lease, err := s.Claim(testContext)
	require.NoError(t, err)
	j, err := s.Prepare(testContext, *lease)
	require.NoError(t, err)
	require.NoError(t, j.Validate())
	require.NotNil(t, j.Input.Swarm)
	require.Empty(t, j.Input.Swarm.OutcomePriors)
}
