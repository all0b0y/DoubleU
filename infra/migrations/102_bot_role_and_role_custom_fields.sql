-- 102: bot-bound roles + role custom fields.
--
-- (1) A role attaches to the bot, not to individual chats: every bot-transport
--     chat answers in the bot's role. One bot per tenant, so this is a single
--     role governing all of that bot's chats.
-- (2) Owners can define arbitrary extra role fields (title + why + optional
--     context), stored alongside the fixed columns and injected into the prompt.
ALTER TABLE public.user_bots
    ADD COLUMN IF NOT EXISTS ai_role_id bigint REFERENCES public.ai_roles(id) ON DELETE SET NULL;

ALTER TABLE public.ai_roles
    ADD COLUMN IF NOT EXISTS custom_fields jsonb NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN public.user_bots.ai_role_id IS
    'Role that governs every bot-transport chat of this bot; NULL means the default assistant persona.';
COMMENT ON COLUMN public.ai_roles.custom_fields IS
    'Owner-defined extra role fields: array of {title, description, context}. Injected into the draft prompt.';
