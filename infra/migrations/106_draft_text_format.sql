-- 106: persist AI response text_format on drafts.
ALTER TABLE public.drafts
  ADD COLUMN IF NOT EXISTS text_format text NOT NULL DEFAULT 'plain';

DO $$
BEGIN
  ALTER TABLE public.drafts
    ADD CONSTRAINT drafts_text_format_check
    CHECK (text_format = ANY (ARRAY['plain'::text, 'rich'::text]));
EXCEPTION
  WHEN duplicate_object THEN NULL;
END $$;
