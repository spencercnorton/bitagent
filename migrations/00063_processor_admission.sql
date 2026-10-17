-- +goose Up
ALTER TABLE processor_control
 ADD COLUMN admission_generation bigint NOT NULL DEFAULT 1,
 ADD COLUMN admission_basis_points integer NOT NULL DEFAULT 10000 CHECK(admission_basis_points BETWEEN 0 AND 10000),
 ADD COLUMN admission_seed text NOT NULL DEFAULT repeat('0',64) CHECK(admission_seed ~ '^[0-9a-f]{64}$'),
 ADD COLUMN admission_policy_version text NOT NULL DEFAULT 'all-v1';

CREATE TABLE processor_admission_history (
 generation bigint PRIMARY KEY,
 policy jsonb NOT NULL,
 reason text NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO processor_admission_history(generation,policy,reason)
 VALUES(1,jsonb_build_object('contract','bitagent.processors.admission.v1','generation',1,
 'algorithm','sha256-seed-infohash-v1','seed',repeat('0',64),'basis_points',10000,'policy_version','all-v1'),
 'initial_compatible_admission');

-- Freeze old plans before admitting work bound to the new policy. Preserve
-- completed facts and all attempt history; old in-flight results become stale.
UPDATE processor_control SET epoch=epoch+1,scan_after=''::bytea,updated_at=clock_timestamp();
WITH invalidated AS (
 UPDATE processor_jobs SET state='stale',reason='admission_contract_upgrade',lease_until=NULL,finished_at=clock_timestamp()
 WHERE state IN('queued','deferred','leased') RETURNING job_id,lease_generation)
UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='unknown',reason='admission_contract_upgrade'
 FROM invalidated j WHERE a.job_id=j.job_id AND a.lease_generation=j.lease_generation AND a.finished_at IS NULL;
WITH generations AS (SELECT id,nextval('processor_registry_generation') AS generation FROM processor_registry)
UPDATE processor_registry r SET generation=g.generation,execution_generation=g.generation,updated_at=clock_timestamp()
 FROM generations g WHERE g.id=r.id;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM processor_admission_history WHERE generation>1) THEN
  RAISE EXCEPTION 'admission audit evidence prevents destructive downgrade';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE processor_admission_history;
ALTER TABLE processor_control DROP COLUMN admission_generation,DROP COLUMN admission_basis_points,
 DROP COLUMN admission_seed,DROP COLUMN admission_policy_version;
