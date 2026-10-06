package llmwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/spencercnorton/bitagent/internal/model"
)

type Kind string

const (
	Type     Kind = "classifier_type"
	Language Kind = "contentfilter"
	Matcher  Kind = "matcher"
)

var (
	ErrDeferred = errors.New("optional model work deferred")
	ErrHeld     = errors.New("optional model work requires explicit reconciliation")
	ErrObsolete = errors.New("optional model work source or policy changed")
	ErrLease    = errors.New("optional model work lease is not owned")
)

type Draft struct {
	Kind         Kind
	InfoHash     []byte
	SourceDigest []byte
	PolicyDigest []byte
	InputDigest  []byte
	FamilyDigest []byte
	Payload      json.RawMessage
	Priority     int
	DailyLimit   int
	MonthlyLimit int
}

func (d Draft) Key() ([]byte, error) {
	if d.Kind != Type && d.Kind != Language && d.Kind != Matcher {
		return nil, fmt.Errorf("unknown optional model task")
	}
	if len(d.InfoHash) != 20 || len(d.SourceDigest) != 32 || len(d.PolicyDigest) != 32 ||
		len(d.InputDigest) != 32 || len(d.FamilyDigest) != 32 ||
		len(d.Payload) == 0 || len(d.Payload) > 64<<10 || !json.Valid(d.Payload) ||
		d.Priority < 0 || d.Priority > 100 || d.DailyLimit < 0 || d.MonthlyLimit < 0 {
		return nil, fmt.Errorf("malformed bounded optional model task")
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(d.Payload, &payload) != nil || payload == nil {
		return nil, fmt.Errorf("task payload must be an object")
	}
	// JSON field boundaries and fixed digests avoid ambiguous concatenations.
	body, _ := json.Marshal(struct {
		Kind                            Kind
		InfoHash, Source, Policy, Input []byte
	}{d.Kind, d.InfoHash, d.SourceDigest, d.PolicyDigest, d.InputDigest})
	sum := sha256.Sum256(body)
	return append([]byte(nil), sum[:]...), nil
}

func Digest(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(body)
	return append([]byte(nil), sum[:]...)
}

// SourceDigest excludes torrent observation times and swarm counters. Refreshes
// cannot change model inputs, while file contents, hints and native privacy can.
func SourceDigest(t model.Torrent) []byte {
	files := append([]model.TorrentFile(nil), t.Files...)
	sort.Slice(files, func(i, j int) bool {
		if files[i].Index != files[j].Index {
			return files[i].Index < files[j].Index
		}
		return files[i].Path < files[j].Path
	})
	fileEvidence := make([]any, 0, len(files))
	for _, f := range files {
		fileEvidence = append(fileEvidence, []any{f.Index, f.Path, f.Size, f.Extension})
	}
	return Digest([]any{t.InfoHash.Bytes(), t.Name, t.Size, t.Private, t.FilesStatus, t.Extension, t.FilesCount, t.Hint, fileEvidence})
}

type Task struct {
	Draft
	Key                              []byte
	State, Reason                    string
	CreatedAt, RetryAfter, ExpiresAt time.Time
}

type Lease struct {
	Task       Task
	Owner      string
	Generation int64
	Until      time.Time
}

type executionKey struct{}
type Execution struct {
	Store *Store
	Lease Lease
}

func WithExecution(ctx context.Context, store *Store, lease Lease) context.Context {
	return context.WithValue(ctx, executionKey{}, &Execution{Store: store, Lease: lease})
}

func ExecutionFrom(ctx context.Context) *Execution {
	e, _ := ctx.Value(executionKey{}).(*Execution)
	return e
}

func (e *Execution) Matches(kind Kind, infoHash, source, policy []byte) bool {
	return e != nil && e.Lease.Task.Kind == kind && bytes.Equal(e.Lease.Task.InfoHash, infoHash) &&
		bytes.Equal(e.Lease.Task.SourceDigest, source) && bytes.Equal(e.Lease.Task.PolicyDigest, policy)
}
