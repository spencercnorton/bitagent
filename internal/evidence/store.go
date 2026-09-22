package evidence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

// ErrDuplicate is returned when an evidence row is a repeat of one
// already present (matches the source+kind+instance+object_id unique
// index). Callers treat this as success — webhook retries and poll
// overlaps are expected.
var ErrDuplicate = errors.New("evidence: duplicate event, absorbed")

// ErrNotUsable is returned when an evidence candidate cannot be
// persisted because it lacks both info_hash and download_id. Such
// records have no possible join path back to a torrent and would be
// pure noise.
var ErrNotUsable = errors.New("evidence: record has neither info_hash nor download_id")

// PostInsertHook is invoked AFTER a successful Insert commits, with
// the same Evidence the caller passed in. Hooks are best-effort
// projections (e.g. liveness): they must not block ingest, and any
// error must be swallowed by the hook itself. The store does not
// fan out to multiple hooks — a single hook is composed externally
// if more than one consumer needs the stream.
type PostInsertHook func(ctx context.Context, ev Evidence)

// Store persists evidence and maintains the canonical-label
// projection. All writes happen inside a single transaction so the
// evidence row and the canonical row move together or not at all.
type Store struct {
	pool lazy.Lazy[*pgxpool.Pool]
	hook PostInsertHook
}

// NewStore returns a Store bound to the given lazy pool.
func NewStore(pool lazy.Lazy[*pgxpool.Pool]) *Store {
	return &Store{pool: pool}
}

// SetPostInsertHook registers a hook fired after every successful
// Insert. Calling SetPostInsertHook with a nil hook removes the
// current hook. Not thread-safe with concurrent Insert calls; wire
// at startup before the workers begin.
func (s *Store) SetPostInsertHook(hook PostInsertHook) {
	s.hook = hook
}

// Insert persists an evidence record and, if the record has an
// info_hash, runs the canonical-label upsert. Returns ErrDuplicate
// (wrapped) when the row collides with the unique index — callers
// should check with errors.Is.
func (s *Store) Insert(ctx context.Context, e Evidence) error {
	if len(e.InfoHash) == 0 && e.DownloadID == "" {
		return ErrNotUsable
	}
	pool, err := s.pool.Get()
	if err != nil {
		return fmt.Errorf("evidence: acquire pool: %w", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("evidence: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insertEvidence = `
insert into label_evidence (
    source, source_kind, source_instance, source_object_id,
    download_id, info_hash, title, media_type, media_id,
    category, observed_at, strength, raw_payload
) values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
returning id`

	var evidenceID int64
	err = tx.QueryRow(ctx, insertEvidence,
		e.Source, e.Kind, e.SourceInstance, e.SourceObjectID,
		nullableString(e.DownloadID), nullableBytes(e.InfoHash),
		nullableString(e.Title), nullableMediaType(e.MediaType),
		nullableString(e.MediaID), nullableString(e.Category),
		e.ObservedAt, int16(e.Strength), nullableJSON(e.RawPayload),
	).Scan(&evidenceID)
	if err != nil {
		if isDuplicate(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("evidence: insert: %w", err)
	}

	// The canonical-label upsert only runs when the observation
	// carries an info_hash AND is a label-bearing kind. Evidence
	// without an info_hash stays in label_evidence as an
	// audit/backfill candidate; non-label kinds (e.g.
	// KindQBStateObservation, which carries liveness signal but no
	// media_type) must NEVER influence the canonical projection —
	// otherwise their strength constants would silently outrank
	// real category/import labels and overwrite a correct
	// media_type with a null one.
	if len(e.InfoHash) > 0 && isLabelBearing(e.Kind) {
		const upsertCanonical = `
insert into torrent_canonical_labels (
    info_hash, media_type, media_id, category, title,
    resolved_from, resolved_source, resolved_strength, resolved_at
) values ($1,$2,$3,$4,$5,$6,$7,$8,$9)
on conflict (info_hash) do update set
    media_type        = excluded.media_type,
    media_id          = excluded.media_id,
    category          = excluded.category,
    title             = excluded.title,
    resolved_from     = excluded.resolved_from,
    resolved_source   = excluded.resolved_source,
    resolved_strength = excluded.resolved_strength,
    resolved_at       = excluded.resolved_at
where
    torrent_canonical_labels.resolved_strength < excluded.resolved_strength
    or (torrent_canonical_labels.resolved_strength = excluded.resolved_strength
        and torrent_canonical_labels.resolved_at < excluded.resolved_at)`

		_, err = tx.Exec(ctx, upsertCanonical,
			e.InfoHash,
			nullableMediaType(e.MediaType),
			nullableString(e.MediaID),
			nullableString(e.Category),
			nullableString(e.Title),
			evidenceID,
			e.Source,
			int16(e.Strength),
			e.ObservedAt,
		)
		if err != nil {
			return fmt.Errorf("evidence: canonical upsert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.hook != nil {
		s.hook(ctx, e)
	}
	return nil
}

// CanonicalForInfoHash returns the current winning label for an
// infohash, or (nil, nil) if none exists. Callers treat absence as
// "no authoritative evidence yet, run the classifier".
func (s *Store) CanonicalForInfoHash(ctx context.Context, infoHash []byte) (*CanonicalLabel, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("evidence: acquire pool: %w", err)
	}
	const q = `
select info_hash, media_type, media_id, category, title,
       resolved_from, resolved_source, resolved_strength, resolved_at
from torrent_canonical_labels
where info_hash = $1`
	row := pool.QueryRow(ctx, q, infoHash)
	var (
		lbl              CanonicalLabel
		mediaType        *string
		mediaID          *string
		category         *string
		title            *string
		resolvedFrom     *int64
		resolvedStrength int16
	)
	if err := row.Scan(
		&lbl.InfoHash, &mediaType, &mediaID, &category, &title,
		&resolvedFrom, &lbl.ResolvedSource, &resolvedStrength, &lbl.ResolvedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("evidence: canonical lookup: %w", err)
	}
	if mediaType != nil {
		lbl.MediaType = MediaType(*mediaType)
	}
	if mediaID != nil {
		lbl.MediaID = *mediaID
	}
	if category != nil {
		lbl.Category = *category
	}
	if title != nil {
		lbl.Title = *title
	}
	if resolvedFrom != nil {
		lbl.ResolvedFrom = *resolvedFrom
	}
	lbl.ResolvedStrength = uint8(resolvedStrength)
	return &lbl, nil
}

// TitleLabel is one *arr identity label with the name of the torrent it was
// grabbed under — the raw material for title-level identity evidence.
type TitleLabel struct {
	InfoHash  []byte
	Name      string
	MediaType MediaType
	MediaID   string
}

// CanonicalTitleLabels returns every canonical label carrying a catalogue
// identity, joined to its torrent's name. One row per torrent an *arr has
// grabbed, so callers load it whole.
func (s *Store) CanonicalTitleLabels(ctx context.Context) ([]TitleLabel, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("evidence: acquire pool: %w", err)
	}
	const q = `
select tcl.info_hash, t.name, coalesce(tcl.media_type, ''), tcl.media_id
from torrent_canonical_labels tcl
join torrents t on t.info_hash = tcl.info_hash
where tcl.media_id is not null and tcl.media_id <> ''`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("evidence: title labels: %w", err)
	}
	defer rows.Close()
	var out []TitleLabel
	for rows.Next() {
		var (
			l  TitleLabel
			mt string
		)
		if err := rows.Scan(&l.InfoHash, &l.Name, &mt, &l.MediaID); err != nil {
			return nil, fmt.Errorf("evidence: title labels scan: %w", err)
		}
		l.MediaType = MediaType(mt)
		out = append(out, l)
	}

	return out, rows.Err()
}

// ListRecent returns up to limit evidence rows starting at offset,
// ordered by observed_at desc, plus the table's total row count.
// The total is computed via a single COUNT(*) — bounded by the
// label_evidence_observed_at_idx index for the page query and a
// sequential pass for the count. Callers should bound limit before
// passing in.
func (s *Store) ListRecent(ctx context.Context, limit, offset int) ([]Evidence, int, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, 0, fmt.Errorf("evidence: acquire pool: %w", err)
	}

	var total int
	if err := pool.QueryRow(ctx, `select count(*) from label_evidence`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("evidence: count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	const q = `
select id, source, source_kind, source_instance, source_object_id,
       coalesce(download_id, ''), info_hash, coalesce(title, ''),
       coalesce(media_type, ''), coalesce(media_id, ''),
       coalesce(category, ''), observed_at, strength
from label_evidence
order by observed_at desc
limit $1 offset $2`

	rows, err := pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("evidence: list: %w", err)
	}
	defer rows.Close()

	out := make([]Evidence, 0, limit)
	for rows.Next() {
		var (
			e         Evidence
			mediaType string
			strength  int16
		)
		if err := rows.Scan(
			&e.ID, &e.Source, &e.Kind, &e.SourceInstance, &e.SourceObjectID,
			&e.DownloadID, &e.InfoHash, &e.Title,
			&mediaType, &e.MediaID, &e.Category,
			&e.ObservedAt, &strength,
		); err != nil {
			return nil, 0, fmt.Errorf("evidence: scan: %w", err)
		}
		e.MediaType = MediaType(mediaType)
		e.Strength = uint8(strength)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("evidence: rows: %w", err)
	}
	return out, total, nil
}

// IsPrivateInfoHash reports whether any evidence row for the
// infohash was produced by a private tracker — i.e. a qBittorrent
// category of 'private' or 'bitgrab', or any source whose strength
// matches the private-category tier. It is used by downstream
// consumers (the LLM classifier stage, primarily) as a hard gate on
// external API calls: private-tracker content must never leave this
// host.
//
// Returns (false, nil) when no evidence exists — a new torrent that
// the ingestor has not seen yet is not private by default. Errors
// are never swallowed; callers must fail closed on error.
func (s *Store) IsPrivateInfoHash(ctx context.Context, infoHash []byte) (bool, error) {
	if len(infoHash) == 0 {
		return false, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return false, fmt.Errorf("evidence: acquire pool: %w", err)
	}
	// Only qB-category evidence is considered private here.
	// Strength is not a proxy for privacy: *arr grabs have higher
	// strength but are not private. Adding a new private source
	// later should extend this query, not reuse the strength axis.
	const q = `
select exists (
  select 1 from label_evidence
  where info_hash = $1
    and source = 'qbittorrent'
    and lower(category) in ('private', 'bitgrab')
)`
	var isPrivate bool
	if err := pool.QueryRow(ctx, q, infoHash).Scan(&isPrivate); err != nil {
		return false, fmt.Errorf("evidence: is_private lookup: %w", err)
	}
	return isPrivate, nil
}

// isLabelBearing reports whether the evidence Kind contributes to
// the canonical-label projection. The set is an explicit allowlist
// rather than a denylist so adding a new non-label kind in the
// future cannot accidentally start corrupting torrent_canonical_labels.
func isLabelBearing(kind Kind) bool {
	switch kind {
	case KindWebhookImport, KindWebhookGrab, KindPollHistory, KindPollCategories:
		return true
	}
	return false
}

func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	return false
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

func nullableMediaType(v MediaType) any {
	if v == "" {
		return nil
	}
	return string(v)
}

func nullableJSON(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}
