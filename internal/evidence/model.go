// Package evidence ingests authoritative category/media signals from
// qBittorrent instances and *arr services (Sonarr, Radarr, Readarr,
// Lidarr), persists them to label_evidence as an append-only audit
// trail, and derives a per-infohash winning label in
// torrent_canonical_labels via explicit precedence rules.
//
// Canonical labels are consumed before the classifier runs; see
// ARCHITECTURE.md §1 for the pre-emption contract. This package does
// not call the classifier and does not know about it; responsibility
// for pre-emption lives on the classifier side.
package evidence

import (
	"encoding/json"
	"time"
)

// Source is the external system that produced the evidence.
type Source string

const (
	SourceQBittorrent Source = "qbittorrent"
	SourceSonarr      Source = "sonarr"
	SourceRadarr      Source = "radarr"
	SourceReadarr     Source = "readarr"
	SourceLidarr      Source = "lidarr"
)

// Kind distinguishes the kind of observation within a source, because
// precedence depends on it (an *arr import webhook is stronger than an
// *arr grab webhook, which is stronger than a qB category tag).
type Kind string

const (
	KindWebhookImport  Kind = "webhook_import"
	KindWebhookGrab    Kind = "webhook_grab"
	KindPollHistory    Kind = "poll_history"
	KindPollCategories Kind = "poll_categories"
	// KindQBStateObservation carries a qBittorrent torrent state
	// (downloading, seeding, stalledDL, …). Distinct from
	// KindPollCategories so the liveness resolver and the canonical
	// resolver can dispatch independently — categories drive media
	// labels, states drive liveness.
	KindQBStateObservation Kind = "qb_state_observation"
)

// MediaType is the kind of content represented by the torrent, using
// a stable vocabulary the canonical-label consumers can match against.
type MediaType string

const (
	MediaTypeMovie     MediaType = "movie"
	MediaTypeTV        MediaType = "tv"
	MediaTypeMusic     MediaType = "music"
	MediaTypeBook      MediaType = "book"
	MediaTypeAudiobook MediaType = "audiobook"
	MediaTypeUnknown   MediaType = "unknown"
)

// Precedence weights. These are the ordering rule from ARCHITECTURE.md
// §1 expressed numerically. Higher wins. Ties broken by recency.
//
// The specific values are not load-bearing; only the ordering is. If a
// stronger source is added later it should be given a higher constant
// and these constants renumbered together — never reuse a previously
// published value, as that would silently downgrade existing rows.
const (
	StrengthQBCategoryPublic  uint8 = 30
	StrengthQBCategoryPrivate uint8 = 40
	StrengthArrPollGrab       uint8 = 60
	StrengthArrWebhookGrab    uint8 = 60
	StrengthArrPollImport     uint8 = 80
	StrengthArrWebhookImport  uint8 = 100

	// Liveness-class strengths. These are NOT used for the
	// canonical-label projection — that table is media-typed and
	// liveness rows carry no media_type. They live in the same
	// numeric space only so all evidence rows share a single
	// strength column at the storage layer; the liveness resolver
	// reads its own table and ignores label_evidence.strength when
	// dispatching.
	StrengthQBStateAlive   uint8 = 50
	StrengthQBStateSuspect uint8 = 35
)

// Evidence is a single observation record. It is written once and
// never mutated. All fields except InfoHash, DownloadID, Title,
// MediaType, MediaID, Category, and RawPayload are required.
//
// At most one of InfoHash or DownloadID can be absent; when both are
// absent the evidence is useless and should be rejected at ingest.
type Evidence struct {
	ID             int64
	Source         Source
	Kind           Kind
	SourceInstance string
	SourceObjectID string
	DownloadID     string
	InfoHash       []byte
	Title          string
	MediaType      MediaType
	MediaID        string
	Category       string
	ObservedAt     time.Time
	Strength       uint8
	RawPayload     json.RawMessage
	// QBState is populated only on KindQBStateObservation rows. It is
	// the raw qBittorrent state string (e.g. "downloading", "stalledDL")
	// kept verbatim so the liveness resolver and any future consumer
	// can apply their own classification rule. Not persisted as a
	// dedicated column — it lives inside RawPayload and is mirrored
	// here as a convenience for in-memory dispatch.
	QBState string
}

// CanonicalLabel is the current winning label for an infohash.
// It is derived from the strongest applicable evidence row.
type CanonicalLabel struct {
	InfoHash         []byte
	MediaType        MediaType
	MediaID          string
	Category         string
	Title            string
	ResolvedFrom     int64
	ResolvedSource   Source
	ResolvedStrength uint8
	ResolvedAt       time.Time
}
