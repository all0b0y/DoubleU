-- Revert dynamic MCP toolsets.
DROP TABLE IF EXISTS public.role_mcp_access;
DROP INDEX IF EXISTS idx_mcp_servers_tenant;
DROP TABLE IF EXISTS public.mcp_servers;
