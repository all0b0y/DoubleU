-- 108: scoped Rich Messages and pseudo-streaming delivery toggles.
-- NULL on bot/role rows means inherit the next broader scope.
ALTER TABLE public.user_settings
  ADD COLUMN IF NOT EXISTS streaming_enabled boolean NOT NULL DEFAULT false;

ALTER TABLE public.ai_roles
  ADD COLUMN IF NOT EXISTS rich_formatting_enabled boolean,
  ADD COLUMN IF NOT EXISTS streaming_enabled boolean;

ALTER TABLE public.user_bots
  ADD COLUMN IF NOT EXISTS rich_formatting_enabled boolean,
  ADD COLUMN IF NOT EXISTS streaming_enabled boolean;
