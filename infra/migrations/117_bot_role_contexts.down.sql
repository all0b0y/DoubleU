BEGIN;

DROP INDEX IF EXISTS public.idx_contact_profiles_role_context;
DROP INDEX IF EXISTS public.idx_profile_suggestions_role_context;
DROP INDEX IF EXISTS public.idx_extraction_log_role_context;
DROP INDEX IF EXISTS public.idx_draft_feedback_role_context;
DROP INDEX IF EXISTS public.idx_drafts_role_context_pending;
DROP INDEX IF EXISTS public.idx_drafts_role_context;
DROP INDEX IF EXISTS public.idx_active_episodes_role_context;
DROP INDEX IF EXISTS public.idx_episode_summaries_role_context;
DROP INDEX IF EXISTS public.idx_semantic_facts_role_context_active;
DROP INDEX IF EXISTS public.idx_messages_role_context_received;
DROP INDEX IF EXISTS public.idx_user_bots_active_role_context;
DROP INDEX IF EXISTS public.idx_bot_role_contexts_no_role_lookup;
DROP INDEX IF EXISTS public.idx_bot_role_contexts_lookup;

ALTER TABLE public.contact_profiles DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.contact_profile_suggestions DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.extraction_log DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.draft_feedback DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.drafts DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.active_episodes DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.episode_summaries DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.semantic_facts DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.messages DROP COLUMN IF EXISTS role_context_id;
ALTER TABLE public.user_bots DROP COLUMN IF EXISTS active_role_context_id;

DROP TABLE IF EXISTS public.bot_role_contexts;

COMMIT;
