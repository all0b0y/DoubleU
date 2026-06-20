-- 111: per-role "speak as owner" toggle.
-- true (default / legacy): the AI impersonates the owner — the contact perceives
-- the reply as written by a human. false: the AI replies openly as the role
-- persona (assistant / companion), with no owner impersonation.
ALTER TABLE public.ai_roles
  ADD COLUMN IF NOT EXISTS speak_as_owner boolean NOT NULL DEFAULT true;
