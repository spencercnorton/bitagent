package httpserver

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

type handler struct {
	config  torznab.Config
	client  torznab.Client
	metrics *Metrics // nil-safe — unwired tests pass nil
}

func (h handler) handleRequest(ctx *gin.Context) {
	// Private-tracker auth boundary. When TORZNAB_API_KEY (legacy single
	// key) or TORZNAB_API_KEYS (multi named keys) is set, every request
	// must carry a matching key. When unset, this is a no-op and the
	// handler continues in open mode for backward compatibility with
	// the pre-2026-04-26 behaviour. See auth.go.
	if !h.requireAPIKey(ctx) {
		// Auth path already wrote 401 + Torznab error envelope. We
		// observe the rejection here so the auth path stays free of
		// metric coupling.
		h.metrics.observeAuth("", "rejected")
		return
	}
	keyName := resolvedAPIKeyName(ctx)
	if keyName == "" {
		// Open mode — no key configured. Track these separately so
		// the operator can see private vs open traffic split at a
		// glance and notice if they accidentally redeployed without
		// keys set.
		keyName = "open"
	}
	h.metrics.observeAuth(keyName, "ok")

	profile, err := h.getProfile(ctx)
	if err != nil {
		h.writeError(ctx, err)
		return
	}

	tp := ctx.Query(torznab.ParamType)

	switch tp {
	case "":
		h.writeError(ctx, torznab.Error{
			Code:        200,
			Description: fmt.Sprintf("missing parameter (%s)", torznab.ParamType),
		})

	case torznab.FunctionCaps:
		h.metrics.observeCaps(profile.ID)
		h.writeXML(ctx, profile.Caps())

	default:
		h.handleSearch(ctx, profile, tp)
	}
}

func (h handler) handleSearch(ctx *gin.Context, profile torznab.Profile, tp string) {
	var cats []int

	for _, csvCat := range ctx.QueryArray(torznab.ParamCat) {
		for _, cat := range strings.Split(csvCat, ",") {
			if intCat, err := strconv.Atoi(cat); err == nil {
				cats = append(cats, intCat)
			}
		}
	}

	imdbID := model.NullString{}
	if qIMDBID := ctx.Query(torznab.ParamIMDBID); qIMDBID != "" {
		imdbID.Valid = true
		imdbID.String = qIMDBID
	}

	tmdbID := model.NullString{}
	if qTMDBID := ctx.Query(torznab.ParamTMDBID); qTMDBID != "" {
		tmdbID.Valid = true
		tmdbID.String = qTMDBID
	}

	season := model.NullInt{}
	episode := model.NullInt{}
	airDate := model.Date{}

	if qSeason := ctx.Query(torznab.ParamSeason); qSeason != "" {
		if intSeason, err := strconv.Atoi(qSeason); err == nil {
			season.Valid = true
			season.Int = intSeason

			if qEpisode := ctx.Query(torznab.ParamEpisode); qEpisode != "" {
				if intEpisode, err := strconv.Atoi(qEpisode); err == nil {
					episode.Valid = true
					episode.Int = intEpisode
				} else if d, ok := parseDailyEpisode(intSeason, qEpisode); ok {
					// daily-show form: season is the year, ep is MM/DD —
					// previously discarded by the Atoi above, silently
					// degrading every daily query to a season browse.
					airDate = d
				}
			}
		}
	}

	limit := model.NullUint{}
	if intLimit, limitErr := strconv.Atoi(ctx.Query(torznab.ParamLimit)); limitErr == nil && intLimit > 0 {
		limit.Valid = true
		limit.Uint = uint(intLimit)
	}

	offset := model.NullUint{}
	if intOffset, offsetErr := strconv.Atoi(ctx.Query(torznab.ParamOffset)); offsetErr == nil {
		offset.Valid = true
		offset.Uint = uint(intOffset)
	}

	t0 := time.Now()
	result, searchErr := h.client.Search(ctx, torznab.SearchRequest{
		Profile: profile,
		Query:   ctx.Query(torznab.ParamQuery),
		Type:    tp,
		Cats:    cats,
		IMDBID:  imdbID,
		TMDBID:  tmdbID,
		Season:  season,
		Episode: episode,
		AirDate: airDate,
		Limit:   limit,
		Offset:  offset,
	})

	// Record metrics regardless of error path. resultCount is 0 on
	// error. Pulled out so a future error-detail histogram can hang
	// off the same observation point.
	h.metrics.observeSearch(
		profile.ID,
		tp,
		cats,
		resultCountIfOK(searchErr, len(result.Channel.Items)),
		time.Since(t0),
		statusForErr(searchErr),
	)

	if searchErr != nil {
		h.writeError(ctx, fmt.Errorf("failed to search: %w", searchErr))
		return
	}

	h.writeXML(ctx, result)
}

func (h handler) writeXML(ctx *gin.Context, obj torznab.XMLer) {
	body, err := obj.XML()
	if err != nil {
		h.writeHTTPError(ctx, fmt.Errorf("failed to encode xml: %w", err))
		return
	}

	ctx.Status(http.StatusOK)
	ctx.Header("Content-Type", "application/xml; charset=utf-8")
	_, _ = ctx.Writer.Write(body)
}

func (h handler) writeError(ctx *gin.Context, err error) {
	var torznabErr torznab.Error
	if ok := errors.As(err, &torznabErr); ok {
		h.writeXML(ctx, torznabErr)
	} else {
		h.writeHTTPError(ctx, err)
	}
}

func (handler) writeHTTPError(ctx *gin.Context, err error) {
	code := http.StatusInternalServerError

	var httpErr httpError

	if ok := errors.As(err, &httpErr); ok {
		code = httpErr.httpErrorCode()
	}

	_ = ctx.AbortWithError(code, err)
	_, _ = ctx.Writer.WriteString(err.Error() + "\n")
}

type httpError interface {
	error
	httpErrorCode() int
}

type profileNotFoundError struct {
	name string
}

func (e profileNotFoundError) Error() string {
	return fmt.Sprintf("profile not found: %s", e.name)
}

func (profileNotFoundError) httpErrorCode() int {
	return http.StatusNotFound
}

func (h handler) getProfile(c *gin.Context) (torznab.Profile, error) {
	profilePathPart := strings.ToLower(strings.Split(strings.Trim(c.Param("any"), "/"), "/")[0])
	switch profilePathPart {
	case "", "api", torznab.ProfileDefault.ID:
		return torznab.ProfileDefault, nil
	default:
		profile, ok := h.config.GetProfile(profilePathPart)
		if !ok {
			return profile, profileNotFoundError{name: profilePathPart}
		}

		return profile, nil
	}
}

// parseDailyEpisode interprets the Torznab daily-show episode form: season is
// a plausible year and ep is "MM/DD". Returns ok=false for anything else so
// the caller falls back to the numeric-episode path.
func parseDailyEpisode(year int, ep string) (model.Date, bool) {
	if year < 1800 || year > 2999 {
		return model.Date{}, false
	}
	parts := strings.Split(ep, "/")
	if len(parts) != 2 {
		return model.Date{}, false
	}
	month, mErr := strconv.Atoi(parts[0])
	day, dErr := strconv.Atoi(parts[1])
	if mErr != nil || dErr != nil {
		return model.Date{}, false
	}
	// bounds-check the ints BEFORE the narrowing casts: uint8(270) wraps to 14
	// and would pass IsValid as a wrong-but-valid date instead of falling back.
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return model.Date{}, false
	}
	d := model.NewDateFromParts(model.Year(year), time.Month(month), uint8(day))
	if !d.IsValid() {
		return model.Date{}, false
	}
	return d, true
}
