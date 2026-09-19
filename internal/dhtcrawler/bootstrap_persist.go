package dhtcrawler

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// bootstrapStore persists a small, bounded slice of known-good DHT
// node addresses to disk between container runs. On startup the
// stored addresses are prepended to the compiled-in bootstrap list
// so the crawler can re-join the DHT mesh without an initial
// cold-start RTT against the well-known routers — which is what
// makes the restart-on-IP-change policy cheap enough to use as
// a default recovery path.
//
// Format is one `IP:port` per line, plain text. Chosen over bencode
// / JSON because the file is small, operator-debuggable with `cat`,
// and malformed lines should be skipped rather than fail the whole
// load. Writes are atomic (temp file + rename) so an interrupted
// save never corrupts the existing snapshot.
type bootstrapStore struct {
	path       string
	maxEntries int
}

// newBootstrapStore constructs a store at the given path. maxEntries
// caps the number of addrs persisted/loaded; callers should pick
// something proportional to their bucket-K × buckets target (200 is
// a safe default for mainline-style routing tables).
//
// An empty path is a sentinel for "persistence disabled" — the
// store's methods become no-ops. Callers should check
// `store.enabled()` before invoking Load/Save to avoid false
// "nothing to load" logs.
func newBootstrapStore(path string, maxEntries int) *bootstrapStore {
	if maxEntries <= 0 {
		maxEntries = 200
	}

	return &bootstrapStore{path: path, maxEntries: maxEntries}
}

func (s *bootstrapStore) enabled() bool {
	return s != nil && s.path != ""
}

// Load reads the snapshot file and returns the parsed AddrPort
// entries. Missing-file returns (nil, nil) — a fresh container with
// no prior snapshot is not an error condition. Malformed lines are
// skipped individually (not fatal) so a partial corruption still
// yields a usable seed set; returned error is non-nil only for
// genuine IO failures.
func (s *bootstrapStore) Load() ([]netip.AddrPort, error) {
	if !s.enabled() {
		return nil, nil
	}

	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("bootstrap store: open %q: %w", s.path, err)
	}
	defer f.Close()

	addrs := make([]netip.AddrPort, 0, s.maxEntries)
	scanner := bufio.NewScanner(f)
	// bufio.Scanner default line length is 64k; an address line is
	// at most ~55 bytes. Trim aggressively so we can't blow past the
	// cap via a bad file.
	scanner.Buffer(make([]byte, 64), 256)

	for scanner.Scan() {
		if len(addrs) >= s.maxEntries {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		addr, parseErr := netip.ParseAddrPort(line)
		if parseErr != nil {
			// Skip — better to seed with 199 good addrs than fail the
			// whole load on one garbage line from a prior crash.
			continue
		}
		addrs = append(addrs, addr)
	}

	if err := scanner.Err(); err != nil {
		return addrs, fmt.Errorf("bootstrap store: scan %q: %w", s.path, err)
	}

	return addrs, nil
}

// Save writes the given AddrPort slice to disk atomically (temp file
// + rename). No-op when the store is disabled. The file is capped at
// maxEntries; callers can pass larger slices and this will trim.
//
// Creates the containing directory with 0755 if needed.
func (s *bootstrapStore) Save(addrs []netip.AddrPort) error {
	if !s.enabled() {
		return nil
	}

	if len(addrs) > s.maxEntries {
		addrs = addrs[:s.maxEntries]
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("bootstrap store: mkdir %q: %w", dir, err)
	}

	// Create temp file in the same directory so the final rename is
	// atomic on the same filesystem. Using os.CreateTemp with an
	// empty dir arg would put it in /tmp which can be a different
	// mount.
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("bootstrap store: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	// Ensure cleanup on any early-return error path. Success path
	// (successful rename) makes the remove a no-op, which is fine.
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	w := bufio.NewWriter(tmp)
	for _, a := range addrs {
		if !a.IsValid() {
			continue
		}
		if _, werr := fmt.Fprintln(w, a.String()); werr != nil {
			_ = tmp.Close()
			return fmt.Errorf("bootstrap store: write: %w", werr)
		}
	}

	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bootstrap store: flush: %w", err)
	}

	// fsync so the rename can't make a file that's durably empty on
	// crash-right-after-rename. A bootstrap snapshot is tiny; the
	// sync cost is negligible and the correctness gain is real.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bootstrap store: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("bootstrap store: close: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("bootstrap store: rename %q -> %q: %w", tmpPath, s.path, err)
	}

	return nil
}
