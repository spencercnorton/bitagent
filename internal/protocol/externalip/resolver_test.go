package externalip

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testLogger(t *testing.T) *zap.SugaredLogger {
	t.Helper()

	return zap.NewNop().Sugar()
}

func newPlainTextIPServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestResolver_staticOverrideShortCircuits(t *testing.T) {
	t.Parallel()

	r, err := New(Config{Override: "203.0.113.42"}, testLogger(t))
	require.NoError(t, err)

	addr, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.42", addr.String())
}

func TestResolver_staticOverrideRejectsInvalidIP(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Override: "not-an-ip"}, testLogger(t))
	assert.Error(t, err)
}

func TestResolver_firstSourceWins(t *testing.T) {
	t.Parallel()

	srv := newPlainTextIPServer(t, "198.51.100.7\n", http.StatusOK)
	defer srv.Close()

	r, err := New(Config{Sources: []string{srv.URL}}, testLogger(t))
	require.NoError(t, err)

	addr, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.7", addr.String())
}

func TestResolver_fallsThroughOnError(t *testing.T) {
	t.Parallel()

	bad := newPlainTextIPServer(t, "oops", http.StatusInternalServerError)
	defer bad.Close()
	good := newPlainTextIPServer(t, "198.51.100.8", http.StatusOK)
	defer good.Close()

	r, err := New(Config{Sources: []string{bad.URL, good.URL}}, testLogger(t))
	require.NoError(t, err)

	addr, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.8", addr.String())
}

func TestResolver_allSourcesFailReturnsError(t *testing.T) {
	t.Parallel()

	bad := newPlainTextIPServer(t, "oops", http.StatusInternalServerError)
	defer bad.Close()

	r, err := New(Config{Sources: []string{bad.URL}}, testLogger(t))
	require.NoError(t, err)

	_, err = r.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 1 sources failed")
}

func TestResolver_rejectsNonIPBody(t *testing.T) {
	t.Parallel()

	// A misconfigured proxy might return HTML — the resolver must
	// reject that instead of happily parsing "some" out of
	// "<html><body>some bytes</body></html>".
	misbehaving := newPlainTextIPServer(t, "<html><body>no ip here</body></html>", http.StatusOK)
	defer misbehaving.Close()

	r, err := New(Config{Sources: []string{misbehaving.URL}}, testLogger(t))
	require.NoError(t, err)

	_, err = r.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unparseable body")
}

func TestResolver_capsBodyAt128Bytes(t *testing.T) {
	t.Parallel()

	// Oversized but valid-IPv4-prefix body: the cap must keep us from
	// reading unbounded data; the truncation will corrupt the parse,
	// which we expect to surface as an error.
	filler := make([]byte, 256)
	for i := range filler {
		filler[i] = 'x'
	}
	srv := newPlainTextIPServer(t, fmt.Sprintf("198.51.100.9%s", filler), http.StatusOK)
	defer srv.Close()

	r, err := New(Config{Sources: []string{srv.URL}}, testLogger(t))
	require.NoError(t, err)

	_, err = r.Resolve(context.Background())
	require.Error(t, err) // truncated-with-x's isn't a valid IP
}

func TestResolver_timeoutBoundsSlowSource(t *testing.T) {
	t.Parallel()

	// Server that hangs forever. The resolver must give up within
	// Timeout and return an error, not block the caller.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()

	r, err := New(Config{
		Sources: []string{slow.URL},
		Timeout: 50 * time.Millisecond,
	}, testLogger(t))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()

	_, err = r.Resolve(ctx)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 300*time.Millisecond,
		"timeout should bound the call well under ctx deadline")
}

// staticResolver satisfies the Resolver interface — keep the
// compile-time check so future refactors of the interface surface the
// break here.
var _ Resolver = staticResolver{addr: netip.MustParseAddr("198.51.100.1")}

// --- validation suite ---------------------------------------------
//
// These guard against the "resolver returned junk and we silently
// built a BEP-42 ID from it" class of bug the reviewer flagged as a
// correctness gap.

func TestValidateAddr_rejectsNonGlobalAddresses(t *testing.T) {
	t.Parallel()

	reject := []string{
		"10.0.0.1",           // RFC1918
		"172.16.0.1",         // RFC1918
		"192.168.1.1",        // RFC1918
		"127.0.0.1",          // loopback
		"169.254.0.1",        // link-local
		"100.64.0.1",         // CGNAT
		"100.127.255.254",    // CGNAT upper bound
		"0.0.0.0",            // unspecified
		"224.0.0.1",          // IPv4 multicast
		"2001:db8::1",        // IPv6 (until dual-stack support)
		"fe80::1",            // IPv6 link-local
		"::1",                // IPv6 loopback
	}

	for _, s := range reject {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			a, err := netip.ParseAddr(s)
			require.NoError(t, err)
			verr := validateAddr(a)
			require.Error(t, verr)
			assert.ErrorIs(t, verr, ErrInvalidAddress)
		})
	}
}

func TestValidateAddr_acceptsGlobalIPv4(t *testing.T) {
	t.Parallel()

	accept := []string{
		"8.8.8.8",
		"1.1.1.1",
		"198.51.100.1",   // TEST-NET-2 — globally-routable for our purposes
		"184.75.208.178", // current AirVPN egress
	}

	for _, s := range accept {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			a, err := netip.ParseAddr(s)
			require.NoError(t, err)
			assert.NoError(t, validateAddr(a))
		})
	}
}

func TestResolver_overrideRejectsNonPublic(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Override: "10.0.0.1"}, testLogger(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAddress)
}

func TestResolver_skipsSourceReturningNonPublic(t *testing.T) {
	t.Parallel()

	// First source: valid-looking body but a non-public address (a
	// misconfigured proxy can happily hand back 10.x). Resolver MUST
	// NOT build an ID from it; must fall through to the next source.
	badAddr := newPlainTextIPServer(t, "10.0.0.1", http.StatusOK)
	defer badAddr.Close()
	good := newPlainTextIPServer(t, "198.51.100.42", http.StatusOK)
	defer good.Close()

	r, err := New(Config{Sources: []string{badAddr.URL, good.URL}}, testLogger(t))
	require.NoError(t, err)

	addr, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.42", addr.String())
}

func TestResolver_skipsSourceReturningCGNAT(t *testing.T) {
	t.Parallel()

	// A tunnel leak might hand us the Tailscale CGNAT. That's
	// BEP-42-useless. Refuse and move on.
	cgnat := newPlainTextIPServer(t, "100.100.1.1", http.StatusOK)
	defer cgnat.Close()
	good := newPlainTextIPServer(t, "198.51.100.43", http.StatusOK)
	defer good.Close()

	r, err := New(Config{Sources: []string{cgnat.URL, good.URL}}, testLogger(t))
	require.NoError(t, err)

	addr, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.43", addr.String())
}

func TestResolver_skipsSourceReturningIPv6(t *testing.T) {
	t.Parallel()

	// A Happy-Eyeballs-at-the-CDN source might flip to v6 on one call.
	// We want a single-family node ID — refuse and use the v4 source.
	v6 := newPlainTextIPServer(t, "2001:db8::1", http.StatusOK)
	defer v6.Close()
	v4 := newPlainTextIPServer(t, "198.51.100.44", http.StatusOK)
	defer v4.Close()

	r, err := New(Config{Sources: []string{v6.URL, v4.URL}}, testLogger(t))
	require.NoError(t, err)

	addr, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.44", addr.String())
}

func TestResolver_unwrapsIPv4In6(t *testing.T) {
	t.Parallel()

	// Some sources (ifconfig.me) wrap v4 as ::ffff:w.x.y.z. Validator
	// must see a pure v4 after unwrap, not reject it as "not v4".
	srv := newPlainTextIPServer(t, "::ffff:198.51.100.45", http.StatusOK)
	defer srv.Close()

	r, err := New(Config{Sources: []string{srv.URL}}, testLogger(t))
	require.NoError(t, err)

	addr, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.True(t, addr.Is4(), "must unwrap ::ffff: to a true v4 addr")
	assert.Equal(t, "198.51.100.45", addr.String())
}

func TestResolver_allSourcesBadAddressStillErrors(t *testing.T) {
	t.Parallel()

	// All sources return addresses that fail validation. The resolver
	// returns an error wrapping ErrInvalidAddress so callers can
	// distinguish from "sources unreachable".
	a := newPlainTextIPServer(t, "10.0.0.1", http.StatusOK)
	defer a.Close()
	b := newPlainTextIPServer(t, "127.0.0.1", http.StatusOK)
	defer b.Close()

	r, err := New(Config{Sources: []string{a.URL, b.URL}}, testLogger(t))
	require.NoError(t, err)

	_, err = r.Resolve(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAddress)
}
