package animedb

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spencercnorton/bitagent/internal/model"
)

// Mapping is one anidbid -> TMDB entry from anime-list-full.xml. Only the
// fields the alias backbone needs in stage 1 are kept; the absolute-episode
// mapping (defaulttvdbseason / episodeoffset / <mapping-list>) is a stage-2
// concern and is intentionally not parsed here.
type Mapping struct {
	AniDBID     int
	TMDBType    model.ContentType // model.ContentTypeMovie | model.ContentTypeTvShow
	TMDBID      int64
	PrimaryName string // the AniDB primary romaji name (<name>)
}

// xmlAnime mirrors one <anime> element's attributes we care about. The
// Anime-Lists convention is: tmdbtv = a TMDB *series* id, tmdbid = a TMDB
// *movie* id. tvdbid may be a numeric id, "movie", or "unknown".
type xmlAnime struct {
	AniDBID string `xml:"anidbid,attr"`
	TMDBTV  string `xml:"tmdbtv,attr"`
	TMDBID  string `xml:"tmdbid,attr"`
	Name    string `xml:"name"`
}

func (a xmlAnime) toMapping() (Mapping, bool) {
	aid, err := strconv.Atoi(strings.TrimSpace(a.AniDBID))
	if err != nil || aid <= 0 {
		return Mapping{}, false
	}
	// Prefer a series id (tmdbtv); fall back to a film id (tmdbid). An entry
	// with neither has no TMDB target we can attach, so it is skipped.
	if id, ok := parsePositiveID(a.TMDBTV); ok {
		return Mapping{AniDBID: aid, TMDBType: model.ContentTypeTvShow, TMDBID: id, PrimaryName: strings.TrimSpace(a.Name)}, true
	}
	if id, ok := parsePositiveID(a.TMDBID); ok {
		return Mapping{AniDBID: aid, TMDBType: model.ContentTypeMovie, TMDBID: id, PrimaryName: strings.TrimSpace(a.Name)}, true
	}
	return Mapping{}, false
}

func parsePositiveID(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ParseAnimeList streams anime-list-full.xml and returns the TMDB-mapped
// entries. The file is a few megabytes; a streaming token loop keeps memory
// flat regardless of size. Entries without a TMDB id are dropped.
func ParseAnimeList(r io.Reader) ([]Mapping, error) {
	dec := xml.NewDecoder(r)
	var out []Mapping
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("animedb: parse anime-list: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "anime" {
			continue
		}
		var a xmlAnime
		if err := dec.DecodeElement(&a, &se); err != nil {
			return nil, fmt.Errorf("animedb: decode <anime>: %w", err)
		}
		if m, ok := a.toMapping(); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// AniDB title types (anime-titles.dat header):
//
//	1 = primary title (one per anime)
//	2 = synonyms (multiple)
//	3 = short titles (multiple)
//	4 = official title (one per language)
const (
	titleTypePrimary  = 1
	titleTypeSynonym  = 2
	titleTypeShort    = 3
	titleTypeOfficial = 4
)

// Title is one alias line from anime-titles.dat: "<aid>|<type>|<language>|<title>".
type Title struct {
	AID   int
	Type  int
	Lang  string
	Value string
}

// ParseAnimeTitles reads the pipe-delimited AniDB title dump. `langs`, when
// non-empty, keeps only those language codes (e.g. x-jat, en, ja); an empty set
// keeps every language. Comment lines (leading '#') and malformed lines are
// skipped. The reader must already be decompressed.
func ParseAnimeTitles(r io.Reader, langs map[string]bool) ([]Title, error) {
	sc := bufio.NewScanner(r)
	// Some synopsis-laden synonyms are long; raise the line cap well above the
	// 64KiB default so no legitimate title line is silently truncated/dropped.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out []Title
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// SplitN into 4 so a title containing '|' keeps its remainder intact.
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		aid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || aid <= 0 {
			continue
		}
		typ, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		lang := strings.TrimSpace(parts[2])
		if len(langs) > 0 && !langs[lang] {
			continue
		}
		value := strings.TrimSpace(parts[3])
		if value == "" {
			continue
		}
		out = append(out, Title{AID: aid, Type: typ, Lang: lang, Value: value})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("animedb: scan anime-titles: %w", err)
	}
	return out, nil
}
