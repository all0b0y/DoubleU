-- 104: sleep-time reflection facts.
--
-- The nightly worker synthesises high-level "behavior pattern" insights from
-- clusters of related facts (Generative Agents reflection). These need a new
-- fact_type ('behavior_pattern') and a distinct provenance ('llm_reflected')
-- so they are auditable and separable from per-message extractions for evals.
ALTER TABLE public.semantic_facts
    DROP CONSTRAINT IF EXISTS semantic_facts_fact_type_check;
ALTER TABLE public.semantic_facts
    ADD CONSTRAINT semantic_facts_fact_type_check CHECK (fact_type = ANY (ARRAY[
        'preference', 'biographical', 'commitment', 'event',
        'relationship', 'health', 'taboo', 'behavior_pattern'
    ]));

ALTER TABLE public.semantic_facts
    DROP CONSTRAINT IF EXISTS semantic_facts_source_check;
ALTER TABLE public.semantic_facts
    ADD CONSTRAINT semantic_facts_source_check CHECK (source = ANY (ARRAY[
        'llm_extracted', 'owner_confirmed', 'owner_authored', 'llm_reflected'
    ]));
