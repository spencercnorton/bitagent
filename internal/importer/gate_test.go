package importer

import (
	"context"
	"errors"
	"testing"

	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCsam drops any hash whose first byte is in `block`.
type fakeCsam struct{ block map[byte]struct{} }

func (f fakeCsam) IsBlocked(h protocol.ID) bool { _, ok := f.block[h[0]]; return ok }
func (f fakeCsam) Filter(hs []protocol.ID) []protocol.ID {
	out := make([]protocol.ID, 0, len(hs))
	for _, h := range hs {
		if _, ok := f.block[h[0]]; !ok {
			out = append(out, h)
		}
	}
	return out
}
func (fakeCsam) Refresh(context.Context) error    { return nil }
func (fakeCsam) Snapshot() csamblocklist.Snapshot { return csamblocklist.Snapshot{} }
func (fakeCsam) Enabled() bool                    { return true }

// fakeBlocking drops any hash whose first byte is in `block`, or errors.
type fakeBlocking struct {
	block map[byte]struct{}
	err   error
}

func (f fakeBlocking) Filter(_ context.Context, hs []protocol.ID) ([]protocol.ID, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]protocol.ID, 0, len(hs))
	for _, h := range hs {
		if _, ok := f.block[h[0]]; !ok {
			out = append(out, h)
		}
	}
	return out, nil
}
func (fakeBlocking) Block(context.Context, []protocol.ID, bool) error { return nil }
func (fakeBlocking) Flush(context.Context) error                      { return nil }

func mkItems(firstBytes ...byte) []Item {
	items := make([]Item, len(firstBytes))
	for i, b := range firstBytes {
		var h protocol.ID
		h[0] = b
		items[i] = Item{InfoHash: h, Name: "item"}
	}
	return items
}

func firstBytes(items []Item) []byte {
	out := make([]byte, len(items))
	for i, it := range items {
		out[i] = it.InfoHash[0]
	}
	return out
}

func newGate(csam csamblocklist.Manager, block *fakeBlocking) *activeImport {
	imp := importer{csamBlocklist: csam, metrics: NewMetrics()}
	if block != nil {
		imp.blockingManager = block
	}
	return &activeImport{importer: imp, ctx: context.Background()}
}

// The /import gate must apply CSAM (first, unconditional) then blocking, and
// FAIL CLOSED when the blocking lookup errors — never importing unfiltered.
func TestGateItems(t *testing.T) {
	// CSAM(2) + blocking(3) each drop one; 1 and 4 survive, order preserved.
	out, err := newGate(fakeCsam{block: map[byte]struct{}{2: {}}},
		&fakeBlocking{block: map[byte]struct{}{3: {}}}).gateItems(mkItems(1, 2, 3, 4))
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 4}, firstBytes(out))

	// Blocking error → whole batch refused (fail closed), no items returned.
	out, err = newGate(fakeCsam{block: map[byte]struct{}{2: {}}},
		&fakeBlocking{err: errors.New("bloom flush failed")}).gateItems(mkItems(1, 2, 3))
	require.Error(t, err)
	assert.Nil(t, out)

	// Nothing dropped → original slice returned.
	in := mkItems(5, 6)
	out, err = newGate(fakeCsam{block: map[byte]struct{}{}},
		&fakeBlocking{block: map[byte]struct{}{}}).gateItems(in)
	require.NoError(t, err)
	assert.Equal(t, []byte{5, 6}, firstBytes(out))

	// Everything dropped → empty, no error (persistItems then no-ops).
	out, err = newGate(fakeCsam{block: map[byte]struct{}{7: {}}},
		&fakeBlocking{block: map[byte]struct{}{8: {}}}).gateItems(mkItems(7, 8))
	require.NoError(t, err)
	assert.Empty(t, out)

	// CSAM is applied even when blocking is nil (degraded wiring).
	out, err = newGate(fakeCsam{block: map[byte]struct{}{9: {}}}, nil).gateItems(mkItems(9, 10))
	require.NoError(t, err)
	assert.Equal(t, []byte{10}, firstBytes(out))

	// Both nil → pass-through, no panic (narrow test construction).
	g := &activeImport{importer: importer{metrics: NewMetrics()}, ctx: context.Background()}
	out, err = g.gateItems(mkItems(11, 12))
	require.NoError(t, err)
	assert.Len(t, out, 2)

	// Empty input short-circuits.
	out, err = newGate(fakeCsam{}, &fakeBlocking{}).gateItems(nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}
