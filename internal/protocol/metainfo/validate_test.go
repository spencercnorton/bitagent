package metainfo

import (
	"errors"
	"strings"
	"testing"

	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/assert"
)

// validInfo returns a baseline Info that passes validateInfo. Tests
// mutate one field then reassert.
func validInfo() Info {
	return Info{
		Name:        "Some.Show.S01E01.1080p.WEB-DL.mkv",
		PieceLength: 262144, // 256 KiB — typical for 1080p video
		Files: []mi.FileInfo{
			{Path: []string{"Some.Show.S01E01.1080p.WEB-DL.mkv"}, Length: 1024},
		},
	}
}

func TestValidateInfo_AcceptsValid(t *testing.T) {
	assert.NoError(t, validateInfo(validInfo()))
}

func TestValidateInfo_NameRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Info)
		wantErr error
	}{
		{
			name:    "empty",
			mutate:  func(i *Info) { i.Name = "" },
			wantErr: ErrEmptyName,
		},
		{
			name:    "too long",
			mutate:  func(i *Info) { i.Name = strings.Repeat("x", MaxNameLen+1) },
			wantErr: ErrNameTooLong,
		},
		{
			name:    "invalid UTF-8 (lone continuation byte)",
			mutate:  func(i *Info) { i.Name = "valid\x80suffix" },
			wantErr: ErrInvalidUTF8,
		},
		{
			name:    "NUL byte (truncation attack)",
			mutate:  func(i *Info) { i.Name = "safe\x00../etc/passwd" },
			wantErr: ErrNullByte,
		},
		{
			name:    "control char (ANSI escape)",
			mutate:  func(i *Info) { i.Name = "evil\x1b[31m" },
			wantErr: ErrControlChar,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			info := validInfo()
			tc.mutate(&info)
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "got %v, want sentinel %v", err, tc.wantErr)
		})
	}
}

func TestValidateInfo_PathRejections(t *testing.T) {
	cases := []struct {
		name    string
		path    []string
		wantErr error
	}{
		{name: "empty component", path: []string{"dir", "", "file.mkv"}, wantErr: ErrEmptyPathComponent},
		{name: "dot traversal", path: []string{".", "file.mkv"}, wantErr: ErrPathTraversal},
		{name: "dotdot traversal", path: []string{"..", "etc", "passwd"}, wantErr: ErrPathTraversal},
		{name: "forward slash in component", path: []string{"a/b", "file"}, wantErr: ErrAbsolutePath},
		{name: "backslash in component", path: []string{"a\\b", "file"}, wantErr: ErrAbsolutePath},
		{name: "NUL byte in component", path: []string{"good", "bad\x00.txt"}, wantErr: ErrNullByte},
		{name: "invalid UTF-8 in component", path: []string{"good", "bad\x80name"}, wantErr: ErrInvalidUTF8},
		{name: "control char in component", path: []string{"good", "evil\x07bell"}, wantErr: ErrControlChar},
		{name: "component too long", path: []string{strings.Repeat("x", MaxPathComponentLen+1)}, wantErr: ErrPathTooLong},
		{name: "path too deep", path: deepPath(MaxPathDepth + 1), wantErr: ErrPathTooDeep},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			info := validInfo()
			info.Files = []mi.FileInfo{{Path: tc.path, Length: 1}}
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "got %v, want sentinel %v", err, tc.wantErr)
		})
	}
}

func TestValidateInfo_PieceLengthRejections(t *testing.T) {
	// PieceLength is the most underrated DoS vector — a hostile peer can
	// claim 1 GiB pieces and downstream allocators will pre-size to match.
	cases := []struct {
		name        string
		pieceLength int64
		wantErr     error
	}{
		{name: "zero", pieceLength: 0, wantErr: ErrPieceLenOutOfRange},
		{name: "negative", pieceLength: -1, wantErr: ErrPieceLenOutOfRange},
		{name: "1 GiB attack", pieceLength: 1 << 30, wantErr: ErrPieceLenOutOfRange},
		{name: "just over MaxPieceLen", pieceLength: MaxPieceLen + 1, wantErr: ErrPieceLenOutOfRange},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			info := validInfo()
			info.PieceLength = tc.pieceLength
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr))
		})
	}
}

func TestValidateInfo_PieceLengthAtBoundariesAccepted(t *testing.T) {
	for _, pl := range []int64{MinPieceLen, 16 * 1024, 262144, MaxPieceLen} {
		pl := pl
		t.Run("", func(t *testing.T) {
			info := validInfo()
			info.PieceLength = pl
			assert.NoError(t, validateInfo(info))
		})
	}
}

func TestValidateInfo_RejectsTabAndNewlineInFilename(t *testing.T) {
	// Tightening: legitimate filenames have no use for \t \n \r either —
	// allowing them invites log-injection and CSV-injection downstream.
	for _, bad := range []string{"name\twith\ttab", "name\nwith\nnewline", "name\rwith\rcr"} {
		bad := bad
		t.Run("", func(t *testing.T) {
			info := validInfo()
			info.Name = bad
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, ErrControlChar))
		})
	}
}

func TestValidateInfo_TooManyFiles(t *testing.T) {
	info := validInfo()
	info.Files = make([]mi.FileInfo, MaxFilesCount+1)
	for i := range info.Files {
		info.Files[i] = mi.FileInfo{Path: []string{"f"}, Length: 1}
	}
	err := validateInfo(info)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooManyFiles))
}

func TestValidateInfo_EmptyFilesIsValid(t *testing.T) {
	// Single-file torrents (Length set, Files empty) must validate.
	info := validInfo()
	info.Files = nil
	assert.NoError(t, validateInfo(info))
}

func TestParseMetaInfoBytes_RejectsOversizedInput(t *testing.T) {
	// Pre-unmarshal size cap fires before the hash check is even attempted.
	oversized := make([]byte, MaxMetaInfoBytes+1)
	_, err := ParseMetaInfoBytes([20]byte{}, oversized)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrMetaInfoTooLarge))
}

// deepPath builds a Path slice of length n with each component "d".
func deepPath(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "d"
	}
	return out
}
