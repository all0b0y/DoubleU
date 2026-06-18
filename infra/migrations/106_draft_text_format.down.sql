-- Revert persisted draft text_format.
ALTER TABLE public.drafts DROP CONSTRAINT IF EXISTS drafts_text_format_check;
ALTER TABLE public.drafts DROP COLUMN IF EXISTS text_format;
