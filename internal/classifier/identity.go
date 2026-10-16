package classifier

import (
	"github.com/spencercnorton/bitagent/internal/model"
	"strings"
	"unicode"
)

func altTitlesOf(content model.Content) []string {
	alts := make([]string, 0, len(content.Attributes)+1)
	for _, a := range content.Attributes {
		if strings.HasPrefix(a.Key, model.AltTitleAttributePrefix) {
			alts = append(alts, a.Value)
		}
	}
	if content.OriginalTitle.Valid {
		alts = append(alts, content.OriginalTitle.String)
	}
	return alts
}

func normalizeIdentityTitle(title string) string {
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case '\'', '’', 'ʼ', '`':
			return -1
		}
		return r
	}, normalizeTitleForMatch(title))
	return strings.Join(strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}), " ")
}

func exactContentTitle(title string, c model.Content, aliases bool) bool {
	n := normalizeIdentityTitle(title)
	if n == "" {
		return false
	}
	if n == normalizeIdentityTitle(c.Title) {
		return true
	}
	if c.OriginalTitle.Valid && n == normalizeIdentityTitle(c.OriginalTitle.String) {
		return true
	}
	if aliases {
		for _, a := range altTitlesOf(c) {
			if n == normalizeIdentityTitle(a) {
				return true
			}
		}
	}
	return false
}

// ExactContentTitle is the strict core title/official-title identity gate.
func ExactContentTitle(title string, c model.Content) bool { return exactContentTitle(title, c, false) }
