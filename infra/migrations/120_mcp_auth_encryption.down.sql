-- Rollback 120: drop the encrypted-header column. Any secrets stored only in
-- auth_header_enc are lost on downgrade (legacy plaintext auth_header remains).
ALTER TABLE public.mcp_servers DROP COLUMN IF EXISTS auth_header_enc;
