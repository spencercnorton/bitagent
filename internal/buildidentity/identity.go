package buildidentity

import (
	"github.com/spencercnorton/bitagent/internal/version"
	"runtime/debug"
	"strings"
)

func CurrentBuildIdentity() string {
	if tag := strings.TrimSpace(version.GitTag); tag != "" {
		return tag
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value
			}
		}
		if revision != "" {
			return "vcs:" + revision + ";modified:" + modified
		}
	}
	return "unversioned-build"
}
