-- 111 rollback: remove both ai_roles columns added in version 111.
ALTER TABLE public.ai_roles DROP COLUMN IF EXISTS speak_as_owner;
ALTER TABLE public.ai_roles DROP COLUMN IF EXISTS model_tier;
