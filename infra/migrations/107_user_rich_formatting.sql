-- 107: owner-level master switch for Telegram Rich Messages.
ALTER TABLE public.user_settings
  ADD COLUMN IF NOT EXISTS rich_formatting_enabled boolean NOT NULL DEFAULT true;
