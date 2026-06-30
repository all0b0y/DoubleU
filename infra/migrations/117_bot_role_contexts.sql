BEGIN;

CREATE TABLE IF NOT EXISTS public.bot_role_contexts (
    id bigserial PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    bot_id bigint NOT NULL REFERENCES public.user_bots(bot_id) ON DELETE CASCADE,
    ai_role_id bigint NULL REFERENCES public.ai_roles(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_activated_at timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz NULL
);

ALTER TABLE public.user_bots
    ADD COLUMN IF NOT EXISTS active_role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;

ALTER TABLE public.messages
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.semantic_facts
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.episode_summaries
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.active_episodes
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.drafts
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.draft_feedback
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.extraction_log
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.contact_profile_suggestions
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;
ALTER TABLE public.contact_profiles
    ADD COLUMN IF NOT EXISTS role_context_id bigint
    REFERENCES public.bot_role_contexts(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_bot_role_contexts_lookup
    ON public.bot_role_contexts (tenant_id, bot_id, ai_role_id, last_activated_at DESC, id DESC)
    WHERE archived_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_bot_role_contexts_no_role_lookup
    ON public.bot_role_contexts (tenant_id, bot_id, last_activated_at DESC, id DESC)
    WHERE archived_at IS NULL AND ai_role_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_user_bots_active_role_context
    ON public.user_bots (tenant_id, bot_id, active_role_context_id);

CREATE INDEX IF NOT EXISTS idx_messages_role_context_received
    ON public.messages (tenant_id, chat_id, role_context_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_semantic_facts_role_context_active
    ON public.semantic_facts (tenant_id, chat_id, role_context_id, fact_type)
    WHERE scope_kind = 'chat' AND deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_episode_summaries_role_context
    ON public.episode_summaries (tenant_id, chat_id, role_context_id, period_end DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_active_episodes_role_context
    ON public.active_episodes (tenant_id, chat_id, role_context_id);
CREATE INDEX IF NOT EXISTS idx_drafts_role_context
    ON public.drafts (tenant_id, chat_id, role_context_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_drafts_role_context_pending
    ON public.drafts (tenant_id, chat_id, role_context_id, created_at DESC)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_draft_feedback_role_context
    ON public.draft_feedback (tenant_id, chat_id, role_context_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_extraction_log_role_context
    ON public.extraction_log (tenant_id, chat_id, role_context_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_profile_suggestions_role_context
    ON public.contact_profile_suggestions (tenant_id, chat_id, role_context_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_contact_profiles_role_context
    ON public.contact_profiles (tenant_id, chat_id, role_context_id);

-- Create one reusable context for each bot's current role. ai_role_id NULL is
-- the no-role/default-assistant context; no magic role id is stored.
INSERT INTO public.bot_role_contexts (tenant_id, bot_id, ai_role_id)
SELECT ub.tenant_id, ub.bot_id, ub.ai_role_id
  FROM public.user_bots ub
 WHERE NOT EXISTS (
       SELECT 1
         FROM public.bot_role_contexts brc
        WHERE brc.tenant_id = ub.tenant_id
          AND brc.bot_id = ub.bot_id
          AND (
              (brc.ai_role_id IS NULL AND ub.ai_role_id IS NULL)
              OR brc.ai_role_id = ub.ai_role_id
          )
          AND brc.archived_at IS NULL
 );

UPDATE public.user_bots ub
   SET active_role_context_id = (
           SELECT brc.id
             FROM public.bot_role_contexts brc
            WHERE brc.tenant_id = ub.tenant_id
              AND brc.bot_id = ub.bot_id
              AND (
                  (brc.ai_role_id IS NULL AND ub.ai_role_id IS NULL)
                  OR brc.ai_role_id = ub.ai_role_id
              )
              AND brc.archived_at IS NULL
            ORDER BY brc.last_activated_at DESC, brc.id DESC
            LIMIT 1
       ),
       updated_at = now()
 WHERE ub.active_role_context_id IS NULL
   AND EXISTS (
       SELECT 1
         FROM public.bot_role_contexts brc
        WHERE brc.tenant_id = ub.tenant_id
          AND brc.bot_id = ub.bot_id
          AND (
              (brc.ai_role_id IS NULL AND ub.ai_role_id IS NULL)
              OR brc.ai_role_id = ub.ai_role_id
          )
          AND brc.archived_at IS NULL
   );

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.messages m
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE m.tenant_id = bcc.tenant_id
   AND m.chat_id = bcc.chat_id
   AND m.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.semantic_facts sf
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE sf.tenant_id = bcc.tenant_id
   AND sf.chat_id = bcc.chat_id
   AND sf.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.episode_summaries es
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE es.tenant_id = bcc.tenant_id
   AND es.chat_id = bcc.chat_id
   AND es.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.active_episodes ae
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE ae.tenant_id = bcc.tenant_id
   AND ae.chat_id = bcc.chat_id
   AND ae.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.drafts d
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE d.tenant_id = bcc.tenant_id
   AND d.chat_id = bcc.chat_id
   AND d.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.draft_feedback df
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE df.tenant_id = bcc.tenant_id
   AND df.chat_id = bcc.chat_id
   AND df.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.extraction_log el
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE el.tenant_id = bcc.tenant_id
   AND el.chat_id = bcc.chat_id
   AND el.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.contact_profile_suggestions cps
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE cps.tenant_id = bcc.tenant_id
   AND cps.chat_id = bcc.chat_id
   AND cps.role_context_id IS NULL;

WITH bot_chat_contexts AS (
    SELECT c.tenant_id, c.chat_id, ub.active_role_context_id AS role_context_id
      FROM public.chats c
      JOIN public.user_bots ub
        ON ub.tenant_id = c.tenant_id
       AND c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')
     WHERE c.transport = 'bot'
       AND ub.active_role_context_id IS NOT NULL
)
UPDATE public.contact_profiles cp
   SET role_context_id = bcc.role_context_id
  FROM bot_chat_contexts bcc
 WHERE cp.tenant_id = bcc.tenant_id
   AND cp.chat_id = bcc.chat_id
   AND cp.role_context_id IS NULL;

COMMIT;
