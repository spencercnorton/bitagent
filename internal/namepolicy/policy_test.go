package namepolicy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/go-playground/validator/v10"
	"github.com/spencercnorton/bitagent/internal/config"
	"github.com/spencercnorton/bitagent/internal/config/configresolver"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestReleaseNamePolicyExplicitScopeAndLookalikes(t *testing.T) {
	p, err := New(Config{Enabled: true})
	require.NoError(t, err)
	for _, name := range []string{
		"Synthetic.电影.ENG.DUB.mkv",
		"Synthetic.Фильм.English.mkv",
		"Synthetic.Ж.2024.mkv",
		"Fetish.XXX.2024.mkv",
		"f.e.t.i.s.h.x-x-x.mkv",
		"FETISH+S3X.mkv",
		"Fetish\u00a0Porn.mkv",
		"Fetish.p0rn.mkv",
		"FetishXXX.mkv",
		"F.e.t.i.s.h.X.X.X.mkv",
		"FetishPorn.mkv",
		"PornXXX.mkv",
		"Porno.XXX.mkv",
		"P0rn.X.X.X.mkv",
		"Gaping.Anal.XXX.mkv",
	} {
		d := p.Evaluate(protocol.ID{}, name)
		require.False(t, d.Eligible, name)
		require.Equal(t, Version, d.Version)
	}
	for _, name := range []string{
		"Café.2024.ENG.mkv",
		"České.Film.2024.mkv",
		"Shen.Ming.2024.ENG.mkv",
		"Zhui.Qin.Ai.2024.mkv",
		"Synthetic.Greek.Α.ENG.mkv",
		"Synthetic.한국어.ENG.mkv",
		"xXx.2002.1080p.mkv",
		"xXx.Return.of.Xander.Cage.mkv",
		"Fetish.2024.Documentary.mkv",
		"Sex.Education.S01.mkv",
		"Hardcore.Henry.2015.mkv",
		"Caféfetish.xxx.mkv",
		"Fetish.xxxé.mkv",
		"FetishFiction.xxx.mkv",
		"pornography.2024.mkv",
		"South.Park.Cartman.Gets.an.Anal.Probe.Season.XXX.mkv",
		"MILF.2018.adult.comedy.mkv",
		"Porno.2019.Adult.Comedy.mkv",
		"Hentai.Documentary.XXX.mkv",
		"FétishXXX.mkv",
	} {
		d := p.Evaluate(protocol.ID{}, name)
		require.True(t, d.Eligible, name)
	}
	// A release name is the only string this pure gate examines; paths and
	// original-language metadata are not supplied as surrogate release names.
	require.True(t, p.Evaluate(protocol.ID{}, "Allowed.Release.ENG.mkv").Eligible)
	require.False(t, p.EvaluateClassified(protocol.ID{}, "Allowed.Release.mkv", "xxx").Eligible)
	require.True(t, p.EvaluateClassified(protocol.ID{}, "xXx.2002.mkv", "movie").Eligible)
}

func TestDefaultOffPrivateHashConfigurationAndMissingName(t *testing.T) {
	p, err := New(NewDefaultConfig())
	require.NoError(t, err)
	require.True(t, p.Evaluate(protocol.ID{}, "汉字.XXX.Fetish").Eligible)
	require.Equal(t, ReasonDisabled, p.Evaluate(protocol.ID{}, "").Reason)
	h := protocol.ID{1}
	p, err = New(Config{Enabled: true, ExcludedInfoHashes: []string{h.String()}})
	require.NoError(t, err)
	require.Equal(t, ReasonOwnerHash, p.Evaluate(h, "Allowed.Name").Reason)
	require.Equal(t, ReasonMissingName, p.Evaluate(protocol.ID{}, " \u00a0\t").Reason)
	require.Equal(t, []protocol.ID{h}, p.ExcludedHashes())
	copy := p.ExcludedHashes()
	copy[0] = protocol.ID{2}
	require.Equal(t, []protocol.ID{h}, p.ExcludedHashes())
}

func TestSharedScriptRangesExactlyMatchGoUnicodeMembership(t *testing.T) {
	s := specification()
	for _, pair := range []struct {
		ranges []RuneRange
		table  *unicode.RangeTable
	}{{s.Han, unicode.Han}, {s.Cyrillic, unicode.Cyrillic}} {
		for r := rune(0); r <= unicode.MaxRune; r++ {
			found := false
			for _, v := range pair.ranges {
				if r >= v.Lo && r <= v.Hi {
					found = true
					break
				}
			}
			if found != unicode.Is(pair.table, r) {
				t.Fatalf("script membership differs at U+%04X", r)
			}
		}
	}
}

func TestInternalCredentialRedactionAndFingerprintIndependence(t *testing.T) {
	secret := SecretToken(strings.Repeat("synthetic-key-", 3))
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		require.NotContains(t, fmt.Sprintf(format, secret), string(secret))
	}
	body, err := json.Marshal(Config{Enabled: true, InternalCheckToken: secret})
	require.NoError(t, err)
	require.NotContains(t, string(body), string(secret))
	a, err := New(Config{Enabled: true, InternalCheckToken: secret})
	require.NoError(t, err)
	b, err := New(Config{Enabled: true, InternalCheckToken: SecretToken(strings.Repeat("different-key-", 3))})
	require.NoError(t, err)
	require.Equal(t, a.Fingerprint(), b.Fingerprint())
	_, err = New(Config{InternalCheckToken: "short"})
	require.Error(t, err)
}

func TestResolvedCredentialRetainsValueWithoutCLIOrDebugDisclosure(t *testing.T) {
	secret := strings.Repeat("synthetic-key-", 3)
	r, err := config.New(config.Params{Specs: []config.Spec{{Key: "name_policy", DefaultValue: NewDefaultConfig()}}, Resolvers: []configresolver.Resolver{configresolver.NewEnv(map[string]string{"NAME_POLICY_INTERNAL_CHECK_TOKEN": secret})}, Validate: validator.New()})
	require.NoError(t, err)
	actual := r.Resolved.NodeMap["name_policy"].Value.(Config)
	require.Equal(t, secret, string(actual.InternalCheckToken))
	leaf := r.Resolved.NodeMap["name_policy"].ChildMap["internal_check_token"]
	require.Equal(t, "[redacted]", leaf.ValueLabel)
	body, err := json.Marshal(r.Resolved)
	require.NoError(t, err)
	require.NotContains(t, string(body), secret)
	require.NotContains(t, fmt.Sprintf("%+v", r.Resolved), secret)
}
