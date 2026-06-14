ALTER TABLE public.ai_roles DROP COLUMN IF EXISTS custom_fields;
ALTER TABLE public.user_bots DROP COLUMN IF EXISTS ai_role_id;
