-- Migration 010: Sleep-time fact consolidation stored procedure.
--
-- consolidate_facts(tenant_id) is called nightly (02:00 UTC) by the Go
-- StartConsolidationWorker for every active tenant. It performs three operations:
--
--   1. Hard-delete semantic_facts rows whose expires_at is in the past.
--   2. Soft-delete (deleted_at = now()) any fact whose superseded_by_id is set
--      but deleted_at is still NULL — rows that escaped the trigger.
--   3. Collapse supersession chains A→B→C into A→C so the graph stays shallow.
--
-- Returns the count of facts cleaned up (deleted + soft-deleted).

CREATE OR REPLACE FUNCTION consolidate_facts(p_tenant_id TEXT) RETURNS INT AS $$
DECLARE
  v_cleaned INT := 0;
  v_rows    INT := 0;
BEGIN
  -- 1. Hard-delete expired facts.
  DELETE FROM semantic_facts
  WHERE tenant_id = p_tenant_id
    AND expires_at IS NOT NULL
    AND expires_at < now()
    AND deleted_at IS NULL;
  GET DIAGNOSTICS v_rows = ROW_COUNT;
  v_cleaned := v_cleaned + v_rows;

  -- 2. Soft-delete facts that are superseded but not yet marked deleted.
  UPDATE semantic_facts
     SET deleted_at = now()
   WHERE tenant_id = p_tenant_id
     AND superseded_by_id IS NOT NULL
     AND deleted_at IS NULL;
  GET DIAGNOSTICS v_rows = ROW_COUNT;
  v_cleaned := v_cleaned + v_rows;

  -- 3. Collapse supersession chains: if A→B→C exists, update A.superseded_by_id = C.
  --    Iterate until no more updates (handles arbitrary chain depth).
  LOOP
    WITH chain_step AS (
      SELECT sf.id AS old_id, sf2.superseded_by_id AS new_target
        FROM semantic_facts sf
        JOIN semantic_facts sf2 ON sf2.id = sf.superseded_by_id
       WHERE sf.tenant_id = p_tenant_id
         AND sf2.superseded_by_id IS NOT NULL
         AND sf.superseded_by_id != sf2.superseded_by_id
    )
    UPDATE semantic_facts sf
       SET superseded_by_id = cs.new_target
      FROM chain_step cs
     WHERE sf.id = cs.old_id;

    GET DIAGNOSTICS v_rows = ROW_COUNT;
    EXIT WHEN v_rows = 0;
  END LOOP;

  RETURN v_cleaned;
END;
$$ LANGUAGE plpgsql;
