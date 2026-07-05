-- Revert 122: personal-bot mode.
DROP TABLE IF EXISTS public.personal_bot_stub_shown;
DROP TABLE IF EXISTS public.personal_bot_allowed_users;
ALTER TABLE public.user_bots DROP COLUMN IF EXISTS personal_mode;
