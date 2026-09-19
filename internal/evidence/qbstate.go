package evidence

import "strings"

// QBStateClass partitions raw qBittorrent state strings into the three
// classes the liveness pipeline cares about. The mapping is a single
// source of truth shared by the qBittorrent poller (which decides
// whether to emit an evidence row for a given torrent state) and the
// liveness resolver (which decides what to do with the row once it
// arrives).
//
// Rationale per state:
//
//	alive — the torrent has either fully retrieved its metadata and
//	is moving bytes, or is healthily seeded by us. ForcedDL/forcedUP
//	are operator overrides where the user has explicitly told qB to
//	ignore queue limits; treat the same as the non-forced variants.
//	stalledUP is acceptable: we hold the data and qB can serve it,
//	only nobody is currently asking — that says nothing about whether
//	the swarm is alive, but it does prove we have a working copy,
//	which is the property the Torznab consumer cares about.
//
//	suspect — qB has tried to download and is stuck. stalledDL means
//	metadata fetched but no peers willing to send data, metaDL means
//	we cannot even pull the .torrent metainfo, error/missingFiles
//	indicate a local IO problem we treat conservatively, pausedDL is
//	typically an arr abandoning the grab. Each is, on its own, a
//	single transient observation; the resolver requires multiple
//	suspect observations across a configured stall window before
//	flipping to dead.
//
//	ignore — transient internal qB states (queued, checking, moving)
//	that carry no liveness signal. Emitting evidence for these would
//	create churn without information.
type QBStateClass string

const (
	QBStateClassAlive   QBStateClass = "alive"
	QBStateClassSuspect QBStateClass = "suspect"
	QBStateClassIgnore  QBStateClass = "ignore"
)

// ClassifyQBState maps a raw qBittorrent state string to a
// QBStateClass. The match is case-insensitive and trims surrounding
// whitespace; unknown states fall to suspect rather than ignore on
// the principle that an unrecognised state is more often a stuck
// torrent than an in-flight transition.
func ClassifyQBState(state string) QBStateClass {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "seeding", "uploading", "forcedup", "forceddl", "downloading", "stalledup":
		return QBStateClassAlive
	case "stalleddl", "metadl", "error", "missingfiles", "pauseddl", "unknown":
		return QBStateClassSuspect
	case "queueddl", "queuedup", "checkingdl", "checkingup", "checkingresumedata", "moving", "allocating", "":
		return QBStateClassIgnore
	default:
		return QBStateClassSuspect
	}
}
