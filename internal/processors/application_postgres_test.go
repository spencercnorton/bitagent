package processors

import (
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMatchApplicationRequiresUniqueCurrentIdentityAndPreservesSearch(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		name := "unique"
		if ambiguous {
			name = "identity_added_during_inference"
		}
		t.Run(name, func(t *testing.T) {
			s, p, _ := fixture(t)
			h := seedTorrent(t, p, 51, "movie")
			_, e := p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES('movie','tmdb','991051','Synthetic Amber',2024,to_tsvector('simple','Synthetic Amber 2024 preservedalias'),now(),now())`)
			require.NoError(t, e)
			a := newService(t, "matcher", []v1.Stage{v1.Match}, func(j v1.Job) v1.Result {
				return v1.Result{Binding: j.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{Match: &v1.MatchRef{Source: "tmdb", ID: "991051"}}}
			})
			register(t, s, a)
			_, e = s.Scan(testContext)
			require.NoError(t, e)
			lease, e := s.Claim(testContext)
			require.NoError(t, e)
			require.NotNil(t, lease)
			job, e := s.Prepare(testContext, *lease)
			require.NoError(t, e)
			require.Len(t, job.Input.Candidates, 1)
			if ambiguous {
				_, e = p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES('movie','tmdb','991052','Synthetic Amber',2024,to_tsvector('simple','Synthetic Amber 2024'),now(),now())`)
				require.NoError(t, e)
			}
			r := v1.Result{Binding: job.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{Match: &v1.MatchRef{Source: "tmdb", ID: "991051"}}}
			require.NoError(t, s.Finish(testContext, *lease, job, r, false))
			var id *string
			var preserved bool
			require.NoError(t, p.QueryRow(testContext, `SELECT content_id,COALESCE(tsv @@ plainto_tsquery('simple','preservedalias'),false) FROM torrent_contents WHERE info_hash=$1`, h).Scan(&id, &preserved))
			if ambiguous {
				require.Nil(t, id)
			} else {
				require.NotNil(t, id)
				require.Equal(t, "991051", *id)
				require.True(t, preserved)
			}
		})
	}
}

func TestTypeProposalHonorsExistingFileAndLanguagePolicy(t *testing.T) {
	for _, music := range []bool{false, true} {
		name := "language"
		kind := "movie"
		if music {
			name = "lossy_files"
			kind = "music"
		}
		t.Run(name, func(t *testing.T) {
			s, p, _ := fixture(t)
			h := seedTorrent(t, p, 52, nil)
			s.filter = contentfilter.New(contentfilter.Config{Enabled: true, Enforce: true, RequireEnglishLanguage: true, AllowedLanguages: []string{"en"}, DropLossyAudioOnly: true})
			var e error
			if music {
				_, e = p.Exec(testContext, `UPDATE torrents SET name='Synthetic.Album.mp3' WHERE info_hash=$1`, h)
			} else {
				_, e = p.Exec(testContext, `UPDATE torrent_contents SET languages='["fr"]' WHERE info_hash=$1`, h)
			}
			require.NoError(t, e)
			a := newService(t, "type", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, kind) })
			register(t, s, a)
			_, e = s.Scan(testContext)
			require.NoError(t, e)
			run(t, s)
			var got *string
			require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&got))
			require.Nil(t, got)
			var count int
			require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrents WHERE info_hash=$1`, h).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}
