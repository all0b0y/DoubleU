-- 101: role context attachments.
-- Owners can attach reference material to a role (pricing, FAQ, product info —
-- pasted or extracted from text files in the Mini App). The content is injected
-- into the role-first draft prompt so the persona answers with that knowledge.
ALTER TABLE public.ai_roles
    ADD COLUMN IF NOT EXISTS context_notes text NOT NULL DEFAULT '';

COMMENT ON COLUMN public.ai_roles.context_notes IS
    'Owner-supplied reference material for the role (FAQ, pricing, docs). Plain text, injected into the draft prompt.';
