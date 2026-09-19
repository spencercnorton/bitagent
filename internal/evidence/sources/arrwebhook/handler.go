// Package arrwebhook exposes the /evidence/arr/:instance endpoint
// that receives Sonarr/Radarr/Readarr/Lidarr webhook events
// (Settings → Connect → Webhook). It translates the webhook payload
// into evidence.Evidence records and hands them to the store.
package arrwebhook

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	// PathPrefix is where the webhook is mounted.
	PathPrefix = "/evidence/arr"

	// HeaderAuth is the header *arr sends via Custom Headers containing
	// the shared secret. Using a custom header keeps it out of URLs
	// and proxy logs.
	HeaderAuth = "X-Evidence-Token"

	// maxBodyBytes caps how much we read from an *arr webhook. *arr
	// payloads are tiny (KB) in practice; 256 KB is far more than we
	// will ever receive, but firmly bounds malicious requests.
	maxBodyBytes = 256 << 10
)

// Params are the fx dependencies for the webhook module.
type Params struct {
	fx.In
	Store   *evidence.Store
	Metrics *evidence.Metrics
	Config  evidence.Config
	Logger  *zap.SugaredLogger
}

// Result exposes the handler as an http_server_options group member so
// it is mounted automatically when the HTTP server starts.
type Result struct {
	fx.Out
	Option httpserver.Option `group:"http_server_options"`
}

// New returns the fx Result. Registering this in an fx module is
// sufficient to expose the webhook route.
func New(p Params) Result {
	return Result{Option: &handler{
		store:   p.Store,
		metrics: p.Metrics,
		config:  p.Config,
		logger:  p.Logger.Named("arrwebhook"),
	}}
}

type handler struct {
	store   *evidence.Store
	metrics *evidence.Metrics
	config  evidence.Config
	logger  *zap.SugaredLogger
}

func (*handler) Key() string { return "evidence_arrwebhook" }

func (h *handler) Apply(e *gin.Engine) error {
	e.POST(PathPrefix+"/:instance", h.handle)
	return nil
}

// handle processes a single webhook delivery. It is intentionally
// forgiving about payload shape: *arr emits many eventType values and
// the shape of data varies across service and version. We extract what
// we can and drop what we cannot; any single malformed field should not
// abort the whole request.
func (h *handler) handle(c *gin.Context) {
	instance := c.Param("instance")

	// AuthN
	if h.config.WebhookSecret != "" {
		given := c.GetHeader(HeaderAuth)
		if subtle.ConstantTimeCompare([]byte(given), []byte(h.config.WebhookSecret)) != 1 {
			h.metrics.Rejected("unknown_arr", "webhook", "auth")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "auth"})
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes))
	if err != nil {
		h.metrics.Rejected("unknown_arr", "webhook", "malformed")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}

	var payload arrPayload
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&payload); err != nil {
		h.metrics.Rejected("unknown_arr", "webhook", "malformed")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "decode"})
		return
	}

	src, kind, mediaType, strength, ok := classifyEvent(deriveApp(instance, payload), payload.EventType)
	h.metrics.Received(src, kind)
	if !ok {
		// Unknown event types are common (Health, Test, ApplicationUpdate,
		// etc.). We acknowledge them and move on.
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "event": payload.EventType})
		return
	}

	ev := evidence.Evidence{
		Source:         src,
		Kind:           kind,
		SourceInstance: instance,
		SourceObjectID: payload.eventID(),
		DownloadID:     strings.ToLower(payload.DownloadID),
		InfoHash:       decodeHexOrNil(payload.DownloadID),
		Title:          payload.bestTitle(),
		MediaType:      mediaType,
		MediaID:        payload.mediaID(),
		ObservedAt:     time.Now().UTC(),
		Strength:       strength,
		RawPayload:     json.RawMessage(body),
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.store.Insert(ctx, ev); err != nil {
		switch {
		case errors.Is(err, evidence.ErrDuplicate):
			h.metrics.Duplicated(src, kind)
			c.JSON(http.StatusOK, gin.H{"status": "duplicate"})
			return
		case errors.Is(err, evidence.ErrNotUsable):
			h.metrics.Rejected(src, kind, "no_join_key")
			c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "no_join_key"})
			return
		default:
			h.metrics.SourceError(src, instance, "store")
			h.logger.Errorw("arrwebhook store error", "source", src, "kind", kind, "err", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "store"})
			return
		}
	}
	h.metrics.Persisted(src, kind)
	if len(ev.InfoHash) > 0 {
		h.metrics.CanonicalUpsert(src, mediaType)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// arrPayload is the permissive subset of the Sonarr/Radarr webhook
// payload we care about. *arr fields shift across versions; we only
// bind fields we know about and JSON-unmarshal the rest into RawPayload
// at the caller. Every field is optional.
type arrPayload struct {
	ApplicationName string `json:"applicationName"`
	EventType       string `json:"eventType"`
	InstanceName    string `json:"instanceName"`
	DownloadID      string `json:"downloadId"`
	DownloadClient  string `json:"downloadClient"`
	Release         struct {
		ReleaseTitle string `json:"releaseTitle"`
		Indexer      string `json:"indexer"`
		Size         int64  `json:"size"`
	} `json:"release"`
	Movie struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		TmdbID int    `json:"tmdbId"`
	} `json:"movie"`
	Series struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		TvdbID int    `json:"tvdbId"`
	} `json:"series"`
	Episodes []struct {
		EpisodeID int `json:"id"`
	} `json:"episodes"`
	MovieFile struct {
		Path      string `json:"path"`
		SceneName string `json:"sceneName"`
	} `json:"movieFile"`
}

// eventID composes a stable identifier that survives retries. *arr
// webhook retries carry the same payload so we join on the full event
// plus the download identifier.
func (p arrPayload) eventID() string {
	var parts []string
	parts = append(parts, p.EventType)
	if p.DownloadID != "" {
		parts = append(parts, p.DownloadID)
	} else if p.Release.ReleaseTitle != "" {
		parts = append(parts, "release:"+p.Release.ReleaseTitle)
	}
	if p.Movie.ID != 0 {
		parts = append(parts, "movie")
	}
	if p.Series.ID != 0 {
		parts = append(parts, "series")
	}
	for _, ep := range p.Episodes {
		parts = append(parts, "ep"+itoaCompact(ep.EpisodeID))
	}
	return strings.Join(parts, "|")
}

func (p arrPayload) bestTitle() string {
	switch {
	case p.Release.ReleaseTitle != "":
		return p.Release.ReleaseTitle
	case p.Movie.Title != "":
		return p.Movie.Title
	case p.Series.Title != "":
		return p.Series.Title
	}
	return ""
}

func (p arrPayload) mediaID() string {
	switch {
	case p.Movie.TmdbID != 0:
		return "tmdb:" + itoaCompact(p.Movie.TmdbID)
	case p.Series.TvdbID != 0:
		return "tvdb:" + itoaCompact(p.Series.TvdbID)
	case p.Movie.ID != 0:
		return "radarr:" + itoaCompact(p.Movie.ID)
	case p.Series.ID != 0:
		return "sonarr:" + itoaCompact(p.Series.ID)
	}
	return ""
}

// decodeHexOrNil returns the decoded bytes of a hex string only if
// the input looks like an infohash (40 hex chars for SHA1). qB
// DownloadID is the lowercased infohash for torrents, so *arr inherits
// that. A non-qB download client (e.g. NZBGet) yields a non-hex
// DownloadID which we correctly decline.
func decodeHexOrNil(s string) []byte {
	if len(s) != 40 {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// deriveApp resolves the *arr application identifier used to route a
// webhook delivery into classifyEvent. Sources are consulted in order:
//
//  1. instance — the URL path :instance, set by the operator when
//     configuring Sonarr/Radarr Connect → Webhook (e.g. sonarr in the
//     URL .../evidence/arr/sonarr). Authoritative — operator decided
//     this when wiring the notification.
//  2. payload.InstanceName — *arr's own instance label. Defaults to
//     the app name on every supported version but operators may
//     customise it (e.g. "Sonarr-4K"), so it is not always a clean
//     "sonarr"/"radarr" string.
//  3. payload.ApplicationName — present in some older *arr versions.
//     Sonarr v4 and Radarr v6 webhook payloads omit this field
//     entirely, which previously caused every delivery to be dropped
//     with empty Source/Kind labels.
//
// Returns the first non-empty source, trimmed of surrounding
// whitespace, or "" when all three are empty.
func deriveApp(instance string, payload arrPayload) string {
	for _, v := range []string{instance, payload.InstanceName, payload.ApplicationName} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// classifyEvent maps a (source app, event type) pair to the evidence
// Source/Kind/Strength we persist. Unknown events return ok=false so
// they are acknowledged and dropped without a DB write.
func classifyEvent(app, event string) (evidence.Source, evidence.Kind, evidence.MediaType, uint8, bool) {
	var src evidence.Source
	var mt evidence.MediaType
	switch strings.ToLower(app) {
	case "sonarr":
		src = evidence.SourceSonarr
		mt = evidence.MediaTypeTV
	case "radarr":
		src = evidence.SourceRadarr
		mt = evidence.MediaTypeMovie
	case "readarr":
		src = evidence.SourceReadarr
		mt = evidence.MediaTypeBook
	case "lidarr":
		src = evidence.SourceLidarr
		mt = evidence.MediaTypeMusic
	default:
		return "", "", "", 0, false
	}
	switch event {
	case "Grab":
		return src, evidence.KindWebhookGrab, mt, evidence.StrengthArrWebhookGrab, true
	case "Download", "DownloadFolderImported":
		return src, evidence.KindWebhookImport, mt, evidence.StrengthArrWebhookImport, true
	}
	return "", "", "", 0, false
}

// itoaCompact avoids pulling in strconv-wide deps for a single call site.
func itoaCompact(n int) string {
	if n == 0 {
		return "0"
	}
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return sign + string(buf[pos:])
}
