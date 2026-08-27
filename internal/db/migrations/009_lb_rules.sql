-- 009: load-balancer routing rules
-- Explicit, UI-curated provider groups per model name.
--   model       = bare or alias-resolved model id the rule governs (e.g. "gpt-4o-mini")
--   provider_id = member of the rotation group
--   position    = 0-based ordering used as the round-robin walk order
-- Rotation happens ACROSS requests (each request picks one member by offset);
-- there is deliberately NO within-request failover between members.
CREATE TABLE IF NOT EXISTS lb_rules (
	id TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	position INTEGER NOT NULL,
	created_at DATETIME NOT NULL,
	UNIQUE(model, provider_id)
);
CREATE INDEX IF NOT EXISTS idx_lb_rules_model ON lb_rules(model, position);
