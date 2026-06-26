-- Revert warning drafts before restoring the narrower CHECK constraint.
DELETE FROM public.drafts
WHERE draft_type = 'warning';

ALTER TABLE public.drafts
    DROP CONSTRAINT IF EXISTS drafts_draft_type_check;

ALTER TABLE public.drafts
    ADD CONSTRAINT drafts_draft_type_check CHECK (draft_type = ANY (ARRAY[
        'reply'::text,
        'question'::text
    ]));
