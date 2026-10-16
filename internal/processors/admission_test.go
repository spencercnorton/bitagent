package processors

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPublishedAdmissionCohorts(t *testing.T) {
	// Golden buckets were produced independently with Python hashlib, using
	// the published byte recipe, not this implementation's helper.
	var fixture struct {
		Algorithm string `json:"algorithm"`
		Vectors   []struct {
			Hash   string `json:"info_hash"`
			Seed   string `json:"seed"`
			Bucket uint64 `json:"bucket"`
		} `json:"vectors"`
	}
	b, err := os.ReadFile("../../docs/processors/fixtures/admission-cohorts.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &fixture))
	require.Equal(t, v1.AdmissionAlgorithm, fixture.Algorithm)
	for _, vector := range fixture.Vectors {
		h, err := hex.DecodeString(vector.Hash)
		require.NoError(t, err)
		bucket, err := admissionBucket(h, vector.Seed)
		require.NoError(t, err)
		require.Equal(t, vector.Bucket, bucket)
		p := &v1.AdmissionPolicy{Contract: v1.AdmissionContract, Algorithm: fixture.Algorithm, Seed: vector.Seed}
		for _, points := range []int{0, 100, 1000, 10000, int(bucket), int(bucket) + 1} {
			p.BasisPoints = points
			require.Equal(t, bucket < uint64(points), admittedByPolicy(h, p))
		}
	}
	require.False(t, admittedByPolicy(make([]byte, 20), nil))
	_, err = admissionBucket(make([]byte, 19), fixture.Vectors[0].Seed)
	require.Error(t, err)
}
