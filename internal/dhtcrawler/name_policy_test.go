package dhtcrawler

import (
	"context"
	"net/netip"
	"testing"

	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/metainforequester"
	"github.com/stretchr/testify/require"
)

type policyCsam struct{ csamblocklist.Manager }

func (policyCsam) IsBlocked(protocol.ID) bool { return false }

type policyRequester struct {
	calls int
	info  metainfo.Info
}

func (r *policyRequester) Request(context.Context, protocol.ID, netip.AddrPort) (metainforequester.Response, error) {
	r.calls++
	return metainforequester.Response{Info: r.info}, nil
}

type policyBanning struct{ calls int }

func (b *policyBanning) Check(metainfo.Info) error { b.calls++; return nil }

func TestNamePolicyStopsAfterMinimumMetadataBeforeBanningAndBlocks(t *testing.T) {
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	for _, name := range []string{"Synthetic.电影.ENG.mkv", "Synthetic.Фильм.Dub.mkv", "FetishXXX.mkv"} {
		r := &policyRequester{info: metainfo.Info{Name: name}}
		b := &policyBanning{}
		c := crawler{namePolicy: p, metainfoRequester: r, banningChecker: b, csamBlocklist: policyCsam{}}
		_, err := c.doRequestMetaInfo(context.Background(), protocol.ID{1}, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")})
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
		require.Equal(t, 1, r.calls)
		require.Zero(t, b.calls)
		// blockingManager is deliberately nil: any destructive fallback panics.
	}
}
func TestOwnerHashDoesNotAcquireNameMetadata(t *testing.T) {
	h := protocol.ID{1}
	p, err := namepolicy.New(namepolicy.Config{Enabled: true, ExcludedInfoHashes: []string{h.String()}})
	require.NoError(t, err)
	c := crawler{namePolicy: p}
	_, err = c.doRequestMetaInfo(context.Background(), h, nil)
	require.ErrorIs(t, err, namepolicy.ErrExcluded)
}
func TestAllowedNameIgnoresForeignSupportPaths(t *testing.T) {
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	r := &policyRequester{info: metainfo.Info{Name: "Allowed.Café.ENG.mkv"}}
	b := &policyBanning{}
	c := crawler{namePolicy: p, metainfoRequester: r, banningChecker: b, csamBlocklist: policyCsam{}}
	_, err = c.doRequestMetaInfo(context.Background(), protocol.ID{1}, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")})
	require.NoError(t, err)
	require.Equal(t, 1, b.calls)
}
