package dhtcrawler

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustAddr(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	a, err := netip.ParseAddrPort(s)
	require.NoError(t, err)

	return a
}

func TestBootstrapStore_roundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dht-bootstrap.peers")
	s := newBootstrapStore(path, 200)

	original := []netip.AddrPort{
		mustAddr(t, "198.51.100.1:6881"),
		mustAddr(t, "198.51.100.2:6881"),
		mustAddr(t, "[2001:db8::1]:6881"),
	}

	require.NoError(t, s.Save(original))

	loaded, err := s.Load()
	require.NoError(t, err)
	assert.Equal(t, original, loaded)
}

func TestBootstrapStore_missingFileReturnsEmpty(t *testing.T) {
	t.Parallel()

	// Fresh tempdir; file never written.
	s := newBootstrapStore(filepath.Join(t.TempDir(), "absent"), 200)

	loaded, err := s.Load()
	require.NoError(t, err,
		"a missing snapshot is expected on a fresh container — must not surface as error")
	assert.Empty(t, loaded)
}

func TestBootstrapStore_emptyPathIsDisabled(t *testing.T) {
	t.Parallel()

	s := newBootstrapStore("", 200)

	assert.False(t, s.enabled())

	// Save / Load on a disabled store are no-ops — caller does not
	// need to guard by `enabled()` for correctness, only to avoid
	// noisy logs.
	require.NoError(t, s.Save([]netip.AddrPort{mustAddr(t, "198.51.100.1:6881")}))
	loaded, err := s.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

func TestBootstrapStore_skipsMalformedLines(t *testing.T) {
	t.Parallel()

	// Hand-written snapshot with a mix of good and bad content.
	// Malformed lines MUST be skipped individually, not fail the
	// whole load — better to seed with the 2 good addrs than lose
	// the warm start over one garbage line from a crash.
	dir := t.TempDir()
	path := filepath.Join(dir, "dht-bootstrap.peers")
	body := "" +
		"# this is a comment and should be ignored\n" +
		"\n" + // blank line
		"198.51.100.1:6881\n" +
		"not-an-address\n" +
		"198.51.100.2:12345\n" +
		"another garbage line without colon\n" +
		"[2001:db8::1]:6881\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

	s := newBootstrapStore(path, 200)
	loaded, err := s.Load()
	require.NoError(t, err)

	assert.Equal(t, []netip.AddrPort{
		mustAddr(t, "198.51.100.1:6881"),
		mustAddr(t, "198.51.100.2:12345"),
		mustAddr(t, "[2001:db8::1]:6881"),
	}, loaded)
}

func TestBootstrapStore_loadCapsAtMaxEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dht-bootstrap.peers")

	// Write 10 entries, cap the load at 3.
	body := ""
	for i := 1; i <= 10; i++ {
		body += "198.51.100.1:68" + padDec2(i) + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

	s := newBootstrapStore(path, 3)
	loaded, err := s.Load()
	require.NoError(t, err)

	assert.Len(t, loaded, 3,
		"load must respect maxEntries — defends against a runaway snapshot growing the heap at boot")
}

func TestBootstrapStore_saveTruncatesOversizedInput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dht-bootstrap.peers")

	// Construct 7 addrs, store caps at 4.
	addrs := make([]netip.AddrPort, 0, 7)
	for i := 1; i <= 7; i++ {
		addrs = append(addrs, mustAddr(t, "198.51.100.1:68"+padDec2(i)))
	}

	s := newBootstrapStore(path, 4)
	require.NoError(t, s.Save(addrs))

	loaded, err := s.Load()
	require.NoError(t, err)
	assert.Len(t, loaded, 4)
	// Must be the FIRST 4, not the last 4 — Save semantics are
	// "drop the tail" so a caller passing a recency-sorted slice
	// retains the N most-preferred.
	assert.Equal(t, addrs[:4], loaded)
}

func TestBootstrapStore_saveCreatesDirTree(t *testing.T) {
	t.Parallel()

	// Path with a directory that doesn't exist yet — Save must
	// create it. Matches the bind-mount + first-boot container case
	// where `/config` exists but `/config/state/` doesn't.
	dir := filepath.Join(t.TempDir(), "state", "dht")
	path := filepath.Join(dir, "dht-bootstrap.peers")

	s := newBootstrapStore(path, 10)
	require.NoError(t, s.Save([]netip.AddrPort{
		mustAddr(t, "198.51.100.1:6881"),
	}))

	_, err := os.Stat(path)
	require.NoError(t, err, "save should have created the file + intermediate dirs")
}

func TestBootstrapStore_saveIsAtomic(t *testing.T) {
	t.Parallel()

	// Pre-populate with a known snapshot. A subsequent Save must
	// either fully replace or leave the prior file intact — never a
	// partial write. Easiest way to check: save twice, confirm the
	// second save's content fully replaced the first.
	dir := t.TempDir()
	path := filepath.Join(dir, "dht-bootstrap.peers")
	s := newBootstrapStore(path, 10)

	first := []netip.AddrPort{
		mustAddr(t, "198.51.100.1:6881"),
		mustAddr(t, "198.51.100.2:6881"),
	}
	second := []netip.AddrPort{
		mustAddr(t, "198.51.100.9:6881"),
	}

	require.NoError(t, s.Save(first))
	require.NoError(t, s.Save(second))

	loaded, err := s.Load()
	require.NoError(t, err)
	assert.Equal(t, second, loaded,
		"second Save must fully replace the first — no residual lines from the prior file")

	// Verify no temp files lingered (they'd appear as
	// dht-bootstrap.peers.tmp-*).
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, filepath.Ext(e.Name()) == ".tmp" || filepath.Base(e.Name()) == "tmp",
			"no orphaned temp files should remain after a successful Save: found %q", e.Name())
	}
}

// padDec2 formats i as exactly 2 digits with leading zero. Inlined
// rather than pulling in fmt-formatted helpers to keep the tests
// allocation-free.
func padDec2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
