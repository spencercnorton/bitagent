package peerrep

import "time"

const (
	defaultMaxEntries = 100_000
	defaultTTL        = 24 * time.Hour
)

// Config controls the peer-reputation cache. Registered as the
// `peer_rep` configfx section; env prefix `PEER_REP_*`.
//
// Defaults aim at "shadow mode is safe to leave on permanently" —
// every threshold is conservative enough that even an aggressive
// false-positive in the dial-timeout class only delays a peer by
// minutes, not hours.
type Config struct {
	// Enabled turns the cache on. When false, every Decide() returns
	// an immediate Allow with no state mutation. Default false so a
	// new build is a pure no-op until the operator opts in.
	Enabled bool `yaml:"enabled"`

	// Enforce flips between shadow and live modes. When false, every
	// Decide() returns Allow but the underlying state mutates and
	// metrics emit — letting operators measure "what would skip" via
	// `bitagent_peer_rep_would_skip_total` without changing fetcher
	// behaviour. When true, Decide() returns Skip when the peer is
	// in active backoff and the fetcher must respect that.
	Enforce bool `yaml:"enforce"`

	// MaxEntries caps the LRU size. A typical crawler sees ~10k
	// distinct peers per minute; 100k entries holds ~10 minutes of
	// peer history without bounding memory unreasonably (~100 MB at
	// the upper bound — each entry is small).
	MaxEntries int `yaml:"max_entries"`

	// TTL is the upper bound on entry lifetime. Even a peer with
	// repeated failures gets re-tried after this expires. Bounds
	// the chance of permanent blacklisting from a bad data
	// persistence event.
	TTL time.Duration `yaml:"ttl"`

	// Per-error-class backoff schedule. After N consecutive failures
	// of the given class, the peer is suppressed for the duration
	// indexed at min(N-1, len(schedule)-1). A success clears the
	// counter back to zero.
	//
	// The defaults follow the GPT-5.5-pro review's heuristic:
	//
	//   dial timeout       — short (peer might be temporarily NAT'd)
	//   connection refused — medium (firewall actively rejecting)
	//   tcp/network error  — medium
	//   peer closed        — short (TCP closed mid-flight, often flaky)
	//   handshake failure  — longer (protocol-level wrong)
	//   hash mismatch      — long (peer responded with wrong info_hash —
	//                        structural protocol bug or attack)
	//   no ut_metadata     — long (peer doesn't speak BEP-9 for
	//                        this swarm — usually structural)
	//   metadata timeout   — short (handshake worked, fetch slow)
	//   krpc error         — long (peer rejected the query method)
	DialTimeoutBackoff       []time.Duration `yaml:"dial_timeout_backoff"`
	ConnectionRefusedBackoff []time.Duration `yaml:"connection_refused_backoff"`
	NetworkErrorBackoff      []time.Duration `yaml:"network_error_backoff"`
	PeerClosedBackoff        []time.Duration `yaml:"peer_closed_backoff"`
	HandshakeFailBackoff     []time.Duration `yaml:"handshake_fail_backoff"`
	HashMismatchBackoff      []time.Duration `yaml:"hash_mismatch_backoff"`
	NoUtMetadataBackoff      []time.Duration `yaml:"no_ut_metadata_backoff"`
	MetadataTimeoutBackoff   []time.Duration `yaml:"metadata_timeout_backoff"`
	KRPCErrorBackoff         []time.Duration `yaml:"krpc_error_backoff"`
}

// NewDefaultConfig returns a config that's safe to ship enabled in
// shadow mode (no behaviour change from a clean container).
func NewDefaultConfig() Config {
	return Config{
		Enabled:    false, // explicit opt-in
		Enforce:    false, // shadow mode by default
		MaxEntries: defaultMaxEntries,
		TTL:        defaultTTL,

		// Conservative ramps. First failure: short suppression.
		// Repeated failures: exponential-ish backoff toward the TTL.
		DialTimeoutBackoff: []time.Duration{
			10 * time.Minute,
			30 * time.Minute,
			2 * time.Hour,
		},
		ConnectionRefusedBackoff: []time.Duration{
			// Firewall actively rejecting — slightly longer than
			// dial timeout because the peer's policy is "no" rather
			// than "maybe later."
			30 * time.Minute,
			2 * time.Hour,
			6 * time.Hour,
		},
		NetworkErrorBackoff: []time.Duration{
			10 * time.Minute,
			30 * time.Minute,
			2 * time.Hour,
		},
		PeerClosedBackoff: []time.Duration{
			// Mid-flight TCP close — often flaky residential peers.
			// Shorter than ConnectionRefused: peer was willing to
			// connect once, may again.
			15 * time.Minute,
			1 * time.Hour,
			4 * time.Hour,
		},
		HandshakeFailBackoff: []time.Duration{
			1 * time.Hour,
			6 * time.Hour,
			24 * time.Hour,
		},
		HashMismatchBackoff: []time.Duration{
			// Peer responded with wrong info_hash — protocol error
			// or attack. Long suppression; this peer is broken or
			// hostile for this swarm.
			6 * time.Hour,
			24 * time.Hour,
		},
		NoUtMetadataBackoff: []time.Duration{
			6 * time.Hour,
			24 * time.Hour,
		},
		MetadataTimeoutBackoff: []time.Duration{
			15 * time.Minute,
			1 * time.Hour,
			6 * time.Hour,
		},
		KRPCErrorBackoff: []time.Duration{
			1 * time.Hour,
			6 * time.Hour,
			24 * time.Hour,
		},
	}
}

// ErrorClass enumerates the failure modes the cache distinguishes.
// New classes go here, not in the call site — the SQL of the cache
// is "if you don't tell me what kind of failure it was, I treat it
// as Network" (see Decide.Record).
type ErrorClass int

const (
	// ClassUnknown maps to NetworkError's schedule. Catch-all for
	// errors the caller couldn't classify.
	ClassUnknown ErrorClass = iota

	// ClassDialTimeout — TCP SYN never got a response. Peer behind
	// NAT, offline, or firewalled. Recovers fast (NAT churn).
	ClassDialTimeout

	// ClassNetworkError — TCP refused, RST, EOF, route unreachable.
	// Peer present but uncooperative at the transport layer.
	ClassNetworkError

	// ClassHandshakeFail — TCP connected but the BitTorrent
	// handshake or BEP-10 extension handshake failed (peer protocol
	// mismatch, bad info hash response, etc.). Less likely to be
	// transient.
	ClassHandshakeFail

	// ClassNoUtMetadata — handshake succeeded but the peer did not
	// advertise the `ut_metadata` extension. Fundamental structural
	// failure, longest backoff.
	ClassNoUtMetadata

	// ClassMetadataTimeout — `ut_metadata` request timed out after
	// handshake. Might just be slow; medium backoff.
	ClassMetadataTimeout

	// ClassKRPCError — peer responded with an explicit KRPC error
	// (e.g. method-unknown for older peer software). Long backoff.
	ClassKRPCError

	// ClassConnectionRefused — TCP RST on connect attempt. Distinct
	// from DialTimeout: peer is REACHABLE but actively rejecting
	// (firewall policy / port closed). Recovers slower than
	// DialTimeout because the peer's stance is policy, not
	// transient state.
	ClassConnectionRefused

	// ClassPeerClosed — TCP closed mid-handshake or mid-fetch
	// (unexpected EOF, connection reset by peer, broken pipe).
	// Often flaky residential or NAT-traversal-failing peers.
	// Distinct from NetworkError because the peer was willing to
	// connect at least once.
	ClassPeerClosed

	// ClassHashMismatch — handshake completed but the peer responded
	// with the wrong info_hash for the queried swarm. Either
	// protocol error in the peer's client OR active mis-routing /
	// attack. Long backoff; this peer is unreliable for this swarm.
	ClassHashMismatch
)

// String returns the metric-label-safe name for this class. Pinned
// here so adding a new class is one switch update.
func (c ErrorClass) String() string {
	switch c {
	case ClassDialTimeout:
		return "dial_timeout"
	case ClassNetworkError:
		return "network_error"
	case ClassHandshakeFail:
		return "handshake_fail"
	case ClassNoUtMetadata:
		return "no_ut_metadata"
	case ClassMetadataTimeout:
		return "metadata_timeout"
	case ClassKRPCError:
		return "krpc_error"
	case ClassConnectionRefused:
		return "connection_refused"
	case ClassPeerClosed:
		return "peer_closed"
	case ClassHashMismatch:
		return "hash_mismatch"
	default:
		return "unknown"
	}
}
