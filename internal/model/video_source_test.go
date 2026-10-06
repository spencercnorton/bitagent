package model

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInferVideoSource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  VideoSource
	}{
		{"Example.Movie.2024.1080p.WEB-DL.H264-GROUP", VideoSourceWEBDL},
		{"Example.Movie.2024.1080p.WEB.DL.H264-GROUP", VideoSourceWEBDL},
		{"Example.Movie.2024.1080p.WEBDL.H264-GROUP", VideoSourceWEBDL},
		{"[web-dl] Example Movie", VideoSourceWEBDL},
		{"WEB-DL", VideoSourceWEBDL},
		{"WEB.DL", VideoSourceWEBDL},
		{"Example.Movie.2024.1080p.WEBRip.H264-GROUP", VideoSourceWEBRip},
		{"Example.Movie.2024.1080p.WEB-Rip.H264-GROUP", VideoSourceWEBRip},
		// Preserve the existing plain WEB alias; a source label is a filename
		// claim, not a verification of how the media was obtained.
		{"Example.Movie.2024.1080p.WEB.H264-GROUP", VideoSourceWEBRip},
		{"Example.Movie.2024.1080p.BluRay.H264-GROUP", VideoSourceBluRay},
		{"Example.Movie.2024.1080p.Blu-Ray.H264-GROUP", VideoSourceBluRay},
		{"Example.Movie.2024.1080p.BDRemux.H264-GROUP", VideoSourceBluRay},
		{"Example.Movie.2024.1080p.BDRip.H264-GROUP", VideoSourceBluRay},
		{"Example.Movie.2024.1080p.BRRip.H264-GROUP", VideoSourceBluRay},
		{"Example.Movie.2024.DVD5.H264-GROUP", VideoSourceDVD},
		{"Example.Movie.2024.DVD9.H264-GROUP", VideoSourceDVD},
		{"Example.Movie.2024.DVDRip.H264-GROUP", VideoSourceDVD},
		{"Example.Show.S01E02.HDTV.H264-GROUP", VideoSourceTV},
		{"Example.Show.S01E02.IPTVRip.H264-GROUP", VideoSourceTV},
		{"Example.Show.S01E02.SATRip.H264-GROUP", VideoSourceTV},
		{"Example.Movie.2024.CAM.H264-GROUP", VideoSourceCAM},
		{"Example.Movie.2024.CAMP.H264-GROUP", ""},
		{"Example.Movie.2024.WEBDLExtra.H264-GROUP", ""},
		{"Example.Movie.2024.SomeWEB-DL.H264-GROUP", ""},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := InferVideoSource(tc.input)
			assert.Equal(t, tc.want != "", got.Valid)
			assert.Equal(t, tc.want, got.VideoSource)
		})
	}
}

func TestVideoSourceRegexStableAliasOrder(t *testing.T) {
	t.Parallel()
	first := createVideoSourceRegex().String()
	for range 64 {
		// Each construction ranges over the alias map afresh. The final regex
		// and the compound-token capture must not depend on that iteration.
		r := createVideoSourceRegex()
		require.Equal(t, first, r.String())
		match := r.FindStringSubmatch("Example.Movie.WEB-DL.H264-GROUP")
		require.Len(t, match, 2)
		assert.Equal(t, "WEB-DL", match[1])
	}
}

func TestInferVideoSourceProcessStability(t *testing.T) {
	const childFlag = "BITAGENT_TEST_VIDEO_SOURCE_CHILD"
	if os.Getenv(childFlag) == "1" {
		require.Equal(t, NewNullVideoSource(VideoSourceWEBDL), InferVideoSource("Example.Movie.WEB-DL.H264-GROUP"))
		require.Equal(t, NewNullVideoSource(VideoSourceWEBDL), InferVideoSource("Example.Movie.WEB.DL.H264-GROUP"))
		return
	}
	for range 4 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestInferVideoSourceProcessStability$")
		cmd.Env = append(os.Environ(), childFlag+"=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
}
