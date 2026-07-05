-- 122: personal-bot mode. When enabled, a user-supplied bot answers only the
-- owner and an explicit allowlist of Telegram users; everyone else gets a single
-- branded stub reply, then silence (no AI, nothing to memory).
ALTER TABLE public.user_bots
  ADD COLUMN IF NOT EXISTS personal_mode boolean NOT NULL DEFAULT false;

-- Per-bot allowlist of Telegram user ids the personal bot also serves (besides
-- the owner). Rows are removed with the bot.
CREATE TABLE IF NOT EXISTS public.personal_bot_allowed_users (
  bot_id           bigint      NOT NULL REFERENCES public.user_bots(bot_id) ON DELETE CASCADE,
  tenant_id        text        NOT NULL,
  telegram_user_id text        NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (bot_id, telegram_user_id)
);

CREATE INDEX IF NOT EXISTS idx_personal_bot_allowed_users_bot
  ON public.personal_bot_allowed_users (bot_id);

-- Tracks contacts that have already been shown the stub, so it is sent at most
-- once per conversation. chat_id is the bot-namespaced id ("bot:{botID}:{raw}"),
-- which uniquely identifies the contact conversation.
CREATE TABLE IF NOT EXISTS public.personal_bot_stub_shown (
  tenant_id text        NOT NULL,
  chat_id   text        NOT NULL,
  shown_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id)
);
