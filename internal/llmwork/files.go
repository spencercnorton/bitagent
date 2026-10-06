package llmwork

import (
	"sort"

	"github.com/spencercnorton/bitagent/internal/model"
)

// OrderedFiles copies file evidence into native index/path order. Preload row
// order is not request evidence and must not change task or provider identity.
// Ties keep synthetic or malformed duplicate-index input deterministic too.
func OrderedFiles(files []model.TorrentFile) []model.TorrentFile {
	ordered := append([]model.TorrentFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Size != b.Size {
			return a.Size < b.Size
		}
		if a.Extension.Valid != b.Extension.Valid {
			return !a.Extension.Valid
		}
		return a.Extension.String < b.Extension.String
	})
	return ordered
}
