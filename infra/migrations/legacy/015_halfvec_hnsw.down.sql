-- 015_halfvec_hnsw.down.sql
-- Drops the halfvec HNSW expression indexes (queries fall back to seq scan).

BEGIN;

DROP INDEX IF EXISTS ix_fe_halfvec_hnsw;
DROP INDEX IF EXISTS ix_es_halfvec_hnsw;

COMMIT;
