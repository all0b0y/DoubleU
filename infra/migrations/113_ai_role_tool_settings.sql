ALTER TABLE ai_roles
    ADD COLUMN IF NOT EXISTS tool_settings jsonb NOT NULL DEFAULT '{}'::jsonb;
