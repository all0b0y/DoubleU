-- 111: add model_tier to ai_roles for per-role LLM quality selection.
ALTER TABLE public.ai_roles
    ADD COLUMN IF NOT EXISTS model_tier text
        NOT NULL
        DEFAULT 'balanced'
        CHECK (model_tier IN ('fast', 'balanced', 'smart'));

COMMENT ON COLUMN public.ai_roles.model_tier IS
    'LLM quality tier for draft generation: fast / balanced (default) / smart.';
