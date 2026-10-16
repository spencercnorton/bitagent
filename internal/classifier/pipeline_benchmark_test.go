package classifier

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/model"
	"testing"
)

// Common baseline harness: real core CEL workflow, parser and deterministic
// policy; no metadata provider, optional plugin or external network is involved.
func BenchmarkDeterministicPipeline(b *testing.B) {
	source, err := (yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}).source()
	if err != nil {
		b.Fatal(err)
	}
	c := compiler{options: []compilerOption{compilerFeatures(defaultFeatures), celEnvOption}}
	workflow, err := c.Compile(source)
	if err != nil {
		b.Fatal(err)
	}
	torrents := []model.Torrent{
		{Name: "Synthetic.Amber.2024.1080p.WEB-DL.H264-GROUP.mkv", Size: 734003200, FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("mkv")},
		{Name: "Synthetic.Series.S02E04.2160p.WEB-DL.HEVC.DDP5.1.mkv", Size: 1073741824, FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("mkv")},
		{Name: "Synthetic.Artist-Album-2020.flac", Size: 100000000, FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("flac")},
	}
	flags := Flags{"local_search_enabled": false, "tmdb_enabled": false, "llm_match_enabled": false, "llm_stage_enabled": false}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, e := workflow.Run(context.Background(), "default", flags, torrents[i%len(torrents)])
		if e != nil {
			b.Fatal(e)
		}
	}
}
