-- 037_mcp_key_configured.sql — remember that a key's MCP grants were explicitly
-- set, so "revoke all" can mean deny-all.
--
-- mcp_key_permissions is the grant table. "This key may use nothing" and "this
-- key was never configured" both had ZERO rows, and AccessForKey treats an empty
-- row set as the historical allow-everything default. The most common admin
-- action — clearing a key's MCP access — therefore granted the widest possible
-- access instead of removing it, and the dashboard showed the grants as removed.
--
-- A single marker row per configured key removes the ambiguity. Keys that were
-- never touched keep the permissive default (no behaviour change on upgrade).
CREATE TABLE IF NOT EXISTS mcp_key_configured (
	key_id TEXT PRIMARY KEY,
	configured_at DATETIME NOT NULL
);
