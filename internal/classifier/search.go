package classifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/database/fts"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
)

const (
	localSearchNarrowLimit = 10
	localSearchBroadLimit  = 40
)

type LocalSearch interface {
	ContentByID(context.Context, model.ContentRef) (model.Content, error)
	ContentBySearch(context.Context, model.ContentType, string, model.Year) (model.Content, error)
	// ContentCandidatesBySearch returns up to limit locally stored content
	// rows matching the title (ranked full-text search over titles incl.
	// alt_title:* attributes), for callers that do their own candidate
	// selection — the LLM rerank. No Levenshtein filter is applied.
	ContentCandidatesBySearch(context.Context, model.ContentType, string, model.Year, int) ([]model.Content, error)
}

type localSearch struct {
	search.Search
	// altTitleMatch extends Levenshtein matching to alt_title:* attributes
	// (config AltTitleMatch; default false).
	altTitleMatch bool
	// fuzzyMatchEnabled mirrors Config.FuzzyMatchEnabled; activates ±1 year
	// retrieval window and the fuzzy scoring path.
	fuzzyMatchEnabled bool
}

// contentMatchCandidates returns the title strings a search result may be
// matched against: the canonical title, the original title, and — when
// altTitleMatch is enabled — any stored alternative/translated titles.
func contentMatchCandidates(item search.ContentResultItem, altTitleMatch bool) []string {
	candidates := []string{item.Title}
	if item.OriginalTitle.Valid {
		candidates = append(candidates, item.OriginalTitle.String)
	}

	if altTitleMatch {
		for _, a := range item.Attributes {
			if strings.HasPrefix(a.Key, model.AltTitleAttributePrefix) {
				candidates = append(candidates, a.Value)
			}
		}
	}

	return candidates
}

func (l localSearch) ContentByID(ctx context.Context, ref model.ContentRef) (model.Content, error) {
	options := []query.Option{
		query.Where(
			search.ContentTypeCriteria(ref.Type),
		),
		search.ContentDefaultPreload(),
		search.ContentDefaultHydrate(),
		query.Limit(1),
	}
	if ref.Source == "tmdb" {
		options = append(options, query.Where(
			search.ContentCanonicalIdentifierCriteria(model.ContentRef{
				Source: ref.Source,
				ID:     ref.ID,
			}),
		))
	} else {
		options = append(options, query.Where(
			search.ContentAlternativeIdentifierCriteria(model.ContentRef{
				Source: ref.Source,
				ID:     ref.ID,
			}),
		))
	}

	result, err := l.Content(ctx, options...)
	if err != nil {
		return model.Content{}, err
	}

	if len(result.Items) == 0 {
		return model.Content{}, classification.ErrUnmatched
	}

	return result.Items[0].Content, nil
}

func (l localSearch) ContentBySearch(
	ctx context.Context,
	ct model.ContentType,
	baseTitle string,
	year model.Year,
) (model.Content, error) {
	items, searchErr := l.contentCandidates(ctx, ct, baseTitle, year, localSearchBroadLimit)
	if searchErr != nil {
		return model.Content{}, searchErr
	}

	bestMatch, ok := fuzzyFindBestMatch[search.ContentResultItem](
		baseTitle,
		items,
		func(item search.ContentResultItem) []string {
			return contentMatchCandidates(item, l.altTitleMatch)
		},
		localContentYearPenalty(ct, year),
		l.fuzzyMatchEnabled,
	)
	if !ok {
		return model.Content{}, classification.ErrUnmatched
	}

	return bestMatch.Content, nil
}

type contentSearchPass struct {
	query   string
	limit   int
	useYear bool
}

func (l localSearch) contentCandidates(
	ctx context.Context,
	ct model.ContentType,
	title string,
	year model.Year,
	limit int,
) ([]search.ContentResultItem, error) {
	if limit <= 0 {
		limit = localSearchNarrowLimit
	}

	passes := localContentSearchPasses(title, ct, year, l.fuzzyMatchEnabled, limit)
	items := make([]search.ContentResultItem, 0, limit)
	seen := make(map[model.ContentRef]struct{}, limit)

	for _, pass := range passes {
		if pass.query == "" {
			continue
		}

		result, searchErr := l.contentSearchPass(ctx, ct, year, pass)
		if searchErr != nil {
			return nil, searchErr
		}

		for _, item := range result.Items {
			ref := item.Content.Ref()
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			items = append(items, item)
		}
	}

	return items, nil
}

func localContentYearPenalty(
	ct model.ContentType,
	year model.Year,
) func(search.ContentResultItem) int {
	if ct == model.ContentTypeTvShow {
		return nil
	}
	queryYear := int(year)
	return func(item search.ContentResultItem) int {
		return yearProximityPenalty(queryYear, int(item.Content.ReleaseYear))
	}
}

func (l localSearch) contentSearchPass(
	ctx context.Context,
	ct model.ContentType,
	year model.Year,
	pass contentSearchPass,
) (search.ContentResult, error) {
	options := []query.Option{
		query.Where(search.ContentTypeCriteria(ct)),
		query.SearchString(pass.query),
		query.OrderByQueryStringRank(),
		query.Limit(uint(pass.limit)),
		search.ContentDefaultPreload(),
		search.ContentDefaultHydrate(),
	}
	if pass.useYear && !year.IsNil() {
		if l.fuzzyMatchEnabled {
			// Widen the retrieval window by ±1 year; exact year is handled as a
			// scoring tiebreaker inside fuzzyFindBestMatch via the yearPenalty func.
			options = append(
				options,
				query.Where(search.ContentReleaseDateCriteria(model.NewDateRangeFromYearWindow(year, 1))),
			)
		} else {
			options = append(
				options,
				query.Where(search.ContentReleaseDateCriteria(model.NewDateRangeFromYear(year))),
			)
		}
	}

	return l.Content(ctx, options...)
}

func localContentSearchPasses(
	title string,
	ct model.ContentType,
	year model.Year,
	fuzzyMatchEnabled bool,
	limit int,
) []contentSearchPass {
	if limit <= 0 {
		limit = localSearchNarrowLimit
	}

	queries := localContentSearchQueries(title, fuzzyMatchEnabled)
	passes := make([]contentSearchPass, 0, len(queries)*2)
	for i, q := range queries {
		passLimit := localSearchNarrowLimit
		if i >= 2 {
			passLimit = localSearchBroadLimit
		}
		if passLimit > limit {
			passLimit = limit
		}
		passes = append(passes, contentSearchPass{
			query:   q,
			limit:   passLimit,
			useYear: true,
		})
	}

	// TV torrent years often come from episode air dates or release names, not
	// the series premiere year. If year-filtered retrieval misses the correct
	// show, a second local-only pass without the date filter gives the scorer a
	// chance to pick the exact title. Movies keep the year guard.
	if !year.IsNil() && ct == model.ContentTypeTvShow {
		for _, q := range queries {
			passes = append(passes, contentSearchPass{
				query:   q,
				limit:   min(limit, localSearchBroadLimit),
				useYear: false,
			})
		}
	}

	return passes
}

func localContentSearchQueries(title string, fuzzyMatchEnabled bool) []string {
	title = strings.TrimSpace(strings.ReplaceAll(title, "\"", " "))
	if title == "" {
		return nil
	}

	queries := []string{fmt.Sprintf("\"%s\"", title)}
	if fuzzyMatchEnabled {
		if q := tokenAndSearchQuery(title); q != "" {
			queries = append(queries, q)
		}
		if q := tokenOrSearchQuery(title); q != "" && q != title {
			queries = append(queries, q)
		}
	}

	return dedupeStrings(queries)
}

func tokenAndSearchQuery(title string) string {
	tokens := dedupeStrings(fts.TokenizeFlat(title))
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " ")
}

func tokenOrSearchQuery(title string) string {
	tokens := significantSearchTokens(fts.TokenizeFlat(title))
	if len(tokens) < 2 {
		return ""
	}
	return strings.Join(tokens, " | ")
}

var weakSearchTokens = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "at": {}, "de": {}, "der": {}, "do": {}, "du": {},
	"el": {}, "for": {}, "in": {}, "la": {}, "le": {}, "les": {}, "of": {}, "on": {},
	"or": {}, "the": {}, "to": {}, "und": {}, "with": {},
}

func significantSearchTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		if len(tok) < 2 {
			continue
		}
		if _, weak := weakSearchTokens[tok]; weak {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	if len(out) == 0 {
		return dedupeStrings(tokens)
	}
	return out
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func (l localSearch) ContentCandidatesBySearch(
	ctx context.Context,
	ct model.ContentType,
	title string,
	year model.Year,
	limit int,
) ([]model.Content, error) {
	if limit <= 0 {
		limit = 5
	}

	items, searchErr := l.contentCandidates(ctx, ct, title, year, limit)
	if searchErr != nil {
		return nil, searchErr
	}
	if len(items) > limit {
		items = items[:limit]
	}

	out := make([]model.Content, 0, len(items))
	for _, item := range items {
		out = append(out, item.Content)
	}

	return out, nil
}
