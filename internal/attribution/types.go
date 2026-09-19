package attribution

import "time"

// Source identifies which *arr a grab originated from. Stable string
// for the report column.
type Source string

const (
	SourceSonarr Source = "sonarr"
	SourceRadarr Source = "radarr"
	SourceLidarr Source = "lidarr"
)

// GrabEvent is one row from Prowlarr's grab history. The info_hash
// is the join key for everything else; the indexer + source + *arr-id
// give context.
type GrabEvent struct {
	GrabbedAt   time.Time
	Indexer     string // "BitMagnet (Local DHT)" / "TorrentLeech (Prowlarr)" / etc.
	Source      Source // which *arr selected it
	Title       string
	InfoHash    string // hex, lower-case; "" if Prowlarr didn't expose it (some indexers don't)
	Size        int64  // 0 if missing
	ProwlarrID  int64  // history record ID, for backref
}

// QBState is the snapshot of a qBittorrent torrent for one info_hash.
// Sentinel value when info_hash isn't in qB (i.e. *arr rejected the
// release before sending it to qB).
type QBState struct {
	Found        bool
	Hash         string
	Name         string
	Category     string
	State        string  // "downloading" / "stalledDL" / "stalledUP" / "missingFiles" / etc.
	Progress     float64 // 0..1
	NumSeeds     int
	NumLeechs    int
	Downloaded   int64
	UpSpeed      int64
	DlSpeed      int64
	LastActivity time.Time
	TrackerError string
}

// ArrImport is the import-side outcome from the *arr that grabbed
// this release. The semantics differ slightly per *arr but the
// shape is normalised here.
type ArrImport struct {
	Found      bool
	Source     Source
	EventType  string // "downloadFolderImported" / "downloadFailed" / "downloadIgnored" / etc.
	OccurredAt time.Time
	Reason     string // free-form; populated on failures
}

// Row is the joined view of one info_hash across all sources. Always
// has a GrabEvent (that's how we got here); QB and Arr are
// best-effort and may be sentinel "not found" rows.
type Row struct {
	Grab   GrabEvent
	QB     QBState
	Import ArrImport
}

// Verdict is the human-readable outcome of one Row, computed from
// the joined state. Stable strings for downstream filtering /
// dashboard chips.
//
//	"imported_ok"     — grabbed, downloaded, imported. Healthy.
//	"downloading"     — grabbed, currently in qB downloading. In flight.
//	"completed_pending_import" — qB has finished but *arr hasn't imported yet.
//	"stalled"         — qB has it but it's stalledDL or has tracker errors.
//	"missing_in_qb"   — Prowlarr says grabbed, qB doesn't have the hash.
//	                   Either *arr rejected it before qB, or it was
//	                   already removed.
//	"import_failed"   — *arr explicitly logged a failed import.
//	"unknown"         — chain incomplete, can't tell.
func (r Row) Verdict() string {
	if r.Import.Found && r.Import.EventType == "downloadFolderImported" {
		return "imported_ok"
	}
	if r.Import.Found && (r.Import.EventType == "downloadFailed" || r.Import.EventType == "downloadIgnored") {
		return "import_failed"
	}
	if !r.QB.Found {
		return "missing_in_qb"
	}
	switch r.QB.State {
	case "downloading", "metaDL", "queuedDL", "stoppedDL", "checkingDL", "forcedDL":
		return "downloading"
	case "stalledDL":
		return "stalled"
	case "uploading", "stalledUP", "queuedUP", "pausedUP", "checkingUP", "forcedUP":
		// qB has it complete or seeding; *arr may not have imported yet.
		return "completed_pending_import"
	case "missingFiles", "error":
		return "stalled"
	}
	return "unknown"
}

// Filters is what the CLI passes to Run() to scope the report.
type Filters struct {
	// SinceHours: how far back to read from Prowlarr's grab history.
	// Default 24.
	SinceHours int
	// IndexerSubstring (optional): if non-empty, only include grabs
	// whose Prowlarr indexer name contains this substring (case-
	// insensitive). Default behaviour pre-narrows to bitagent so
	// the report focuses on our chain.
	IndexerSubstring string
	// Limit: cap the number of grab events to process. 0 = no cap.
	Limit int
}

// Summary is the top-of-report counts. Computed by the orchestrator
// after Run() collects rows.
type Summary struct {
	GrabsTotal int
	ByVerdict  map[string]int
	BySource   map[Source]int
	BitAgentN  int // count of grabs whose indexer matches "BitMagnet"
}
