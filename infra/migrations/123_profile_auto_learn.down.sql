-- Revert 123: restore the owner-approval gate schema.

-- Move any 'reverted' rows to a legacy-valid status before tightening the CHECK.
UPDATE public.contact_profile_suggestions SET status = 'rejected' WHERE status = 'reverted';

ALTER TABLE public.contact_profile_suggestions
    DROP CONSTRAINT IF EXISTS contact_profile_suggestions_status_check;
ALTER TABLE public.contact_profile_suggestions
    ADD CONSTRAINT contact_profile_suggestions_status_check
    CHECK (status = ANY (ARRAY['pending'::text, 'accepted'::text, 'rejected'::text, 'expired'::text]));

ALTER TABLE public.contact_profile_suggestions
    DROP COLUMN IF EXISTS previous_value,
    DROP COLUMN IF EXISTS auto_applied;

ALTER TABLE public.ai_roles DROP COLUMN IF EXISTS auto_learn_profile;
