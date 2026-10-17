package processors

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublicTrackerURL(t *testing.T) {
	for _, raw := range []string{
		"udp://open.stealth.si:80/announce",
		"udp://tracker.opentrackr.org:1337/announce",
		"https://tracker.example.org/announce",
	} {
		got, ok := publicTrackerURL(raw)
		require.True(t, ok, raw)
		require.Equal(t, raw, got)
	}
	for _, raw := range []string{
		"",
		"https://tracker.example.org/announce?passkey=secret",
		"https://tracker.example.org/announce/0123456789abcdef0123456789abcdef",
		"https://user:pass@tracker.example.org/announce",
		"https://passthepopcorn.me/announce",
		"udp://tracker.example.org:80/announce?authkey=secret",
		"magnet:?xt=urn:btih:abc",
		"not a url",
	} {
		_, ok := publicTrackerURL(raw)
		require.False(t, ok, raw)
	}
}

func TestPublicAliveSource(t *testing.T) {
	for _, raw := range []string{"qb_state", "arr_webhook_import", "tracker_scrape", "dht_revalidate"} {
		got, ok := publicAliveSource(raw)
		require.True(t, ok, raw)
		require.Equal(t, raw, got)
	}
	for _, raw := range []string{"", "QB_State", "bitgrab", "manual:override", "has space", "passthepopcorn"} {
		_, ok := publicAliveSource(raw)
		require.False(t, ok, raw)
	}
}
