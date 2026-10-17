package v1

const CoreOpsContract = "bitagent.processors.core-ops.v1"

// CoreParseRequest invokes BitAgent Core's deterministic title, episode, anime,
// language, and release-attribute parser on a torrent name and optional file list.
type CoreParseRequest struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Files       []File `json:"files,omitempty"`
}

type CoreParseResponse struct {
	Contract    string         `json:"contract"`
	Extraction  Extraction     `json:"extraction"`
	Language    *LanguageClaim `json:"language,omitempty"`
	ContentType string         `json:"content_type,omitempty"`
}

// CoreSearchRequest queries BitAgent Core's local metadata catalogue (content)
// for candidate matches by title, year, content_type, or external source/ID.
type CoreSearchRequest struct {
	ContentType string `json:"content_type"`
	Title       string `json:"title,omitempty"`
	Year        int    `json:"year,omitempty"`
	Source      string `json:"source,omitempty"`
	ExternalID  string `json:"external_id,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type CoreSearchResponse struct {
	Contract   string      `json:"contract"`
	Candidates []Candidate `json:"candidates"`
}

// CoreLivenessRequest inspects BitAgent Core's swarm liveness, tracker seeds,
// and evidence state for a torrent info_hash.
type CoreLivenessRequest struct {
	InfoHash string `json:"info_hash"`
}

type CoreLivenessResponse struct {
	Contract     string        `json:"contract"`
	InfoHash     string        `json:"info_hash"`
	Liveness     LivenessClaim `json:"liveness"`
	TrackerCount int           `json:"tracker_count"`
}

// CorePolicyRequest evaluates BitAgent Core's name policy and content filter
// against a candidate torrent name, extensions, languages, and content_type.
type CorePolicyRequest struct {
	InfoHash         string   `json:"info_hash,omitempty"`
	Name             string   `json:"name"`
	ContentType      string   `json:"content_type,omitempty"`
	PrimaryExtension string   `json:"primary_extension,omitempty"`
	AllExtensions    []string `json:"all_extensions,omitempty"`
	Languages        []string `json:"languages,omitempty"`
}

type CorePolicyResponse struct {
	Contract         string   `json:"contract"`
	Allowed          bool     `json:"allowed"`
	NameEligible     bool     `json:"name_eligible"`
	FilterAllowed    bool     `json:"filter_allowed"`
	ExtensionAllowed bool     `json:"extension_allowed"`
	Reasons          []string `json:"reasons"`
}
