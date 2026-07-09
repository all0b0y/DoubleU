-- Revert 124.
ALTER TABLE public.ai_roles DROP COLUMN IF EXISTS auto_improve_skills;
DROP TABLE IF EXISTS public.skill_versions;
DROP TABLE IF EXISTS public.skill_usage;
