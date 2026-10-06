package llmwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
)

type Kind string

const (
	Type     Kind = "classifier_type"
	Language Kind = "contentfilter"
	Matcher  Kind = "matcher"
)

var (
	ErrDeferred   = errors.New("optional model work deferred")
	ErrHeld       = errors.New("optional model work requires explicit reconciliation")
	ErrObsolete   = errors.New("optional model work source or policy changed")
	ErrLease      = errors.New("optional model work lease is not owned")
	ErrReplayOnly = errors.New("completed optional model work may replay only")
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
	return Digest([]any{t.InfoHash.Bytes(), t.Name, t.Size, t.Private, t.FilesStatus, t.Extension, t.FilesCount, []any{t.Hint.ContentType, t.Hint.ContentSource, t.Hint.ContentID, t.Hint.Title, t.Hint.ReleaseYear, t.Hint.Languages, t.Hint.Episodes, t.Hint.VideoResolution, t.Hint.VideoSource, t.Hint.VideoCodec, t.Hint.Video3D, t.Hint.VideoModifier, t.Hint.ReleaseGroup}, fileEvidence})
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
type replayOnlyKey struct{}

func WithReplayOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, replayOnlyKey{}, true)
}
func ReplayOnly(ctx context.Context) bool { v, _ := ctx.Value(replayOnlyKey{}).(bool); return v }

type Execution struct {
	Store        *Store
	Lease        Lease
	mu           sync.Mutex
	recheck      func(context.Context) error
	txRecheck    func(context.Context, pgx.Tx) error
	lastDeferred *DeferredError
}

func WithExecution(ctx context.Context, store *Store, lease Lease) context.Context {
	return context.WithValue(ctx, executionKey{}, &Execution{Store: store, Lease: lease})
}

func CaseFence(ctx context.Context) *llmcapture.CaseFence {
	e := ExecutionFrom(ctx)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	check := e.txRecheck
	e.mu.Unlock()
	return &llmcapture.CaseFence{TaskKey: e.Lease.Task.Key, LeaseOwner: e.Lease.Owner, LeaseGeneration: e.Lease.Generation, SourceDigest: e.Lease.Task.SourceDigest, PolicyDigest: e.Lease.Task.PolicyDigest, SourceRecheck: check}
}
func SetSourceRecheck(ctx context.Context, check func(context.Context) error) error {
	e := ExecutionFrom(ctx)
	if e == nil || check == nil {
		return ErrLease
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recheck = check
	return nil
}

// SetTransactionalSourceRecheck binds an exact current-source comparator to
// owned dispatch transactions. The callback retains its source locks until the
// controller commits and cannot invoke a provider or change the catalogue.
func SetTransactionalSourceRecheck(ctx context.Context, check func(context.Context, pgx.Tx) error) error {
	e := ExecutionFrom(ctx)
	if e == nil || check == nil {
		return ErrLease
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.txRecheck = check
	return nil
}
func BeforeDispatch(ctx context.Context) error {
	e := ExecutionFrom(ctx)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	check := e.recheck
	e.mu.Unlock()
	if check == nil {
		return ErrLease
	}
	return check(ctx)
}
func RecordDeferral(ctx context.Context, reason string, retryAt time.Time) error {
	d := DeferredError{Reason: reason, RetryAfter: retryAt}
	if e := ExecutionFrom(ctx); e != nil {
		e.mu.Lock()
		e.lastDeferred = &d
		e.mu.Unlock()
	}
	return d
}
func LastDeferral(ctx context.Context) *DeferredError {
	e := ExecutionFrom(ctx)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastDeferred == nil {
		return nil
	}
	d := *e.lastDeferred
	return &d
}
func RecordProgress(ctx context.Context) error {
	e := ExecutionFrom(ctx)
	if e == nil || e.Store == nil || e.Lease.Task.Kind != Matcher {
		return nil
	}
	return e.Store.MarkProgress(ctx, e.Lease)
}

func ExecutionFrom(ctx context.Context) *Execution {
	e, _ := ctx.Value(executionKey{}).(*Execution)
	return e
}

func (e *Execution) Matches(kind Kind, infoHash, source, policy []byte) bool {
	return e != nil && e.Lease.Task.Kind == kind && bytes.Equal(e.Lease.Task.InfoHash, infoHash) &&
		bytes.Equal(e.Lease.Task.SourceDigest, source) && bytes.Equal(e.Lease.Task.PolicyDigest, policy)
}
