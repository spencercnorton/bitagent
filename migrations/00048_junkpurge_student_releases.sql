-- +goose Up
-- +goose StatementBegin

-- The student's RELEASE arm, and only its release arm.
--
-- A release says "this candidate is confidently real, so do not spend a paid LLM
-- call on it". It keeps things. It cannot create false-junk, which is why it is
-- the one part of the student that is invariant to the gold-contract rewrite:
-- 43.51% of the stream releases under BOTH the raw cataloguedness gold and the
-- policy-corrected gold. The ACTION arm (deleting on the student's own verdict)
-- is a different matter entirely and is gated on that rewrite.
--
-- Two design constraints, both load-bearing:
--
-- 1. Releases must NOT be written to junkpurge_judgments. That table is the gold
--    sampling frame, and every reader of it selects on `verdict` with no
--    provenance predicate (tools/llmeval/oracle_pilot.go, junkpurge/worker.go),
--    so student rows mixed in would silently corrupt the corpus AND touch the
--    only preserved retrospective safety evidence that exists.
--
-- 2. This table has no verdict, action or disposition column, and that omission
--    is the point. A release is expressible here; a deletion is not. There is no
--    sentinel threshold that turns this into an action path by accident -- the
--    reported footgun was a `p >= 0` default silently actioning everything.
--    Structure enforces it instead of a startup assertion.
--
-- WRITER CONTRACT (nothing writes this table yet -- see AGENTS/journal.md):
-- the scorer runs OFFLINE, out of the serving path, and inserts one row per
-- released candidate. Deliberately not in-worker inference: the worker needs no
-- model loading, no feature pipeline and no retraining hook to honour a release,
-- because the release is already a row. Re-scoring under a new model_id is an
-- upsert; withdrawing a release is a DELETE.
create table junkpurge_student_releases
(
  info_hash   bytea       primary key check (octet_length(info_hash) = 20),
  p_junk      real        not null check (p_junk >= 0 and p_junk <= 1),
  -- The operating point that admitted this row, recorded per row so that raising
  -- or lowering it later stays auditable and the W3.2 release-band audit can
  -- sample the band it actually cares about. Threshold lives with the scorer, not
  -- in worker config: the worker never compares anything, it only honours rows.
  threshold   real        not null check (threshold >= 0 and threshold <= 1),
  model_id    text        not null,
  released_at timestamptz not null default now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists junkpurge_student_releases;

-- +goose StatementEnd
