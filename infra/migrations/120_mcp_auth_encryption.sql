-- 120: encrypt MCP auth headers at rest. auth_header (plaintext) is kept for
-- legacy rows; new writes go to auth_header_enc only. Drop of the plaintext
-- column is a follow-up once rows are re-saved (see docs/backlog.md).
ALTER TABLE public.mcp_servers ADD COLUMN IF NOT EXISTS auth_header_enc bytea;
