-- Revert reflection facts. Removes any reflected rows first, then restores the
-- original CHECK constraints (the restore would otherwise fail on existing
-- behavior_pattern / llm_reflected rows).
DELETE FROM public.semantic_facts WHERE source = 'llm_reflected' OR fact_type = 'behavior_pattern';

ALTER TABLE public.semantic_facts
    DROP CONSTRAINT IF EXISTS semantic_facts_fact_type_check;
ALTER TABLE public.semantic_facts
    ADD CONSTRAINT semantic_facts_fact_type_check CHECK (fact_type = ANY (ARRAY[
        'preference', 'biographical', 'commitment', 'event',
        'relationship', 'health', 'taboo'
    ]));

ALTER TABLE public.semantic_facts
    DROP CONSTRAINT IF EXISTS semantic_facts_source_check;
ALTER TABLE public.semantic_facts
    ADD CONSTRAINT semantic_facts_source_check CHECK (source = ANY (ARRAY[
        'llm_extracted', 'owner_confirmed', 'owner_authored'
    ]));
