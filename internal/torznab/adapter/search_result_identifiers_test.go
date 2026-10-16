package adapter

import (
	"encoding/xml"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestTorznabArrIdentifierNamesSurviveXML(t *testing.T) {
	for _, kind := range []model.ContentType{model.ContentTypeMovie, model.ContentTypeTvShow} {
		item := torrentContentResultItemToTorznabResultItem(search.TorrentContentResultItem{TorrentContent: model.TorrentContent{
			ContentType: model.NewNullContentType(kind),
			Torrent:     model.Torrent{Name: "Amber.Signal.2025.S01E01.1080p.mkv"},
			Content:     model.Content{Type: kind, Source: model.SourceTmdb, ID: "42", Attributes: []model.ContentAttribute{{Key: "id", Source: "tvdb", Value: "84"}, {Key: "id", Source: "imdb", Value: "tt0000042"}}},
		}}, false)
		raw, err := xml.Marshal(item)
		require.NoError(t, err)
		attrs := map[string]string{}
		dec := xml.NewDecoder(strings.NewReader(string(raw)))
		for {
			token, e := dec.Token()
			if e != nil {
				break
			}
			if start, ok := token.(xml.StartElement); ok && strings.HasSuffix(start.Name.Local, "attr") {
				name, value := "", ""
				for _, a := range start.Attr {
					if a.Name.Local == "name" {
						name = a.Value
					}
					if a.Name.Local == "value" {
						value = a.Value
					}
				}
				attrs[name] = value
			}
		}
		// Literal protocol keys, independently of the implementation constants.
		require.Equal(t, "42", attrs["tmdbid"])
		require.Equal(t, "42", attrs["tmdb"])
		require.Equal(t, "0000042", attrs["imdb"])
		if kind == model.ContentTypeTvShow {
			require.Equal(t, "84", attrs["tvdbid"])
		} else {
			require.NotContains(t, attrs, "tvdbid")
		}
	}
}

func TestTorznabOmitsMalformedCatalogueIdentifiers(t *testing.T) {
	for _, id := range []string{"", "0", "-1", "abc", "2147483648", "42junk"} {
		item := torrentContentResultItemToTorznabResultItem(search.TorrentContentResultItem{TorrentContent: model.TorrentContent{Content: model.Content{Source: model.SourceTmdb, ID: id, Attributes: []model.ContentAttribute{{Key: "id", Source: "imdb", Value: id}}}}}, false)
		for _, a := range item.TorznabAttrs {
			require.NotContains(t, []string{"tmdb", "tmdbid", "imdb", "tvdbid"}, a.AttrName)
		}
	}
}
