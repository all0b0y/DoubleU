-- 111: per-role AI configuration columns for ai_roles.
--
-- Merges two formerly-colliding version-111 files into one so the migration
-- sequence is strictly linear on a fresh database.  Both columns are
-- idempotent (ADD COLUMN IF NOT EXISTS) so re-applying on an existing DB
-- that already has one or both columns is safe.
ALTER TABLE public.ai_roles
    ADD COLUMN IF NOT EXISTS speak_as_owner boolean NOT NULL DEFAULT true;

COMMENT ON COLUMN public.ai_roles.speak_as_owner IS
    'true (default/legacy): AI impersonates the owner. false: AI replies openly as the role persona.';

ALTER TABLE public.ai_roles
    ADD COLUMN IF NOT EXISTS model_tier text
        NOT NULL
        DEFAULT 'balanced'
        CHECK (model_tier IN ('fast', 'balanced', 'smart'));

COMMENT ON COLUMN public.ai_roles.model_tier IS
    'LLM quality tier for draft generation: fast / balanced (default) / smart.';
