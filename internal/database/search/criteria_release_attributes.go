package search

import (
	"encoding/json"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/database/query"
)

// TorrentContentReleaseAttributeCriteria matches advertised filename claims.
// Unknown values and fields fail validation; SQL keys and values are bound.
func TorrentContentReleaseAttributeCriteria(field, value string) query.Criteria {
	return query.GenCriteria(func(query.DBContext) (query.Criteria, error) {
		allowed := map[string]map[string]bool{
			"hdrFormats":    {"HDR": true, "HDR10": true, "HDR10_PLUS": true, "DOLBY_VISION": true},
			"audioFormats":  {"TRUEHD": true, "DTS": true, "EAC3": true, "AC3": true, "AAC": true, "FLAC": true, "OPUS": true},
			"audioFeatures": {"ATMOS": true, "DTS_X": true},
			"revisions":     {"PROPER": true, "REPACK": true, "READNFO": true, "INTERNAL": true},
			"encoder":       {"x264": true, "x265": true},
			"audioChannels": {"1.0": true, "2.0": true, "2.1": true, "3.0": true, "3.1": true, "4.0": true, "4.1": true, "5.0": true, "5.1": true, "6.0": true, "6.1": true, "7.0": true, "7.1": true, "8.0": true, "8.1": true, "9.0": true, "9.1": true,
				"1ch": true, "2ch": true, "3ch": true, "4ch": true, "5ch": true, "6ch": true, "7ch": true, "8ch": true, "9ch": true},
		}
		if !allowed[field][value] {
			return nil, fmt.Errorf("unsupported advertised release attribute %s", field)
		}
		var wanted any = value
		if field != "encoder" && field != "audioChannels" {
			wanted = []string{value}
		}
		body, err := json.Marshal(map[string]any{field: wanted, "version": 1})
		if err != nil {
			return nil, err
		}
		return query.DBCriteria{SQL: "torrent_contents.release_attributes @> ?::jsonb", Args: []any{string(body)}}, nil
	})
}
