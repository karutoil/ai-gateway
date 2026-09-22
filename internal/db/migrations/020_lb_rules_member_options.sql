-- 020_lb_rules_member_options.sql — rules may list one provider several times
-- A routing member is a specific (provider, model_override) OPTION: the same
-- provider can appear multiple times with different model overrides (e.g.
-- "openai + gpt-4o" and "openai + gpt-4o-mini" in one group). The 009 schema
-- constrained members to UNIQUE(model, provider_id), capping a provider at
-- one option per rule. Rebuild the table without that constraint — the
-- write path (lb.Store.ReplaceRule) enforces uniqueness of the
-- (provider, model_override) pair itself.
CREATE TABLE lb_rules_v2 (
	id TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	position INTEGER NOT NULL,
	created_at DATETIME NOT NULL,
	strategy TEXT NOT NULL DEFAULT 'round_robin',
	model_override TEXT NOT NULL DEFAULT '',
	weight INTEGER NOT NULL DEFAULT 1
);
INSERT INTO lb_rules_v2 (id, model, provider_id, position, created_at, strategy, model_override, weight)
	SELECT id, model, provider_id, position, created_at, strategy, model_override, weight FROM lb_rules;
DROP TABLE lb_rules;
ALTER TABLE lb_rules_v2 RENAME TO lb_rules;
CREATE INDEX IF NOT EXISTS idx_lb_rules_model ON lb_rules(model, position);
