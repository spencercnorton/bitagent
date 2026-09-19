package metainforequester

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/anacrolix/torrent/peer_protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadAllPiecesAcceptsLegalPieceOrders(t *testing.T) {
	tests := []struct {
		name  string
		size  int
		order []int
	}{
		{name: "short single piece", size: 37, order: []int{0}},
		{name: "full single piece", size: metadataPieceSize, order: []int{0}},
		{name: "out of order exact multiple", size: 2 * metadataPieceSize, order: []int{1, 0}},
		{
			name:  "out of order partial final piece",
			size:  2*metadataPieceSize + 31,
			order: []int{2, 0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := patternedBytes(tt.size)
			wire := metadataWire(metadata, tt.order...)

			got, err := readAllPieces(bytes.NewReader(wire), uint(len(metadata)))
			require.NoError(t, err)
			assert.Equal(t, metadata, got)
		})
	}
}

func TestReadAllPiecesDuplicateDoesNotAdvanceCompletion(t *testing.T) {
	metadata := patternedBytes(2 * metadataPieceSize)

	t.Run("duplicate followed by missing piece fails", func(t *testing.T) {
		wire := metadataWire(metadata, 0, 0)

		got, err := readAllPieces(bytes.NewReader(wire), uint(len(metadata)))
		require.ErrorIs(t, err, io.EOF)
		assert.Nil(t, got)
	})

	t.Run("duplicate followed by remaining piece succeeds", func(t *testing.T) {
		wire := metadataWire(metadata, 0, 0, 1)

		got, err := readAllPieces(bytes.NewReader(wire), uint(len(metadata)))
		require.NoError(t, err)
		assert.Equal(t, metadata, got)
	})
}

func TestReadAllPiecesRejectsConflictingDuplicate(t *testing.T) {
	metadata := patternedBytes(2 * metadataPieceSize)
	firstPiece := metadata[:metadataPieceSize]
	conflictingPiece := append([]byte(nil), firstPiece...)
	conflictingPiece[0] ^= 0xff

	wire := bytes.Join([][]byte{
		dataFrame(0, len(metadata), firstPiece),
		dataFrame(0, len(metadata), conflictingPiece),
	}, nil)

	_, err := readAllPieces(bytes.NewReader(wire), uint(len(metadata)))
	require.ErrorContains(t, err, "conflicts with an earlier copy")
}

func TestReadAllPiecesRejectsInvalidPieceIndex(t *testing.T) {
	metadataSize := metadataPieceSize + 7
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name   string
		header string
	}{
		{
			name:   "negative",
			header: dataHeader(-1, metadataSize),
		},
		{
			name:   "equal to piece count",
			header: dataHeader(2, metadataSize),
		},
		{
			name:   "maximum integer",
			header: dataHeader(maxInt, metadataSize),
		},
		{
			name: "missing piece field",
			header: fmt.Sprintf(
				"d8:msg_typei1e10:total_sizei%dee",
				metadataSize,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire := utMetadataFrame(tt.header, nil)

			_, err := readAllPieces(bytes.NewReader(wire), uint(metadataSize))
			require.ErrorContains(t, err, "piece index")
		})
	}

	t.Run("integer overflow is a decode error", func(t *testing.T) {
		header := fmt.Sprintf(
			"d8:msg_typei1e5:piecei999999999999999999999999e10:total_sizei%dee",
			metadataSize,
		)

		_, err := readAllPieces(bytes.NewReader(utMetadataFrame(header, nil)), uint(metadataSize))
		require.ErrorContains(t, err, "decode ut_metadata header")
	})
}

func TestReadAllPiecesRequiresExactPieceLength(t *testing.T) {
	metadataSize := metadataPieceSize + 7
	tests := []struct {
		name        string
		piece       int
		payloadSize int
		expected    int
	}{
		{name: "non-final short", piece: 0, payloadSize: metadataPieceSize - 1, expected: metadataPieceSize},
		{name: "non-final long", piece: 0, payloadSize: metadataPieceSize + 1, expected: metadataPieceSize},
		{name: "final short", piece: 1, payloadSize: 6, expected: 7},
		{name: "final long", piece: 1, payloadSize: 8, expected: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire := dataFrame(tt.piece, metadataSize, patternedBytes(tt.payloadSize))

			_, err := readAllPieces(bytes.NewReader(wire), uint(metadataSize))
			require.ErrorContains(t, err, fmt.Sprintf("expected %d", tt.expected))
		})
	}
}

func TestReadAllPiecesRejectsInconsistentMessages(t *testing.T) {
	const metadataSize = 11
	tests := []struct {
		name      string
		header    string
		payload   []byte
		errString string
	}{
		{
			name:      "missing msg type",
			header:    "d5:piecei0ee",
			errString: "missing msg_type",
		},
		{
			name:      "missing total size",
			header:    "d8:msg_typei1e5:piecei0ee",
			payload:   patternedBytes(metadataSize),
			errString: "total_size -1",
		},
		{
			name:      "mismatched total size",
			header:    dataHeader(0, metadataSize+1),
			payload:   patternedBytes(metadataSize),
			errString: "does not match handshake size",
		},
		{
			name:      "negative total size",
			header:    dataHeader(0, -1),
			payload:   patternedBytes(metadataSize),
			errString: "total_size -1",
		},
		{
			name:      "request with trailing payload",
			header:    messageHeader(0, 0),
			payload:   []byte("unexpected"),
			errString: "request contains trailing payload",
		},
		{
			name:      "reject with trailing payload",
			header:    messageHeader(2, 0),
			payload:   []byte("unexpected"),
			errString: "reject contains trailing payload",
		},
		{
			name:      "unknown message type",
			header:    messageHeader(99, 0),
			errString: "unknown ut_metadata msg_type 99",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readAllPieces(
				bytes.NewReader(utMetadataFrame(tt.header, tt.payload)),
				metadataSize,
			)
			require.ErrorContains(t, err, tt.errString)
		})
	}
}

func TestReadAllPiecesRejectsInvalidMetadataSize(t *testing.T) {
	for _, size := range []uint{0, maxMetadataSize} {
		_, err := readAllPieces(bytes.NewReader(nil), size)
		require.ErrorContains(t, err, "invalid metadata size")
	}
}

func TestReadUmMessageSkipsUnrelatedAndShortFrames(t *testing.T) {
	want := append(
		[]byte{byte(peer_protocol.Extended), 1},
		[]byte(messageHeader(2, 0))...,
	)
	wire := bytes.Join([][]byte{
		frame(nil),
		frame([]byte{byte(peer_protocol.Choke)}),
		frame([]byte{byte(peer_protocol.Extended)}),
		frame([]byte{byte(peer_protocol.Extended), 2, 'x'}),
		frame(want),
	}, nil)

	got, err := readUmMessage(bytes.NewReader(wire))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReadMessageRejectsInvalidFraming(t *testing.T) {
	t.Run("oversized declared length", func(t *testing.T) {
		wire := make([]byte, 4)
		binary.BigEndian.PutUint32(wire, maxMetadataSize+1)

		_, err := readMessage(bytes.NewReader(wire))
		require.ErrorContains(t, err, "longer than max allowed")
	})

	t.Run("truncated payload", func(t *testing.T) {
		wire := append(framePrefix(3), []byte{1, 2}...)

		_, err := readMessage(bytes.NewReader(wire))
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})
}

func FuzzReadAllPiecesRoundTrip(f *testing.F) {
	f.Add([]byte("short metadata"), []byte{0, 0})
	f.Add(patternedBytes(metadataPieceSize+17), []byte{1, 0, 1})

	f.Fuzz(func(t *testing.T, metadata, orderBytes []byte) {
		if len(metadata) == 0 || len(metadata) > 2*metadataPieceSize+31 || len(orderBytes) > 8 {
			return
		}

		nPieces := metadataPieceCount(uint(len(metadata)))
		order := make([]int, 0, len(orderBytes)+nPieces)
		for _, value := range orderBytes {
			order = append(order, int(value)%nPieces)
		}
		for piece := range nPieces {
			order = append(order, piece)
		}

		got, err := readAllPieces(
			bytes.NewReader(metadataWire(metadata, order...)),
			uint(len(metadata)),
		)
		require.NoError(t, err)
		assert.Equal(t, metadata, got)
	})
}

func FuzzReadAllPiecesMalformedWireDoesNotPanic(f *testing.F) {
	f.Add(uint16(1), []byte{})
	f.Add(uint16(metadataPieceSize+1), utMetadataFrame(dataHeader(-1, metadataPieceSize+1), nil))
	f.Add(uint16(17), frame([]byte{byte(peer_protocol.Extended)}))

	f.Fuzz(func(t *testing.T, rawSize uint16, wire []byte) {
		if len(wire) > 4*metadataPieceSize || hasLargeDeclaredFrame(wire) {
			return
		}

		metadataSize := uint(rawSize)%(2*metadataPieceSize) + 1
		_, _ = readAllPieces(bytes.NewReader(wire), metadataSize)
	})
}

func patternedBytes(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}

	return data
}

func metadataWire(metadata []byte, order ...int) []byte {
	var wire bytes.Buffer
	for _, piece := range order {
		start := piece * metadataPieceSize
		end := min(start+metadataPieceSize, len(metadata))
		wire.Write(dataFrame(piece, len(metadata), metadata[start:end]))
	}

	return wire.Bytes()
}

func dataFrame(piece, totalSize int, payload []byte) []byte {
	return utMetadataFrame(dataHeader(piece, totalSize), payload)
}

func dataHeader(piece, totalSize int) string {
	return fmt.Sprintf(
		"d8:msg_typei1e5:piecei%de10:total_sizei%dee",
		piece,
		totalSize,
	)
}

func messageHeader(msgType, piece int) string {
	return fmt.Sprintf("d8:msg_typei%de5:piecei%dee", msgType, piece)
}

func utMetadataFrame(header string, payload []byte) []byte {
	body := make([]byte, 0, 2+len(header)+len(payload))
	body = append(body, byte(peer_protocol.Extended), 1)
	body = append(body, header...)
	body = append(body, payload...)

	return frame(body)
}

func frame(payload []byte) []byte {
	return append(framePrefix(len(payload)), payload...)
}

func framePrefix(length int) []byte {
	prefix := make([]byte, 4)
	binary.BigEndian.PutUint32(prefix, uint32(length))

	return prefix
}

func hasLargeDeclaredFrame(wire []byte) bool {
	for len(wire) >= 4 {
		length := int(binary.BigEndian.Uint32(wire[:4]))
		if length > 2*metadataPieceSize {
			return true
		}

		wire = wire[4:]
		if length > len(wire) {
			return false
		}

		wire = wire[length:]
	}

	return false
}
