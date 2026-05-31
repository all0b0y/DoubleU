-- 006: provision a dedicated schema for LiteLLM Proxy's bookkeeping tables.
-- LiteLLM auto-creates spend / budget / virtualkey tables inside this schema
-- on first start so they do not collide with the DoubleU application schema.

CREATE SCHEMA IF NOT EXISTS litellm;
GRANT ALL ON SCHEMA litellm TO CURRENT_USER;
GRANT ALL ON ALL TABLES IN SCHEMA litellm TO CURRENT_USER;
ALTER DEFAULT PRIVILEGES IN SCHEMA litellm GRANT ALL ON TABLES TO CURRENT_USER;
