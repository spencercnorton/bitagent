package bloom

import (
	"encoding/binary"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/spencercnorton/bitagent/internal/protocol/dht"
)

type Filter = bloom.BloomFilter

const (
	size     = 32
	byteSize = size * 8
	M        = byteSize * 8
	K        = 2
)

var NewWithEstimates = bloom.NewWithEstimates

func FromScrape(f dht.ScrapeBloomFilter) Filter {
	return *bloom.FromWithM(convertBytes(f), M, K)
}

func convertBytes(b [byteSize]byte) []uint64 {
	ret := make([]uint64, size)

	for i := range size {
		startPos := i * 8
		ret[i] = binary.BigEndian.Uint64(b[startPos : startPos+8])
	}

	return ret
}
