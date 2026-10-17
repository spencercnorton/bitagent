package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/evidence"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

func insertFeedbackTx(
	ctx context.Context,
	tx pgx.Tx,
	processorID string,
	execGen int64,
	jobID string,
	hash []byte,
	tier string,
	in *v1.Input,
	pred *v1.Result,
	trace json.RawMessage,
	gt v1.FeedbackGroundTruth,
	entry *Entry,
) (*v1.FeedbackEvent, error) {
	eventID := digest([]any{processorID, execGen, jobID, hex.EncodeToString(hash), tier, gt})
	var inRaw, predRaw, traceRaw any
	if in != nil {
		b, _ := json.Marshal(in)
		inRaw = string(b)
	}
	if pred != nil {
		b, _ := json.Marshal(pred)
		predRaw = string(b)
	}
	if len(trace) > 0 {
		traceRaw = string(trace)
	}
	gtBytes, _ := json.Marshal(gt)
	deliveryState := "pending"
	if entry != nil && entry.Driver != "" && entry.Driver != v1.DriverHTTP {
		deliveryState = "not_applicable"
	}
	var seq int64
	var created time.Time
	err := tx.QueryRow(ctx, `INSERT INTO processor_feedback_events
  (event_id,processor_id,execution_generation,job_id,info_hash,tier,input,prediction,trace,ground_truth,delivery_state)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
  ON CONFLICT(event_id) DO UPDATE SET event_id=EXCLUDED.event_id
  RETURNING seq,created_at`,
		eventID, processorID, execGen, jobID, hash, tier, inRaw, predRaw, traceRaw, string(gtBytes), deliveryState,
	).Scan(&seq, &created)
	if err != nil {
		return nil, err
	}
	if entry != nil && entry.Driver == v1.DriverBuiltinRegex && entry.Learning != nil && entry.Learning.Enabled && entry.Learning.Mode == "online_calibrate" {
		var currentState []byte
		if err := tx.QueryRow(ctx, `SELECT learned_state FROM processor_registry WHERE id=$1 FOR UPDATE`, entry.ID).Scan(&currentState); err == nil {
			if nextState, ok := calibrateBuiltinRegex(currentState, trace, pred, gt); ok {
				_, _ = tx.Exec(ctx, `UPDATE processor_registry SET learned_state=$2,updated_at=clock_timestamp() WHERE id=$1`, entry.ID, string(nextState))
			}
		}
	}
	return &v1.FeedbackEvent{
		Seq:                 seq,
		EventID:             eventID,
		ProcessorID:         processorID,
		ExecutionGeneration: execGen,
		JobID:               jobID,
		InfoHash:            hex.EncodeToString(hash),
		Tier:                tier,
		Input:               in,
		Prediction:          pred,
		Trace:               trace,
		GroundTruth:         gt,
		CreatedAt:           stamp(created),
	}, nil
}

func (s *Store) recordExecutionFeedback(
	ctx context.Context,
	tx pgx.Tx,
	l Lease,
	stepOutcomes map[int]string,
	finalType string,
	finalMatch *v1.MatchRef,
	finalExcluded bool,
) error {
	var teacherStep = -1
	for i := len(l.Results) - 1; i >= 0; i-- {
		if i < len(l.Plan.Steps) && l.Plan.Steps[i].Entry.Mode == "apply" &&
			(l.Results[i].Result.Outcome == v1.Completed || l.Results[i].Result.Outcome == v1.Excluded) &&
			l.Results[i].Result.Proposal != nil {
			teacherStep = i
			break
		}
	}
	for i, receipt := range l.Results {
		if i >= len(l.Plan.Steps) {
			break
		}
		entry := l.Plan.Steps[i].Entry
		r := receipt.Result
		if (entry.Learning == nil || !entry.Learning.Enabled) && !r.RequestFeedback {
			continue
		}
		if r.Outcome == v1.Abstained && !r.RequestFeedback {
			continue
		}
		inCopy := l.Input
		rCopy := r
		if (entry.Learning != nil && entry.Learning.Accepts(v1.FeedbackTierOperationalReceipt)) || r.RequestFeedback {
			outcome := stepOutcomes[i]
			if outcome == "" {
				outcome = string(r.Outcome)
			}
			gt := v1.FeedbackGroundTruth{
				Outcome:     outcome,
				ContentType: finalType,
				Match:       finalMatch,
				Excluded:    &finalExcluded,
				Source:      "core_apply",
			}
			if _, err := insertFeedbackTx(ctx, tx, entry.ID, entry.ExecutionGeneration, l.ID, l.Hash, v1.FeedbackTierOperationalReceipt, &inCopy, &rCopy, r.Trace, gt, &entry); err != nil {
				return err
			}
		}
		if teacherStep > i && entry.Learning != nil && entry.Learning.Accepts(v1.FeedbackTierTeacherConsensus) {
			teacherRes := l.Results[teacherStep].Result
			teacherProp := teacherRes.Proposal
			teacherExcl := teacherRes.Outcome == v1.Excluded
			gt := v1.FeedbackGroundTruth{
				Outcome:     "teacher_consensus",
				ContentType: teacherProp.ContentType,
				Extraction:  teacherProp.Extraction,
				Language:    teacherProp.Language,
				Match:       teacherProp.Match,
				Liveness:    teacherProp.Liveness,
				Excluded:    &teacherExcl,
				Source:      "processor:" + l.Plan.Steps[teacherStep].Entry.ID,
			}
			if _, err := insertFeedbackTx(ctx, tx, entry.ID, entry.ExecutionGeneration, l.ID, l.Hash, v1.FeedbackTierTeacherConsensus, &inCopy, &rCopy, r.Trace, gt, &entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// HandleEvidence is an evidence.PostInsertHook that routes verified downstream
// *arr and qBittorrent observations back to Processors with learning enabled.
func (s *Store) HandleEvidence(ctx context.Context, ev evidence.Evidence) {
	if len(ev.InfoHash) != 20 {
		return
	}
	cat := strings.ToLower(strings.TrimSpace(ev.Category))
	if cat == "private" || cat == "bitgrab" || catalogueguard.ProtectedTag(cat, false) {
		return
	}
	gt := v1.FeedbackGroundTruth{
		Source: string(ev.Source) + ":" + string(ev.Kind),
	}
	switch ev.Kind {
	case evidence.KindWebhookImport, evidence.KindPollHistory:
		gt.Outcome = "import_succeeded"
	case evidence.KindWebhookGrab, evidence.KindPollCategories:
		gt.Outcome = "applied"
	case evidence.KindQBStateObservation:
		state := strings.ToLower(ev.QBState)
		obs := "unknown"
		if strings.Contains(state, "up") || strings.Contains(state, "dl") || state == "uploading" || state == "downloading" || state == "seeding" {
			obs = "live"
			gt.Outcome = "applied"
		} else if state == "error" || state == "missingfiles" {
			obs = "dead"
			gt.Outcome = "import_failed"
		} else {
			return
		}
		gt.Liveness = &v1.LivenessClaim{Observation: obs, Source: string(ev.Source)}
	default:
		return
	}
	switch ev.MediaType {
	case evidence.MediaTypeMovie:
		gt.ContentType = "movie"
	case evidence.MediaTypeTV:
		gt.ContentType = "tv_show"
	case evidence.MediaTypeMusic:
		gt.ContentType = "music"
	case evidence.MediaTypeBook:
		gt.ContentType = "ebook"
	case evidence.MediaTypeAudiobook:
		gt.ContentType = "audiobook"
	}
	if ev.MediaID != "" {
		parts := strings.SplitN(ev.MediaID, ":", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			gt.Match = &v1.MatchRef{Source: parts[0], ID: parts[1]}
		} else {
			src := "tmdb"
			if ev.Source == evidence.SourceSonarr {
				src = "tvdb"
			} else if ev.Source == evidence.SourceLidarr {
				src = "musicbrainz"
			} else if ev.Source == evidence.SourceReadarr {
				src = "openlibrary"
			}
			gt.Match = &v1.MatchRef{Source: src, ID: ev.MediaID}
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = s.RecordFeedback(bounded, v1.FeedbackWrite{
		InfoHash:    hex.EncodeToString(ev.InfoHash),
		Tier:        v1.FeedbackTierVerifiedImport,
		GroundTruth: gt,
	})
}

func (s *Store) RecordFeedback(ctx context.Context, w v1.FeedbackWrite) ([]v1.FeedbackEvent, error) {
	hash, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(w.InfoHash)))
	if err != nil || len(hash) != 20 {
		return nil, ErrControl
	}
	if !slices.Contains(v1.FeedbackTiers, w.Tier) || w.GroundTruth.Outcome == "" {
		return nil, ErrControl
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	src, _, err := readSource(ctx, tx, hash, false)
	if err != nil {
		return nil, err
	}
	if !src.admittedForFeedback() {
		return nil, ErrStale
	}
	es, err := entries(ctx, tx)
	if err != nil {
		return nil, err
	}
	entryByID := map[string]Entry{}
	for _, e := range es {
		entryByID[e.ID] = e
	}
	var jobID string
	var inBytes, planBytes, resBytes []byte
	var input v1.Input
	var plan Plan
	var receipts []Receipt
	hasJob := false
	err = tx.QueryRow(ctx, `SELECT job_id,input,plan,results FROM processor_jobs WHERE info_hash=$1 AND state='completed' ORDER BY finished_at DESC NULLS LAST,created_at DESC LIMIT 1`, hash).Scan(&jobID, &inBytes, &planBytes, &resBytes)
	if err == nil {
		_ = json.Unmarshal(inBytes, &input)
		_ = json.Unmarshal(planBytes, &plan)
		_ = json.Unmarshal(resBytes, &receipts)
		hasJob = true
	} else {
		input = src.input(hash, s.policy, s.noiseV2)
	}
	var out []v1.FeedbackEvent
	recordedProc := map[string]bool{}
	if hasJob {
		for i, step := range plan.Steps {
			procID := step.Entry.ID
			if w.ProcessorID != "" && procID != w.ProcessorID {
				continue
			}
			currentEntry, ok := entryByID[procID]
			if !ok {
				currentEntry = step.Entry
			}
			if w.ProcessorID == "" && (currentEntry.Learning == nil || !currentEntry.Learning.Accepts(w.Tier)) {
				continue
			}
			var pred *v1.Result
			var trace json.RawMessage
			if i < len(receipts) {
				r := receipts[i].Result
				pred = &r
				trace = r.Trace
			}
			ev, err := insertFeedbackTx(ctx, tx, procID, currentEntry.ExecutionGeneration, jobID, hash, w.Tier, &input, pred, trace, w.GroundTruth, &currentEntry)
			if err != nil {
				return nil, err
			}
			out = append(out, *ev)
			recordedProc[procID] = true
		}
	}
	for _, entry := range es {
		if recordedProc[entry.ID] {
			continue
		}
		if w.ProcessorID != "" && entry.ID != w.ProcessorID {
			continue
		}
		if w.ProcessorID == "" && (entry.Learning == nil || !entry.Learning.Accepts(w.Tier)) {
			continue
		}
		ev, err := insertFeedbackTx(ctx, tx, entry.ID, entry.ExecutionGeneration, jobID, hash, w.Tier, &input, nil, nil, w.GroundTruth, &entry)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) Feedback(ctx context.Context, processorID string, afterSeq int64, limit int, tier string, consumerID string) (v1.FeedbackPage, error) {
	out := v1.FeedbackPage{Contract: v1.FeedbackContract, Items: []v1.FeedbackEvent{}}
	if limit < 1 || limit > 100 || afterSeq < 0 {
		return out, ErrControl
	}
	if tier != "" && !slices.Contains(v1.FeedbackTiers, tier) {
		return out, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return out, err
	}
	if afterSeq == 0 && processorID != "" && consumerID != "" {
		_ = p.QueryRow(ctx, `SELECT last_seq FROM processor_feedback_cursors WHERE processor_id=$1 AND consumer_id=$2`, processorID, consumerID).Scan(&afterSeq)
	}
	rows, err := p.Query(ctx, `SELECT seq,event_id,processor_id,execution_generation,job_id,info_hash,tier,input,prediction,trace,ground_truth,created_at
  FROM processor_feedback_events
  WHERE seq>$1 AND ($2='' OR processor_id=$2) AND ($3='' OR tier=$3)
  ORDER BY seq ASC LIMIT $4`, afterSeq, processorID, tier, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ev v1.FeedbackEvent
		var hash, inRaw, predRaw, traceRaw, gtRaw []byte
		var created time.Time
		if err := rows.Scan(&ev.Seq, &ev.EventID, &ev.ProcessorID, &ev.ExecutionGeneration, &ev.JobID, &hash, &ev.Tier, &inRaw, &predRaw, &traceRaw, &gtRaw, &created); err != nil {
			return out, err
		}
		ev.InfoHash = hex.EncodeToString(hash)
		if len(inRaw) > 0 {
			var in v1.Input
			if json.Unmarshal(inRaw, &in) == nil {
				ev.Input = &in
			}
		}
		if len(predRaw) > 0 {
			var pred v1.Result
			if json.Unmarshal(predRaw, &pred) == nil {
				ev.Prediction = &pred
			}
		}
		if len(traceRaw) > 0 {
			ev.Trace = json.RawMessage(traceRaw)
		}
		_ = json.Unmarshal(gtRaw, &ev.GroundTruth)
		ev.CreatedAt = stamp(created)
		out.Items = append(out.Items, ev)
		out.NextCursor = ev.Seq
	}
	return out, rows.Err()
}

func (s *Store) AckFeedback(ctx context.Context, ack v1.FeedbackCursorAck) error {
	if ack.ProcessorID == "" || len(ack.ProcessorID) > 96 || ack.LastSeq < 0 {
		return ErrControl
	}
	consumer := ack.ConsumerID
	if consumer == "" {
		consumer = "default"
	}
	if len(consumer) > 96 {
		return ErrControl
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	_, err = tx.Exec(ctx, `INSERT INTO processor_feedback_cursors(processor_id,consumer_id,last_seq,updated_at)
  VALUES($1,$2,$3,clock_timestamp())
  ON CONFLICT(processor_id,consumer_id) DO UPDATE SET last_seq=GREATEST(processor_feedback_cursors.last_seq,EXCLUDED.last_seq),updated_at=clock_timestamp()`,
		ack.ProcessorID, consumer, ack.LastSeq)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE processor_feedback_events SET delivery_state='pulled',delivered_at=clock_timestamp()
  WHERE processor_id=$1 AND seq<=$2 AND delivery_state='pending'`, ack.ProcessorID, ack.LastSeq)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeliverFeedback(ctx context.Context) (int, error) {
	es, err := s.Entries(ctx)
	if err != nil {
		return 0, err
	}
	p, err := s.pool.Get()
	if err != nil {
		return 0, err
	}
	deliveredTotal := 0
	for _, entry := range es {
		if entry.State != "active" || (entry.Driver != "" && entry.Driver != v1.DriverHTTP) || entry.Learning == nil || !entry.Learning.Enabled || !entry.Learning.PushEnabled {
			continue
		}
		rows, err := p.Query(ctx, `SELECT seq,event_id,processor_id,execution_generation,job_id,info_hash,tier,input,prediction,trace,ground_truth,created_at
   FROM processor_feedback_events
   WHERE processor_id=$1 AND delivery_state='pending' AND delivery_attempts<$2
   ORDER BY seq ASC LIMIT 32`, entry.ID, s.cfg.MaxAttempts)
		if err != nil {
			continue
		}
		var events []v1.FeedbackEvent
		var seqs []int64
		for rows.Next() {
			var ev v1.FeedbackEvent
			var hash, inRaw, predRaw, traceRaw, gtRaw []byte
			var created time.Time
			if err := rows.Scan(&ev.Seq, &ev.EventID, &ev.ProcessorID, &ev.ExecutionGeneration, &ev.JobID, &hash, &ev.Tier, &inRaw, &predRaw, &traceRaw, &gtRaw, &created); err != nil {
				break
			}
			ev.InfoHash = hex.EncodeToString(hash)
			if len(inRaw) > 0 {
				var in v1.Input
				if json.Unmarshal(inRaw, &in) == nil {
					ev.Input = &in
				}
			}
			if len(predRaw) > 0 {
				var pred v1.Result
				if json.Unmarshal(predRaw, &pred) == nil {
					ev.Prediction = &pred
				}
			}
			if len(traceRaw) > 0 {
				ev.Trace = json.RawMessage(traceRaw)
			}
			_ = json.Unmarshal(gtRaw, &ev.GroundTruth)
			ev.CreatedAt = stamp(created)
			events = append(events, ev)
			seqs = append(seqs, ev.Seq)
		}
		rows.Close()
		if len(events) == 0 {
			continue
		}
		batch := v1.FeedbackBatch{
			Contract:  v1.FeedbackContract,
			Processor: entry.Manifest.Processor,
			Events:    events,
		}
		bounded, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		ack, pushErr := s.transport.Feedback(bounded, entry.Endpoint, entry.AuthEnv, batch)
		cancel()
		if pushErr == nil {
			_, _ = p.Exec(ctx, `UPDATE processor_feedback_events SET delivery_state='delivered',delivery_attempts=delivery_attempts+1,delivered_at=clock_timestamp() WHERE seq=ANY($1)`, seqs)
			if len(ack.LearnedState) > 0 {
				_, _ = p.Exec(ctx, `UPDATE processor_registry SET learned_state=$2,updated_at=clock_timestamp() WHERE id=$1`, entry.ID, string(ack.LearnedState))
			}
			deliveredTotal += len(events)
		} else {
			_, _ = p.Exec(ctx, `UPDATE processor_feedback_events SET delivery_attempts=delivery_attempts+1,delivery_state=CASE WHEN delivery_attempts+1>=$2 THEN 'failed' ELSE 'pending' END WHERE seq=ANY($1)`, seqs, s.cfg.MaxAttempts)
		}
	}
	return deliveredTotal, nil
}

func (s source) admittedForFeedback() bool {
	if s.Private || s.PrivateEvidence || s.Held || len(s.Files) > 256 || len(s.Contents) != 1 || s.Size < 0 || len(s.Name) > 4096 {
		return false
	}
	for _, tag := range s.Tags {
		if catalogueguard.ProtectedTag(tag, false) {
			return false
		}
	}
	var c struct {
		Category *string `json:"category"`
	}
	_ = json.Unmarshal(s.Canonical, &c)
	if c.Category != nil {
		cat := strings.ToLower(strings.TrimSpace(*c.Category))
		if cat == "private" || cat == "bitgrab" {
			return false
		}
	}
	return true
}
