// Package namepolicy makes a pure library-eligibility decision from the
// authoritative release name. It does not infer media tracks, origin or death,
// dispatch work, authorize public grabs or mutate catalogue/blocking state.
package namepolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

const Version = "release-name-policy-v1"

const (
	ReasonDisabled       = "disabled"
	ReasonAllowed        = "allowed"
	ReasonOwnerHash      = "owner_excluded_hash"
	ReasonMissingName    = "name_unavailable"
	ReasonHan            = "han_release_name"
	ReasonCyrillic       = "cyrillic_release_name"
	ReasonAdultComposite = "explicit_adult_name"
	ReasonAdultType      = "explicit_adult_classification"
)

var ErrExcluded = errors.New("release excluded by name policy")

type Decision struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
	Version  string `json:"version"`
}

type Policy struct {
	enabled        bool
	excluded       map[protocol.ID]struct{}
	excludedHashes []protocol.ID
	internalToken  SecretToken
	terms          []*regexp.Regexp
	strong         []bool
}

func New(c Config) (*Policy, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	p := &Policy{enabled: c.Enabled, excluded: map[protocol.ID]struct{}{}, internalToken: c.InternalCheckToken}
	for _, value := range c.ExcludedInfoHashes {
		h, _ := protocol.ParseID(value)
		p.excluded[h] = struct{}{}
	}
	for h := range p.excluded {
		p.excludedHashes = append(p.excludedHashes, h)
	}
	sort.Slice(p.excludedHashes, func(i, j int) bool { return p.excludedHashes[i].String() < p.excludedHashes[j].String() })
	for _, t := range specification().AdultTerms {
		p.terms = append(p.terms, regexp.MustCompile(t.Pattern))
		p.strong = append(p.strong, t.Strong)
	}
	return p, nil
}

func (p *Policy) Enabled() bool { return p != nil && p.enabled }
func (p *Policy) ExcludedHashes() []protocol.ID {
	if p == nil {
		return nil
	}
	return append([]protocol.ID(nil), p.excludedHashes...)
}
func (p *Policy) ExcludesHash(h protocol.ID) bool {
	if !p.Enabled() {
		return false
	}
	_, ok := p.excluded[h]
	return ok
}
func (p *Policy) InternalToken() SecretToken {
	if p == nil {
		return ""
	}
	return p.internalToken
}

// Fingerprint binds the decision configuration, excluding the internal API
// credential. Secret rotation cannot change model/application policy identity.
func (p *Policy) Fingerprint() string {
	body, _ := json.Marshal(struct {
		Definition Specification
		Enabled    bool
		Hashes     []protocol.ID
	}{specification(), p.Enabled(), p.ExcludedHashes()})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func (p *Policy) Evaluate(h protocol.ID, name string) Decision {
	d := Decision{Eligible: true, Reason: ReasonAllowed, Version: Version}
	if !p.Enabled() {
		d.Reason = ReasonDisabled
		return d
	}
	if p.ExcludesHash(h) {
		d.Eligible = false
		d.Reason = ReasonOwnerHash
		return d
	}
	if strings.TrimSpace(name) == "" {
		d.Eligible = false
		d.Reason = ReasonMissingName
		return d
	}
	for _, r := range name {
		if unicode.Is(unicode.Han, r) {
			d.Eligible = false
			d.Reason = ReasonHan
			return d
		}
	}
	for _, r := range name {
		if unicode.Is(unicode.Cyrillic, r) {
			d.Eligible = false
			d.Reason = ReasonCyrillic
			return d
		}
	}
	count, strong := 0, false
	for i, t := range p.terms {
		if t.MatchString(name) {
			count++
			strong = strong || p.strong[i]
		}
	}
	if count >= 2 && strong {
		d.Eligible = false
		d.Reason = ReasonAdultComposite
	}
	return d
}

// EvaluateClassified also uses an already known explicit adult classification;
// it does not run a classifier or treat title/metadata origin as that fact.
func (p *Policy) EvaluateClassified(h protocol.ID, name, contentType string) Decision {
	d := p.Evaluate(h, name)
	if p.Enabled() && d.Eligible && contentType == "xxx" {
		d.Eligible = false
		d.Reason = ReasonAdultType
	}
	return d
}

type RuneRange struct{ Lo, Hi rune }
type Term struct {
	Name, Pattern string
	Strong        bool
}
type Specification struct {
	Version                   string
	UnicodeVersion            string
	Whitespace                string
	Han, Cyrillic             []RuneRange
	AdultTerms                []Term
	MinimumDistinctAdultTerms int
}

func (p *Policy) Specification() Specification { return specification() }

func scriptRanges(table *unicode.RangeTable) []RuneRange {
	var out []RuneRange
	add := func(lo, hi, stride rune) {
		if stride == 1 {
			out = append(out, RuneRange{lo, hi})
			return
		}
		for r := lo; r <= hi; r += stride {
			out = append(out, RuneRange{r, r})
		}
	}
	for _, r := range table.R16 {
		add(rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	for _, r := range table.R32 {
		add(rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	return out
}

// Explicit release delimiters preserve accented/other letter lookalikes. The
// ASCII case classes and regex syntax are shared directly with SQL rendering;
// no Unicode case folding or origin/file-path inference is used.
func termPattern(words ...string) string {
	delimiters := strings.ReplaceAll(regexp.QuoteMeta(catalogueguard.TagWhitespace+`._-+:/\()[]{}!,;=?&'"|#%@`), "-", `\-`)
	separators := strings.ReplaceAll(regexp.QuoteMeta(catalogueguard.TagWhitespace+`._-+`), "-", `\-`)
	var variants []string
	for _, w := range words {
		var letters []string
		for _, r := range w {
			if r >= 'a' && r <= 'z' {
				letters = append(letters, "["+string(r)+string(r-'a'+'A')+"]")
			} else {
				letters = append(letters, regexp.QuoteMeta(string(r)))
			}
		}
		variants = append(variants, strings.Join(letters, "["+separators+"]*"))
	}
	return "(^|[" + delimiters + "])(" + strings.Join(variants, "|") + ")($|[" + delimiters + "])"
}

func specification() Specification {
	s := Specification{Version: Version, UnicodeVersion: unicode.Version, Whitespace: catalogueguard.TagWhitespace, Han: scriptRanges(unicode.Han), Cyrillic: scriptRanges(unicode.Cyrillic), MinimumDistinctAdultTerms: 2}
	for _, w := range []string{"fetish", "porno", "hentai", "milf", "anal", "blowjob", "gangbang", "bukkake", "cumshot"} {
		s.AdultTerms = append(s.AdultTerms, Term{w, termPattern(w), true})
	}
	s.AdultTerms = append(s.AdultTerms, Term{"porn", termPattern("porn", "p0rn", "pr0n"), true})
	for _, w := range []string{"xxx", "adult", "nsfw", "hardcore"} {
		s.AdultTerms = append(s.AdultTerms, Term{w, termPattern(w), false})
	}
	s.AdultTerms = append(s.AdultTerms, Term{"sex", termPattern("sex", "s3x"), false})
	return s
}
