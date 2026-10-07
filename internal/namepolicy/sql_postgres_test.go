package namepolicy

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestSharedSQLScriptClassIsExactGoHanCyrillic(t *testing.T) {
	p, err := New(Config{Enabled: true})
	require.NoError(t, err)
	spec := p.Specification()
	r := regexp.MustCompile(runeClass(append(spec.Han, spec.Cyrillic...)))
	for c := rune(0); c <= unicode.MaxRune; c++ {
		if c >= 0xd800 && c <= 0xdfff {
			continue
		}
		want := unicode.Is(unicode.Han, c) || unicode.Is(unicode.Cyrillic, c)
		if r.MatchString(string(c)) != want {
			t.Fatalf("script mismatch at U+%04X", c)
		}
	}
	disabled, args := (*Policy)(nil).AllowsSQL("untrusted input", "also untrusted")
	require.Equal(t, "TRUE", disabled)
	require.Empty(t, args)
}

func TestPostgresNamePredicateMatchesPureDecision(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	defer pool.Close()
	hash := protocol.ID{1}
	excluded := protocol.ID{2}
	p, err := New(Config{Enabled: true, ExcludedInfoHashes: []string{excluded.String()}})
	require.NoError(t, err)
	names := []string{"Synthetic.Ordinary.2026", "Amélie.2001", "テスト.2026", "테스트.2026", "Άλφα.2026", "Synthetic.测试.2026", "Synthetic.Фильм.2026", "English.Dub.中字.2026", "", "\u3000\u0085", "Fetish.XXX.2026", "F.E.T.I.S.H_X-X-X.2026", "Fetish.p0rn", "Fetish.S3X", "Fetish.Fetish.2026", "xXx.2002.1080p", "Sex.and.The.City.2008", "Fetish.2005.Drama", "Sussex.Hardcore.2026", "Fetishé.XXX", "ſex.Fetish", "Analytical.XXX", "Porn.Porno.2026", "Fetish/XXX", "Fetish\u3000XXX"}
	spec := p.Specification()
	for _, spans := range [][]RuneRange{spec.Han, spec.Cyrillic} {
		for _, span := range spans {
			for _, c := range []rune{span.Lo - 1, span.Lo, span.Hi, span.Hi + 1} {
				if c > 0 && c <= unicode.MaxRune && !(c >= 0xd800 && c <= 0xdfff) {
					names = append(names, "Synthetic."+string(c)+".2026")
				}
			}
		}
	}
	sql, args := p.AllowsSQL("candidate.name", "candidate.info_hash")
	i := 2
	var b strings.Builder
	for _, c := range sql {
		if c == '?' {
			i++
			b.WriteString(fmt.Sprint("$", i))
		} else {
			b.WriteRune(c)
		}
	}
	for _, name := range names {
		for _, h := range []protocol.ID{hash, excluded} {
			var allowed bool
			all := append([]any{name, h.Bytes()}, args...)
			err = pool.QueryRow(context.Background(), "SELECT ("+b.String()+") FROM (SELECT $1::text name,$2::bytea info_hash) candidate", all...).Scan(&allowed)
			require.NoError(t, err, "%q", name)
			require.Equal(t, p.Evaluate(h, name).Eligible, allowed, "%q", name)
		}
	}
}
