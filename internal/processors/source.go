package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	llmwork "github.com/spencercnorton/bitagent/internal/legacyresults"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"reflect"
	"strings"
)

// A single SQL snapshot binds metadata, file evidence, catalogue facts and
// independent privacy/authority. Volatile counts and timestamps are excluded.
const sourceSQL = `SELECT jsonb_build_object(
 'name',t.name,'size',t.size,'private',t.private,'files_status',t.files_status,
 'file_count',t.files_count,'extension',t.extension,
 'files',COALESCE((SELECT jsonb_agg(x ORDER BY x->>'path',x->>'index') FROM
  (SELECT jsonb_build_object('path',path,'size',size,'index',"index",'extension',extension) x
   FROM torrent_files WHERE info_hash=t.info_hash ORDER BY "index",path LIMIT 257) f),'[]'),
 'receipt',(SELECT to_jsonb(r)-'recorded_at' FROM processor_core_receipts r WHERE r.info_hash=t.info_hash),
 'reprocess',(SELECT request_id FROM processor_reprocess_requests r WHERE r.info_hash=t.info_hash AND r.state IN('ready','queued','settled','core_only') ORDER BY created_at DESC,request_id DESC LIMIT 1),
 'hint',(SELECT to_jsonb(h)-'created_at'-'updated_at' FROM torrent_hints h WHERE h.info_hash=t.info_hash),
 'contents',COALESCE((SELECT jsonb_agg(x ORDER BY x->>'id') FROM
  (SELECT to_jsonb(c)-'created_at'-'updated_at'-'seeders'-'leechers'-'published_at'-'tsv' x
   FROM torrent_contents c WHERE c.info_hash=t.info_hash ORDER BY id LIMIT 2) c),'[]'),
 'tags',COALESCE((SELECT jsonb_agg(name ORDER BY name) FROM torrent_tags WHERE info_hash=t.info_hash),'[]'),
 'canonical',(SELECT to_jsonb(c)-'created_at'-'updated_at' FROM torrent_canonical_labels c WHERE c.info_hash=t.info_hash),
 'private_evidence',EXISTS(SELECT 1 FROM label_evidence WHERE info_hash=t.info_hash AND ` + `%s` + `),
 'held',EXISTS(SELECT 1 FROM torrent_verdict_state WHERE info_hash=t.info_hash AND verdict IN('quarantined','blacklisted','tombstoned'))
 OR EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=t.info_hash AND expired_at IS NULL)
) FROM torrents t WHERE t.info_hash=$1`

type sourceFile struct {
	Path      string  `json:"path"`
	Size      int64   `json:"size"`
	Index     int     `json:"index"`
	Extension *string `json:"extension"`
}
type source struct {
	Receipt         json.RawMessage   `json:"receipt"`
	Name            string            `json:"name"`
	Size            int64             `json:"size"`
	Private         bool              `json:"private"`
	FilesStatus     string            `json:"files_status"`
	FileCount       *int64            `json:"file_count"`
	Extension       *string           `json:"extension"`
	Files           []sourceFile      `json:"files"`
	Hint            json.RawMessage   `json:"hint"`
	Contents        []json.RawMessage `json:"contents"`
	Tags            []string          `json:"tags"`
	Canonical       json.RawMessage   `json:"canonical"`
	PrivateEvidence bool              `json:"private_evidence"`
	Held            bool              `json:"held"`
}
type contentFact struct {
	ID        string         `json:"id"`
	Type      *string        `json:"content_type"`
	Source    *string        `json:"content_source"`
	ContentID *string        `json:"content_id"`
	Episodes  model.Episodes `json:"episodes"`
}

func readSource(ctx context.Context, tx pgx.Tx, hash []byte, lock bool) (source, json.RawMessage, error) {
	var out source
	if lock {
		// Lock order matches core persistence: independent authority, then parent
		// and child rows. The short transaction has a deadline and no network I/O.
		if _, e := tx.Exec(ctx, `LOCK TABLE label_evidence,torrent_canonical_labels,torrent_verdict_state,junkpurge_quarantine,torrent_files,torrent_hints,torrent_contents,torrent_tags IN SHARE MODE`); e != nil {
			return out, nil, e
		}
		if _, e := tx.Exec(ctx, `SELECT info_hash FROM torrents WHERE info_hash=$1 FOR UPDATE`, hash); e != nil {
			return out, nil, e
		}
	}
	q := strings.Replace(sourceSQL, "%s", catalogueguard.QBPrivacySQL("source", "category", "$2"), 1)
	var raw []byte
	if e := tx.QueryRow(ctx, q, hash, catalogueguard.TagWhitespace).Scan(&raw); e != nil {
		return out, nil, e
	}
	if len(raw) > 262144 {
		return out, nil, errors.New("source_too_large")
	}
	if e := json.Unmarshal(raw, &out); e != nil {
		return out, nil, e
	}
	return out, canonical(raw), nil
}
func (s source) admitted() bool {
	if s.Private || s.PrivateEvidence || s.Held || len(s.Files) > 256 || len(s.Contents) != 1 || s.Size < 0 || len(s.Name) > 4096 {
		return false
	}
	for _, tag := range s.Tags {
		if catalogueguard.ProtectedTag(tag, false) {
			return false
		}
	}
	var c struct {
		MediaID  *string `json:"media_id"`
		Category *string `json:"category"`
	}
	_ = json.Unmarshal(s.Canonical, &c)
	if c.MediaID != nil && *c.MediaID != "" {
		return false
	}
	if c.Category != nil {
		cat := strings.ToLower(strings.TrimSpace(*c.Category))
		if cat == "private" || cat == "bitgrab" {
			return false
		}
	}
	return true
}
func (s source) torrent(hash string) model.Torrent {
	id, _ := protocol.ParseID(hash)
	t := model.Torrent{InfoHash: id, Name: s.Name, Size: uint(s.Size), Private: s.Private, FilesStatus: model.FilesStatus(s.FilesStatus)}
	if s.Extension != nil {
		t.Extension = model.NewNullString(*s.Extension)
	}
	if s.FileCount != nil && *s.FileCount >= 0 {
		t.FilesCount = model.NewNullUint(uint(*s.FileCount))
	}
	for _, f := range s.Files {
		x := model.TorrentFile{InfoHash: id, Path: f.Path, Size: uint(f.Size), Index: uint(f.Index)}
		if f.Extension != nil {
			x.Extension = model.NewNullString(*f.Extension)
		}
		t.Files = append(t.Files, x)
	}
	_ = decodeRow(s.Hint, &t.Hint)
	return t
}
func (s source) input(hash []byte, policy string, noiseV2 bool) v1.Input {
	in := v1.Input{InfoHash: hex.EncodeToString(hash), Name: s.Name, Size: s.Size, FileCount: s.FileCount, Extension: s.Extension, Files: []v1.File{}, Candidates: []v1.Candidate{}, PriorResults: []v1.Result{}}
	for _, f := range s.Files {
		in.Files = append(in.Files, v1.File{Path: f.Path, Size: f.Size})
	}
	sortFiles(in.Files)
	in.FilesComplete = s.FilesStatus == "single" || (s.FilesStatus == "multi" && s.FileCount != nil && int64(len(s.Files)) == *s.FileCount)
	in.Deterministic.MatchingOutcome = "not_attempted"
	in.Deterministic.ProcessingOutcome = "not_attempted"
	if len(s.Contents) == 1 {
		var c contentFact
		_ = json.Unmarshal(s.Contents[0], &c)
		if c.Type != nil {
			in.Deterministic.ContentType = *c.Type
		}
		if c.Source != nil && c.ContentID != nil {
			in.Deterministic.Match = &v1.MatchRef{Source: *c.Source, ID: *c.ContentID}
			in.Deterministic.MatchingOutcome = "matched"
		}
		{
			var receipt struct {
				Processing string `json:"processing_outcome"`
				Digest     string `json:"source_digest"`
				ContentID  string `json:"content_id"`
				Outcome    string `json:"outcome"`
				Policy     string `json:"core_policy"`
			}
			_ = json.Unmarshal(s.Receipt, &receipt)
			if receipt.Policy == policy && receipt.Digest == `\x`+hex.EncodeToString(llmwork.SourceDigest(s.torrent(in.InfoHash))) {
				if receipt.ContentID == c.ID && in.Deterministic.Match == nil {
					in.Deterministic.MatchingOutcome = receipt.Outcome
				}
				in.Deterministic.ProcessingOutcome = receipt.Processing
			}
		}
	}
	cl := classification.Result{}
	if in.Deterministic.ContentType != "" {
		cl.ContentType = model.NewNullContentType(model.ContentType(in.Deterministic.ContentType))
	}
	parsed, e := parsers.ParseVideoContentWithOptions(s.torrent(in.InfoHash), cl, parsers.ParseOptions{NoiseV2: noiseV2})
	if e == nil {
		in.Deterministic.Title = parsed.BaseTitle.String
		in.Deterministic.Year = int(parsed.Date.Year)
	}
	return in
}

// Translate database column names using generated model tags, avoiding another
// hand-maintained model. Bytea identity and volatile dates are supplied by core.
func decodeRow(raw []byte, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		return err
	}
	values := map[string]json.RawMessage{}
	typ := reflect.TypeOf(out).Elem()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		col := ""
		for _, tag := range strings.Split(f.Tag.Get("gorm"), ";") {
			if strings.HasPrefix(tag, "column:") {
				col = strings.TrimPrefix(tag, "column:")
			}
		}
		if col == "info_hash" || col == "created_at" || col == "updated_at" || col == "published_at" || col == "tsv" {
			continue
		}
		if value, ok := row[col]; ok {
			values[strings.Split(f.Tag.Get("json"), ",")[0]] = value
		}
	}
	body, _ := json.Marshal(values)
	return json.Unmarshal(body, out)
}
