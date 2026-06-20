-- 111 rollback: remove model_tier from ai_roles.
ALTER TABLE public.ai_roles
    DROP COLUMN IF EXISTS model_tier;
