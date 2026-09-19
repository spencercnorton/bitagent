package adapter

import (
	"strconv"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"time"
)

func torrentContentResultToTorznabResult(
	req torznab.SearchRequest,
	res search.TorrentContentResult,
	authoritativeZeroOnly bool,
) torznab.SearchResult {
	entries := make([]torznab.SearchResultItem, 0, len(res.Items))
	for _, item := range res.Items {
		entries = append(entries, torrentContentResultItemToTorznabResultItem(item, authoritativeZeroOnly))
	}

	return torznab.SearchResult{
		Channel: torznab.SearchResultChannel{
			Title: req.Profile.Title,
			Response: torznab.SearchResultResponse{
				Offset: req.Offset.Uint,
				// Total:  res.TotalCount,
			},
			Items: entries,
		},
	}
}

func torrentContentResultItemToTorznabResultItem(item search.TorrentContentResultItem, authoritativeZeroOnly bool) torznab.SearchResultItem {
	category := "Unknown"
	if item.ContentType.Valid {
		category = item.ContentType.ContentType.Label()
	}

	categoryID := torznab.CategoryOther.ID

	if item.ContentType.Valid {
		switch item.ContentType.ContentType {
		case model.ContentTypeMovie:
			categoryID = torznab.CategoryMovies.ID
		case model.ContentTypeTvShow:
			categoryID = torznab.CategoryTV.ID
		case model.ContentTypeMusic:
			categoryID = torznab.CategoryAudio.ID
		case model.ContentTypeEbook:
			categoryID = torznab.CategoryBooks.ID
		case model.ContentTypeComic:
			categoryID = torznab.CategoryBooksComics.ID
		case model.ContentTypeAudiobook:
			categoryID = torznab.CategoryAudioAudiobook.ID
		case model.ContentTypeSoftware:
			categoryID = torznab.CategoryPC.ID
		case model.ContentTypeGame:
			categoryID = torznab.CategoryPCGames.ID
		case model.ContentTypeXxx:
			categoryID = torznab.CategoryXXX.ID
		}
	}

	// Anime is served under BOTH the generic TV category (5000) and the
	// dedicated TV/Anime subcategory (5070). Sonarr anime-type series query
	// and match on 5070 and use absolute numbering; without it, a correctly
	// classified anime is indistinguishable from ordinary TV. Anime-ness is
	// read from the persisted torrent_contents.is_anime flag, computed once at
	// classify time by the full deterministic detector (fansub-group bracket
	// anywhere, "Title - NNN" absolute shape, romaji season markers) — the same
	// signal the server-side cat=5070 filter reads, so browse and per-result
	// emission agree. Only TV gets 5070: the Newznab category set has no
	// anime-movie subcategory, so anime films stay under Movies (2000).
	animeTV := item.ContentType.Valid &&
		item.ContentType.ContentType == model.ContentTypeTvShow &&
		item.IsAnime
	if animeTV {
		category = torznab.CategoryTVAnime.Name
	}

	attrs := []torznab.SearchResultItemTorznabAttr{
		{
			AttrName:  torznab.AttrInfoHash,
			AttrValue: item.Torrent.InfoHash.String(),
		},
		{
			AttrName:  torznab.AttrMagnetURL,
			AttrValue: item.Torrent.MagnetURI(),
		},
		{
			AttrName:  torznab.AttrCategory,
			AttrValue: strconv.Itoa(categoryID),
		},
		{
			AttrName:  torznab.AttrSize,
			AttrValue: strconv.FormatUint(uint64(item.Torrent.Size), 10),
		},
	}

	// A Torznab item may carry multiple category attributes; add 5070 in
	// addition to the primary 5000 so category-agnostic TV queries still
	// match while anime-type series filtering on 5070 can find it.
	if animeTV {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrCategory,
			AttrValue: strconv.Itoa(torznab.CategoryTVAnime.ID),
		})
	}

	attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
		AttrName:  torznab.AttrPublishDate,
		AttrValue: item.PublishedAt.Format(torznab.RssDateDefaultFormat),
	})

	seeders := item.Torrent.Seeders()
	leechers := item.Torrent.Leechers()

	// A bloom-only zero is honest-unknown, not a real 0: omit the attr
	// so an *arr's minimumSeeders check treats the release as unknown
	// instead of rejecting a young, alive swarm. An authoritative
	// tracker verdict (any value, including 0) is always reported.
	emitSeeders := seeders.Valid &&
		(!authoritativeZeroOnly || seeders.Uint > 0 || trackerSeeders(item.Torrent.Sources).Valid)

	if emitSeeders {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrSeeders,
			AttrValue: strconv.Itoa(int(seeders.Uint)),
		})
	}

	// Freshness of the authoritative verdict: the 'tracker' source row's
	// updated_at is bumped on every positive/known-zero scrape (~daily
	// cadence), and the row is deleted on tracker-unknown — so presence of
	// this attr means "a tracker verified these counts at this time", and
	// absence keeps DHT-approximate counts honestly unlabelled. Zero extra
	// queries: sources are already preloaded for the seeders logic above.
	for _, src := range item.Torrent.Sources {
		if src.Source == model.SourceKeyTracker {
			attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
				AttrName:  torznab.AttrSeedsCheckedAt,
				AttrValue: src.UpdatedAt.UTC().Format(time.RFC3339),
			})
			break
		}
	}

	if leechers.Valid {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrLeechers,
			AttrValue: strconv.Itoa(int(leechers.Uint)),
		})
	}

	// English availability (anime): straight from the persisted column —
	// zero extra queries. Absent = unknown or not anime (honest-unknown,
	// same convention as the seeders attr).
	if item.EnglishAudio.Valid {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrEnglishAudio,
			AttrValue: item.EnglishAudio.EnglishAudio.String(),
		})
	}

	if leechers.Valid && emitSeeders {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrPeers,
			AttrValue: strconv.Itoa(int(leechers.Uint) + int(seeders.Uint)),
		})
	}

	if len(item.Torrent.Files) > 0 {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrFiles,
			AttrValue: strconv.Itoa(len(item.Torrent.Files)),
		})
	}

	if !item.Content.ReleaseYear.IsNil() {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrYear,
			AttrValue: strconv.Itoa(int(item.Content.ReleaseYear)),
		})
	}

	if len(item.Episodes) > 0 {
		// should we be adding all seasons and episodes here?
		seasons := item.Episodes.SeasonEntries()
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrSeason,
			AttrValue: strconv.Itoa(seasons[0].Season),
		})

		if len(seasons[0].Episodes) > 0 {
			attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
				AttrName:  torznab.AttrEpisode,
				AttrValue: strconv.Itoa(seasons[0].Episodes[0]),
			})
		}
	}

	if item.VideoCodec.Valid {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrVideo,
			AttrValue: item.VideoCodec.VideoCodec.Label(),
		})
	}

	if item.VideoResolution.Valid {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrResolution,
			AttrValue: item.VideoResolution.VideoResolution.Label(),
		})
	}

	if item.ReleaseGroup.Valid {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrTeam,
			AttrValue: item.ReleaseGroup.String,
		})
	}

	if tmdbID, ok := item.Content.Identifier("tmdb"); ok {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrTmdb,
			AttrValue: tmdbID,
		})
	}

	if imdbID, ok := item.Content.Identifier("imdb"); ok {
		attrs = append(attrs, torznab.SearchResultItemTorznabAttr{
			AttrName:  torznab.AttrImdb,
			AttrValue: imdbID[2:],
		})
	}

	return torznab.SearchResultItem{
		Title:    item.Torrent.Name,
		Size:     item.Torrent.Size,
		Category: category,
		GUID:     item.InfoHash.String(),
		PubDate:  torznab.RSSDate(item.PublishedAt),
		Enclosure: torznab.SearchResultItemEnclosure{
			URL:    item.Torrent.MagnetURI(),
			Type:   "application/x-bittorrent;x-scheme-handler/magnet",
			Length: strconv.FormatUint(uint64(item.Torrent.Size), 10),
		},
		TorznabAttrs: attrs,
	}
}
