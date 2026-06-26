-- Revert owner-level Rich Messages switch.
ALTER TABLE public.user_settings DROP COLUMN IF EXISTS rich_formatting_enabled;
