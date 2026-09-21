-- 019: user-created model groups
--
-- Model groups are operator-curated aliases that map a single group name to
-- an ordered list of provider/model members. They reuse the same load-
-- balancer strategies as per-model routing rules (round_robin, random,
-- weighted, failover). When a client sends the group name as the model, the
-- gateway expands it to the group's member list and applies the strategy.
--
-- Use cases:
--   * "subagent-dispatcher" -> [openai/gpt-5, anthropic/claude-opus-4-6,
--                              opencode-go/deepseek-v4-flash]
--   * "fast-coding"         -> [opencode-zen/gpt-4o-mini, groq/llama-3.3-70b]
--
-- A member may carry a model_override, so a group can route several
-- different provider/model pairs under one gateway-facing name.

CREATE TABLE IF NOT EXISTS model_groups (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL DEFAULT '',
	strategy TEXT NOT NULL DEFAULT 'round_robin',
	created_at DATETIME NOT NULL,
	org_id TEXT DEFAULT NULL
);

CREATE INDEX IF NOT EXISTS idx_model_groups_name ON model_groups(name);
CREATE INDEX IF NOT EXISTS idx_model_groups_org ON model_groups(org_id);

CREATE TABLE IF NOT EXISTS model_group_members (
	id TEXT PRIMARY KEY,
	group_id TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	position INTEGER NOT NULL,
	model_override TEXT NOT NULL DEFAULT '',
	weight INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL,
	UNIQUE(group_id, provider_id),
	FOREIGN KEY (provider_id) REFERENCES providers(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_model_group_members_group ON model_group_members(group_id, position);