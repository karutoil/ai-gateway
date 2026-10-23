-- 036_lb_rules_org_scope.sql — partition routing rules by organization.
--
-- lb_rules was a single global table keyed only by `model`, while the admin API
-- accepted org-scoped callers and validated only that the referenced providers
-- belonged to the caller's org. The row it wrote was global, so:
--
--   * tenant A's admin could PUT /api/lb/rules/gpt-4o and re-point EVERY
--     tenant's gpt-4o traffic at org A's provider — a cross-tenant traffic and
--     billing hijack, and org A's credential then served other tenants' prompts;
--   * DELETE /api/lb/rules/gpt-4o from tenant A silently degraded routing for
--     everyone;
--   * GET /api/lb/rules returned every tenant's rules.
--
-- org_id is nullable on purpose: NULL keeps the pre-existing semantics of a
-- GLOBAL rule that every tenant resolves. Resolution (lb.Store.RuleForModelOrg)
-- prefers the caller's org rule and falls back to the global one, so existing
-- deployments behave exactly as before until an org-specific rule is written.
--
-- Purely additive DDL: one nullable column plus one index. Existing rows become
-- global (NULL), which is the safe default.
ALTER TABLE lb_rules ADD COLUMN org_id TEXT;

CREATE INDEX IF NOT EXISTS idx_lb_rules_model_org ON lb_rules(model, org_id, position);
