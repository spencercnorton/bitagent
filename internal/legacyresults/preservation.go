// Package legacyresults contains only frozen source/snapshot validation for
// historical committed results. It has no dispatcher, provider, prompts or config.
package legacyresults

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/model"
	"sort"
	"strings"
	"time"
)

type Kind string

const (
	Type     Kind = "classifier_type"
	Language Kind = "contentfilter"
	Matcher  Kind = "matcher"
)

var ErrHeld = errors.New("historical application requires explicit reconciliation")

func Digest(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(body)
	return append([]byte(nil), sum[:]...)
}

// SourceDigest excludes torrent observation times and swarm counters. Refreshes
// cannot change model inputs, while file contents, hints and native privacy can.
func SourceDigest(t model.Torrent) []byte {
	files := OrderedFiles(t.Files)
	fileEvidence := make([]any, 0, len(files))
	for _, f := range files {
		fileEvidence = append(fileEvidence, []any{f.Index, f.Path, f.Size, f.Extension})
	}
	return Digest([]any{t.InfoHash.Bytes(), t.Name, t.Size, t.Private, t.FilesStatus, t.Extension, t.FilesCount, []any{t.Hint.ContentType, t.Hint.ContentSource, t.Hint.ContentID, t.Hint.Title, t.Hint.ReleaseYear, t.Hint.Languages, t.Hint.Episodes, t.Hint.VideoResolution, t.Hint.VideoSource, t.Hint.VideoCodec, t.Hint.Video3D, t.Hint.VideoModifier, t.Hint.ReleaseGroup}, fileEvidence})
}

func OrderedFiles(files []model.TorrentFile) []model.TorrentFile {
	ordered := append([]model.TorrentFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Size != b.Size {
			return a.Size < b.Size
		}
		if a.Extension.Valid != b.Extension.Valid {
			return !a.Extension.Valid
		}
		return a.Extension.String < b.Extension.String
	})
	return ordered
}

type ApplicationSnapshot struct {
	content            *model.Content
	Schema             string                    `json:"schema"`
	Kind               Kind                      `json:"kind"`
	ContentType        model.NullContentType     `json:"contentType"`
	ContentSource      model.NullString          `json:"contentSource"`
	ContentID          model.NullString          `json:"contentId"`
	Languages          model.Languages           `json:"languages"`
	Episodes           model.Episodes            `json:"episodes"`
	VideoResolution    model.NullVideoResolution `json:"videoResolution"`
	VideoSource        model.NullVideoSource     `json:"videoSource"`
	VideoCodec         model.NullVideoCodec      `json:"videoCodec"`
	Video3D            model.NullVideo3D         `json:"video3d"`
	VideoModifier      model.NullVideoModifier   `json:"videoModifier"`
	ReleaseGroup       model.NullString          `json:"releaseGroup"`
	EnglishAudio       model.NullEnglishAudio    `json:"englishAudio"`
	EnglishAudioSource model.NullString          `json:"englishAudioSource"`
	ReleaseAttributes  *model.ReleaseAttributes  `json:"releaseAttributes,omitempty"`
	ReleaseDate        model.Date                `json:"releaseDate"`
	ContentCreatedAt   time.Time                 `json:"contentCreatedAt"`
	Tags               []string                  `json:"tags"`
}

func NewApplicationSnapshot(kind Kind, tc model.TorrentContent, tags []string) ApplicationSnapshot {
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	return ApplicationSnapshot{content: &tc.Content, Schema: "llm-application-v1", Kind: kind, ContentType: tc.ContentType, ContentSource: tc.ContentSource, ContentID: tc.ContentID,
		Languages: tc.Languages, Episodes: tc.Episodes, VideoResolution: tc.VideoResolution, VideoSource: tc.VideoSource, VideoCodec: tc.VideoCodec, Video3D: tc.Video3D, VideoModifier: tc.VideoModifier, ReleaseGroup: tc.ReleaseGroup, EnglishAudio: tc.EnglishAudio, EnglishAudioSource: tc.EnglishAudioSource, ReleaseAttributes: tc.ReleaseAttributes, ReleaseDate: tc.ReleaseDate, ContentCreatedAt: tc.Content.CreatedAt, Tags: sorted}
}

func CanonicalAuthorityAllows(kind Kind, media, id string, expected model.NullContentType) bool {
	if strings.TrimSpace(id) != "" {
		return false
	}
	var ct model.ContentType
	switch media {
	case "movie":
		ct = model.ContentTypeMovie
	case "tv":
		ct = model.ContentTypeTvShow
	case "music":
		ct = model.ContentTypeMusic
	case "audiobook":
		ct = model.ContentTypeAudiobook
	case "book":
		ct = model.ContentTypeEbook
	}
	return ct == "" || kind == Matcher && expected.Valid && expected.ContentType == ct
}

func HasAuthorityTag(name string) bool { return catalogueguard.ProtectedTag(name, false) }
