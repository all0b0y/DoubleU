-- Multi-bot: a tenant may own several Telegram bots.
--
-- Previously user_bots.tenant_id was UNIQUE, so each tenant had exactly one bot
-- and registering a new one replaced the old. Drop that constraint. bot_id stays
-- globally UNIQUE (a bot belongs to one tenant), which is the upsert/delete key.
ALTER TABLE public.user_bots DROP CONSTRAINT IF EXISTS user_bots_tenant_id_key;

-- Help the per-tenant bot listing.
CREATE INDEX IF NOT EXISTS idx_user_bots_tenant ON public.user_bots USING btree (tenant_id);
