package metainfo

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Hardening bounds. These are defence-in-depth — the BEP-9 receive layer
// already caps incoming bytes at 10 MiB (see metainforequester.maxMetadataSize),
// but ParseMetaInfoBytes must not trust callers to have done that, since it's
// also used by ReadTorrentFileBytes for operator-supplied .torrent files.
const (
	// MaxMetaInfoBytes caps the bencoded `info` dict size we accept. 10 MiB
	// matches the BEP-9 receive cap; values above this are either malformed
	// or hostile.
	MaxMetaInfoBytes = 10 * 1024 * 1024

	// MaxNameLen caps Info.Name. 256 covers every legitimate scene release
	// name we've seen in the corpus and also matches POSIX NAME_MAX.
	MaxNameLen = 256

	// MaxPathComponentLen caps each component of a Files[i].Path. 255 matches
	// POSIX NAME_MAX and is the ceiling Postgres TEXT will accept without
	// padding to the next page.
	MaxPathComponentLen = 255

	// MaxPathDepth caps how deep Files[i].Path may nest. Real torrents nest
	// 2-5 levels; 64 is a generous cap that still bounds DoS surface.
	MaxPathDepth = 64

	// MaxFilesCount caps how many Files entries we accept. The largest
	// real-world torrents (Linux distro mirrors, BluRay sets) sit in the
	// low thousands. 100k is the working ceiling.
	MaxFilesCount = 100_000

	// MaxPieceLen caps PieceLength. Real torrents use power-of-two sizes
	// from 16 KiB up to 16 MiB; values above that are spec-violating and
	// can be used as a downstream allocator-DoS vector (peers claim huge
	// piece sizes so naive engines pre-allocate per-piece buffers).
	MaxPieceLen = 16 * 1024 * 1024

	// MinPieceLen — anacrolix/torrent treats <= 0 as malformed. Pin a floor
	// so we reject zero/negative explicitly with our sentinel error.
	MinPieceLen = 1
)

// Validation errors are sentinel-comparable via errors.Is so callers
// (banning logic, peer-reputation scoring) can act on them without
// string-matching.
var (
	ErrMetaInfoTooLarge   = errors.New("metainfo bytes exceed MaxMetaInfoBytes")
	ErrInvalidUTF8        = errors.New("string field contains invalid UTF-8")
	ErrNullByte           = errors.New("string field contains a NUL byte")
	ErrControlChar        = errors.New("string field contains a control character outside \\t\\n\\r")
	ErrEmptyName          = errors.New("Info.Name is empty")
	ErrNameTooLong        = errors.New("Info.Name exceeds MaxNameLen")
	ErrEmptyPathComponent = errors.New("Files[i].Path contains an empty component")
	ErrPathTraversal      = errors.New("Files[i].Path contains \".\" or \"..\"")
	ErrAbsolutePath       = errors.New("Files[i].Path component contains a path separator")
	ErrPathTooLong        = errors.New("Files[i].Path component exceeds MaxPathComponentLen")
	ErrPathTooDeep        = errors.New("Files[i].Path exceeds MaxPathDepth")
	ErrTooManyFiles       = errors.New("Files count exceeds MaxFilesCount")
	ErrPieceLenOutOfRange = errors.New("PieceLength outside [MinPieceLen, MaxPieceLen]")
)

// validateInfo runs the post-unmarshal hardening checks on a parsed Info
// value. Returns the first violation as a wrapped sentinel error; callers
// can pull the sentinel via errors.Is. Order matters only for clearer error
// messages — every check is mandatory.
func validateInfo(info Info) error {
	if err := validateName(info.Name); err != nil {
		return fmt.Errorf("Info.Name: %w", err)
	}
	if info.PieceLength < MinPieceLen || info.PieceLength > MaxPieceLen {
		// Hostile peers can claim PieceLength=1<<30 to force huge per-piece
		// allocations downstream. Real torrents always sit in [16 KiB, 16 MiB].
		return fmt.Errorf("Info.PieceLength (%d): %w", info.PieceLength, ErrPieceLenOutOfRange)
	}
	if len(info.Files) > MaxFilesCount {
		return fmt.Errorf("Info.Files (%d): %w", len(info.Files), ErrTooManyFiles)
	}
	for i, f := range info.Files {
		if err := validatePath(f.Path); err != nil {
			return fmt.Errorf("Info.Files[%d].Path: %w", i, err)
		}
	}
	return nil
}

// validateName checks Info.Name. Must be non-empty, length-bounded, valid
// UTF-8, and free of NUL / non-tab/newline/CR control characters.
func validateName(name string) error {
	if name == "" {
		return ErrEmptyName
	}
	if len(name) > MaxNameLen {
		return ErrNameTooLong
	}
	if !utf8.ValidString(name) {
		return ErrInvalidUTF8
	}
	for _, r := range name {
		if r == 0 {
			return ErrNullByte
		}
		if isBadControl(r) {
			return ErrControlChar
		}
	}
	return nil
}

// validatePath checks one Files[i].Path slice. Each component must be
// non-empty, not "." or "..", not contain a path separator (the slice
// IS the separator), valid UTF-8, NUL-free, control-char-free, and
// length-bounded. The slice itself is depth-bounded.
func validatePath(parts []string) error {
	if len(parts) > MaxPathDepth {
		return ErrPathTooDeep
	}
	for _, p := range parts {
		if p == "" {
			return ErrEmptyPathComponent
		}
		if p == "." || p == ".." {
			return ErrPathTraversal
		}
		if len(p) > MaxPathComponentLen {
			return ErrPathTooLong
		}
		if !utf8.ValidString(p) {
			return ErrInvalidUTF8
		}
		for _, r := range p {
			if r == 0 {
				return ErrNullByte
			}
			if isBadControl(r) {
				return ErrControlChar
			}
			if r == '/' || r == '\\' {
				return ErrAbsolutePath
			}
		}
	}
	return nil
}

// isBadControl returns true for control runes that should never appear in
// a torrent Name or Files[i].Path component. Filenames legitimately have
// no use for ANY control character — including \t \n \r — and allowing
// them invites log-injection / shell-pipeline / CSV-injection attacks
// when downstream consumers print or split on whitespace. Reject all of
// C0 (< 0x20) and DEL (0x7f).
func isBadControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}
