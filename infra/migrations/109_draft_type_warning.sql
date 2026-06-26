-- 109: allow owner-only critical-risk warning cards in drafts.
ALTER TABLE public.drafts
    DROP CONSTRAINT IF EXISTS drafts_draft_type_check;

ALTER TABLE public.drafts
    ADD CONSTRAINT drafts_draft_type_check CHECK (draft_type = ANY (ARRAY[
        'reply'::text,
        'question'::text,
        'warning'::text
    ]));
