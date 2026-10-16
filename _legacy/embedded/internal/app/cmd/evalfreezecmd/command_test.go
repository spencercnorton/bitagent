package evalfreezecmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))

	return p
}

func TestParseHashesFromJSONSampleObject(t *testing.T) {
	t.Parallel()

	p := writeTemp(t, "sample.json",
		`{"sample": [{"infoHash": "acb9f606d1538b2bfac3d0d6ca0763c62f6f827e", "name": "x"},`+
			`{"infoHash": "ACB9F606D1538B2BFAC3D0D6CA0763C62F6F827E"},`+
			`{"infoHash": "08c5fade62026e9e91e7e9e00000000000000000"}]}`)

	hashes, err := parseHashesFromFile(p)
	require.NoError(t, err)
	require.Equal(t, []string{
		"acb9f606d1538b2bfac3d0d6ca0763c62f6f827e",
		"08c5fade62026e9e91e7e9e00000000000000000",
	}, hashes)
}

func TestParseHashesFromJSONArrayOfStrings(t *testing.T) {
	t.Parallel()

	p := writeTemp(t, "arr.json", `["acb9f606d1538b2bfac3d0d6ca0763c62f6f827e", "not-a-hash"]`)

	hashes, err := parseHashesFromFile(p)
	require.NoError(t, err)
	require.Equal(t, []string{"acb9f606d1538b2bfac3d0d6ca0763c62f6f827e"}, hashes)
}

func TestParseHashesFromTSVFirstField(t *testing.T) {
	t.Parallel()

	p := writeTemp(t, "rows.tsv",
		"acb9f606d1538b2bfac3d0d6ca0763c62f6f827e\tmovie\tSome Name\n"+
			"08c5fade62026e9e91e7e9e00000000000000000|tv_show|Other\n"+
			"garbage line\n")

	hashes, err := parseHashesFromFile(p)
	require.NoError(t, err)
	require.Equal(t, []string{
		"acb9f606d1538b2bfac3d0d6ca0763c62f6f827e",
		"08c5fade62026e9e91e7e9e00000000000000000",
	}, hashes)
}

func TestResolveExpected(t *testing.T) {
	t.Parallel()

	tv := int64(259909)
	m := map[string]tvdbMapping{"424242": {TmdbTV: &tv}}

	exp := resolveExpected("tv", "tvdb:424242", m)
	require.Equal(t, "tv_show", exp["tmdb_type"])
	require.Equal(t, "259909", exp["tmdb_id"])

	exp = resolveExpected("movie", "tmdb:603", nil)
	require.Equal(t, "movie", exp["tmdb_type"])
	require.Equal(t, "603", exp["tmdb_id"])

	exp = resolveExpected("tv", "sonarr:12", nil)
	_, hasID := exp["tmdb_id"]
	require.False(t, hasID)
	require.Equal(t, "sonarr:12", exp["media_id"])

	exp = resolveExpected("tv", "tvdb:999", map[string]tvdbMapping{})
	_, hasID = exp["tmdb_id"]
	require.False(t, hasID)
}
