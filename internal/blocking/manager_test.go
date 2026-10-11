package blocking

import (
	"context"
	"errors"
	"testing"

	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

func TestRecoveryOffRejectsBlockBeforeBufferOrDatabaseAccess(t *testing.T) {
	hash := protocol.ID{1}
	m := &manager{buffer: map[protocol.ID]struct{}{}}
	for _, flush := range []bool{false, true} {
		if err := m.Block(context.Background(), []protocol.ID{hash}, "blocking", "synthetic", flush); !errors.Is(err, cataloguerecovery.ErrDisabled) {
			t.Fatalf("Block error = %v, want recovery disabled", err)
		}
		if len(m.buffer) != 0 || m.filter != nil {
			t.Fatal("held removal changed buffer or bloom")
		}
	}
}

func TestRecoveryOffRejectsLegacyBufferedFlushBeforeDatabaseAccess(t *testing.T) {
	hash := protocol.ID{1}
	m := &manager{buffer: map[protocol.ID]struct{}{hash: {}}}
	if err := m.Flush(context.Background()); !errors.Is(err, cataloguerecovery.ErrDisabled) {
		t.Fatalf("Flush error = %v, want recovery disabled", err)
	}
	if len(m.buffer) != 1 || m.filter != nil {
		t.Fatal("held flush changed buffer or bloom")
	}
}
