// Package externalip resolves the caller's public egress IP.
//
// The crawler derives its BEP-42 DHT node ID from this value, so the
// answer has to track the real VPN egress — not a value baked in at
// compose time. We fetch from a small pool of plain-text IP echo
// services and return the first success that passes validation. A
// periodic watcher (NewWatcher) re-checks at a configured cadence and
// fires a callback when the current BEP-42-compliant node ID no longer
// verifies against the observed IP.
//
// Validation is strict by default: only globally-routable IPv4
// addresses are accepted. RFC1918, loopback, link-local, CGNAT
// (100.64.0.0/10), and the whole IPv6 family are rejected — a
// resolver result that doesn't pass validation is treated the same
// as a transport error (logged, counted, not fired).
//
// Falls back to a static override if one is configured, which is
// useful for break-glass situations.
package externalip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Resolver returns the current external IP. Implementations must be
// safe for concurrent use. Returned addresses are guaranteed to have
// passed validation (see validateAddr).
type Resolver interface {
	Resolve(ctx context.Context) (netip.Addr, error)
}

// defaultSources is the pool of **IPv4-pinned** plain-text IP echo
// endpoints. Each MUST return just the IP address in the response
// body (no JSON wrapping, no trailing whitespace other than a
// newline). IPv4-specific hostnames (`api4.`, `ipv4.`) prevent Happy
// Eyeballs from flipping us to IPv6 mid-run — a single BEP-42 node ID
// is tied to a single address family, so mixing is a bug.
var defaultSources = []string{
	"https://api4.ipify.org",
	"https://ipv4.icanhazip.com",
	"https://ifconfig.me/ip",
}

// cgnatPrefix is BCP 153 / RFC 6598 shared address space. Go's
// netip.Addr doesn't surface a dedicated predicate for it, but a
// BEP-42 ID derived from a CGNAT address is useless — our egress
// should always be globally-routable public space. If the resolver
// hands us CGNAT, the tunnel is likely leaking or the resolver is
// lying; either way, refuse to build an ID from it.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// Config tunes the resolver.
type Config struct {
	// Sources is the list of plain-text-IP HTTP endpoints to try, in
	// order. Zero-value uses defaultSources.
	Sources []string
	// Timeout bounds each individual HTTP attempt. Zero-value is 10s.
	Timeout time.Duration
	// Override, if non-empty, short-circuits all network lookups and
	// always returns this IP. Used for break-glass and offline tests.
	// The override is still validated — you can't pin to 127.0.0.1.
	Override string
}

// ErrInvalidAddress is returned when a resolver result fails
// validation (wrong family, not globally-routable, CGNAT, etc.).
// Errors from the multi-source resolver wrap this so callers can
// distinguish "resolver returned garbage" from "all sources
// unreachable". Operators should see both in metrics but not treat
// them the same when deciding whether to restart.
var ErrInvalidAddress = errors.New("externalip: address failed validation")

// New builds a Resolver. If cfg.Override is set it short-circuits
// to a static resolver (after validating the override); otherwise
// it's the multi-source HTTP resolver.
func New(cfg Config, logger *zap.SugaredLogger) (Resolver, error) {
	if cfg.Override != "" {
		addr, err := netip.ParseAddr(cfg.Override)
		if err != nil {
			return nil, fmt.Errorf("externalip: override %q is not a valid IP: %w", cfg.Override, err)
		}
		if err := validateAddr(addr); err != nil {
			return nil, fmt.Errorf("externalip: override %q is not usable: %w", cfg.Override, err)
		}

		logger.Infow("external IP resolver: static override active", "ip", addr.String())

		return staticResolver{addr: addr}, nil
	}

	sources := cfg.Sources
	if len(sources) == 0 {
		sources = defaultSources
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return &multiSourceResolver{
		sources: sources,
		client:  &http.Client{Timeout: timeout},
		logger:  logger,
	}, nil
}

// validateAddr enforces "must be a globally-routable IPv4 address we
// can derive a BEP-42 anchor from." The reviewer explicitly flagged
// this as a correctness gap: handing the BEP-42 derivation a
// non-public address silently builds an ID no peer will accept.
//
// Returned errors wrap ErrInvalidAddress so callers can discriminate.
func validateAddr(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid zero addr", ErrInvalidAddress)
	}
	// IPv6 support would need either per-family node IDs (we have
	// one) or careful dual-stack accounting. Keep IPv4-only until
	// that design work happens.
	if !addr.Is4() && !addr.Is4In6() {
		return fmt.Errorf("%w: not IPv4 (%s)", ErrInvalidAddress, addr.String())
	}
	if addr.IsLoopback() {
		return fmt.Errorf("%w: loopback (%s)", ErrInvalidAddress, addr.String())
	}
	if addr.IsPrivate() { // RFC1918
		return fmt.Errorf("%w: RFC1918 private (%s)", ErrInvalidAddress, addr.String())
	}
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: link-local (%s)", ErrInvalidAddress, addr.String())
	}
	if addr.IsMulticast() {
		return fmt.Errorf("%w: multicast (%s)", ErrInvalidAddress, addr.String())
	}
	if addr.IsUnspecified() {
		return fmt.Errorf("%w: unspecified (%s)", ErrInvalidAddress, addr.String())
	}
	if cgnatPrefix.Contains(addr) {
		return fmt.Errorf("%w: CGNAT 100.64.0.0/10 (%s)", ErrInvalidAddress, addr.String())
	}

	return nil
}

type staticResolver struct {
	addr netip.Addr
}

func (s staticResolver) Resolve(context.Context) (netip.Addr, error) { return s.addr, nil }

type multiSourceResolver struct {
	sources []string
	client  *http.Client
	logger  *zap.SugaredLogger
}

func (m *multiSourceResolver) Resolve(ctx context.Context) (netip.Addr, error) {
	var errs []error

	for _, url := range m.sources {
		addr, err := m.fetchOne(ctx, url)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		if verr := validateAddr(addr); verr != nil {
			// A source that returned a bad-shaped address doesn't
			// poison the whole resolve — try the next source. This
			// is a common Happy-Eyeballs-at-the-CDN pattern where one
			// endpoint sometimes flips to IPv6 and the others stay on
			// IPv4.
			errs = append(errs, fmt.Errorf("%s: %w", url, verr))
			continue
		}
		return addr, nil
	}

	return netip.Addr{}, fmt.Errorf("externalip: all %d sources failed: %w",
		len(m.sources), errors.Join(errs...))
}

func (m *multiSourceResolver) fetchOne(ctx context.Context, url string) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return netip.Addr{}, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("status %d", resp.StatusCode)
	}

	// Cap the read at 128 bytes — a legitimate IP echo is at most ~45
	// (IPv6 textual max) plus a newline. Anything larger is either a
	// misconfigured proxy or a hostile response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return netip.Addr{}, err
	}

	raw := strings.TrimSpace(string(body))

	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("unparseable body %q: %w", raw, err)
	}

	// ifconfig.me sometimes wraps IPv4 in IPv4-in-IPv6 form; unwrap
	// so the downstream validator sees a true v4 address.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}

	return addr, nil
}
