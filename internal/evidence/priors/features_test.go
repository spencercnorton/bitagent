package priors

import (
	"reflect"
	"testing"
)

func TestExtract_Movie(t *testing.T) {
	got := Extract(
		"The.Boys.S04E01.1080p.WEB-DL.x265-NTb",
		[]string{"http://tracker.opentrackr.org:1337/announce", "udp://tracker.coppersurfer.tk:6969"},
		"mkv",
	)
	want := []FeatureKey{
		{Type: FeatureCodec, Value: "x265"},
		{Type: FeatureExtension, Value: "mkv"},
		{Type: FeatureQualityTag, Value: "webdl"},
		{Type: FeatureReleaseGroup, Value: "ntb"},
		{Type: FeatureResolution, Value: "1080p"},
		{Type: FeatureSource, Value: "coppersurfer.tk"},
		{Type: FeatureSource, Value: "opentrackr.org"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestExtract_Dedupe(t *testing.T) {
	// Two source URLs to the same apex+TLD should collapse.
	got := Extract(
		"Movie.2024.1080p.BluRay.x264-RARBG",
		[]string{
			"http://tracker.rarbg.to:1337/announce",
			"udp://tracker.rarbg.to:1337",
		},
		"mp4",
	)
	sources := 0
	for _, k := range got {
		if k.Type == FeatureSource {
			sources++
		}
	}
	if sources != 1 {
		t.Fatalf("expected 1 source feature after dedupe, got %d (%+v)", sources, got)
	}
}

func TestExtract_RejectsNoiseAsReleaseGroup(t *testing.T) {
	// Names that end in -1080p / -x264 must NOT be treated as a release group.
	for _, name := range []string{"Foo.Bar-1080p", "Baz.Qux-x265"} {
		ks := Extract(name, nil, "")
		for _, k := range ks {
			if k.Type == FeatureReleaseGroup {
				t.Fatalf("%q: false-positive release group %q", name, k.Value)
			}
		}
	}
}

func TestExtract_PrimaryExtensionWithDot(t *testing.T) {
	got := Extract("Foo", nil, ".MKV")
	want := []FeatureKey{{Type: FeatureExtension, Value: "mkv"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestExtract_Empty(t *testing.T) {
	got := Extract("", nil, "")
	if len(got) != 0 {
		t.Fatalf("expected no features for empty inputs, got %#v", got)
	}
}

func TestNormaliseSource(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://tracker.openbittorrent.com:80/announce", "openbittorrent.com"},
		{"udp://9.rarbg.to:2900", "rarbg.to"},
		{"https://www.subdomain.example.org/path", "example.org"},
		{"  ", ""},
	}
	for _, c := range cases {
		if got := normaliseSource(c.in); got != c.want {
			t.Errorf("normaliseSource(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestExtractCodecAliases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Foo.Bar.h265.mkv", "x265"},
		{"Foo.Bar.HEVC", "x265"},
		{"Foo.Bar.h.264", "x264"},
		{"Foo.Bar.AVC.WEB-DL", "x264"},
		{"Foo.Bar.AV1.WEBRip", "av1"},
		{"Foo.Bar.XviD", "xvid"},
	}
	for _, c := range cases {
		if got := extractCodec(c.in); got != c.want {
			t.Errorf("extractCodec(%q) = %q want %q", c.in, got, c.want)
		}
	}
}
