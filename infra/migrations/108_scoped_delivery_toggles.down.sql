-- Revert scoped Rich Messages and pseudo-streaming delivery toggles.
ALTER TABLE public.user_bots
  DROP COLUMN IF EXISTS streaming_enabled,
  DROP COLUMN IF EXISTS rich_formatting_enabled;

ALTER TABLE public.ai_roles
  DROP COLUMN IF EXISTS streaming_enabled,
  DROP COLUMN IF EXISTS rich_formatting_enabled;

ALTER TABLE public.user_settings
  DROP COLUMN IF EXISTS streaming_enabled;
