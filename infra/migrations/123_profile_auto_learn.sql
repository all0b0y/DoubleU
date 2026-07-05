-- 123: auto-apply profile suggestions (remove the owner-approval gate).
-- Profile-field suggestions previously waited in `pending` for owner review.
-- With auto-learn on (default), the Go post-turn path applies them immediately
-- and records a revert snapshot so the owner can undo from the Mini App.

-- Per-role switch: when true (default) profile suggestions auto-apply; when
-- false the legacy manual accept/reject queue is kept for cautious owners.
ALTER TABLE public.ai_roles
    ADD COLUMN IF NOT EXISTS auto_learn_profile boolean NOT NULL DEFAULT true;

-- previous_value: the profile field's value at auto-apply time (NULL when the
-- field was unset), so an owner-triggered revert can restore it.
-- auto_applied: distinguishes rows written by the auto-learn path (revertible)
-- from rows the owner accepted manually.
ALTER TABLE public.contact_profile_suggestions
    ADD COLUMN IF NOT EXISTS previous_value jsonb,
    ADD COLUMN IF NOT EXISTS auto_applied boolean NOT NULL DEFAULT false;

-- Allow the 'reverted' terminal status for undone auto-applied suggestions.
ALTER TABLE public.contact_profile_suggestions
    DROP CONSTRAINT IF EXISTS contact_profile_suggestions_status_check;
ALTER TABLE public.contact_profile_suggestions
    ADD CONSTRAINT contact_profile_suggestions_status_check
    CHECK (status = ANY (ARRAY['pending'::text, 'accepted'::text, 'rejected'::text, 'expired'::text, 'reverted'::text]));
