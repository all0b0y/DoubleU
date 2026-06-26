ALTER TABLE public.ai_roles
    ADD COLUMN IF NOT EXISTS model_name text NOT NULL DEFAULT '';

COMMENT ON COLUMN public.ai_roles.model_name IS
    'Optional concrete model slug/alias selected within ai_roles.model_tier.';
