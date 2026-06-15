-- Revert multi-bot. NOTE: this fails if any tenant currently owns more than one
-- bot — drop the extras first. Restores the single-bot uniqueness constraint.
DROP INDEX IF EXISTS idx_user_bots_tenant;
ALTER TABLE public.user_bots ADD CONSTRAINT user_bots_tenant_id_key UNIQUE (tenant_id);
