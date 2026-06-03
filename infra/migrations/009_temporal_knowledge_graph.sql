-- Migration 009: Temporal knowledge graph for commitment/event/relationship facts.
--
-- fact_relations models directed temporal edges between semantic_facts rows:
--   - supersedes: fact_to replaces fact_from (e.g. new address overrides old one)
--   - fulfills:   a commitment fact was completed
--   - contradicts: new info contradicts the old fact (both recorded, newer wins)
--   - updates:    partial update, both fact_from and fact_to remain valid
--   - related:    soft association (same topic / entity)
--
-- valid_until = NULL means the relation is still active.
-- The current_facts view returns only facts that have no active supersedes edge
-- pointing away from them (i.e. they have not been replaced by a newer fact).
--
-- The Go backend creates 'supersedes' edges in saveTypedFacts when
-- upsert_semantic_fact returns action='superseded'.

CREATE TABLE IF NOT EXISTS fact_relations (
  id             BIGSERIAL PRIMARY KEY,
  tenant_id      TEXT NOT NULL,
  fact_id_from   BIGINT NOT NULL REFERENCES semantic_facts(id) ON DELETE CASCADE,
  fact_id_to     BIGINT NOT NULL REFERENCES semantic_facts(id) ON DELETE CASCADE,
  relation_type  TEXT NOT NULL CHECK (relation_type IN (
                   'supersedes',
                   'fulfills',
                   'contradicts',
                   'updates',
                   'related'
                 )),
  valid_from     TIMESTAMPTZ NOT NULL DEFAULT now(),
  valid_until    TIMESTAMPTZ,
  confidence     REAL NOT NULL DEFAULT 1.0 CHECK (confidence BETWEEN 0 AND 1),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (fact_id_from, fact_id_to, relation_type)
);

CREATE INDEX IF NOT EXISTS ix_fr_from ON fact_relations(tenant_id, fact_id_from);
CREATE INDEX IF NOT EXISTS ix_fr_to   ON fact_relations(tenant_id, fact_id_to);

-- current_facts: facts that have not been superseded by a newer active relation.
-- Use this view when you want "current truth" — e.g. current address, not all addresses.
CREATE OR REPLACE VIEW current_facts AS
  SELECT sf.*
  FROM semantic_facts sf
  WHERE sf.deleted_at IS NULL
    AND sf.superseded_by_id IS NULL
    AND NOT EXISTS (
      SELECT 1 FROM fact_relations fr
      WHERE fr.fact_id_from = sf.id
        AND fr.relation_type = 'supersedes'
        AND fr.valid_until IS NULL
    );
