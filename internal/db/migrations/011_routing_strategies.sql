-- 011: routing rule strategies and (provider, model) member pairs
--   strategy       = per-rule load-balancing strategy: round_robin (default),
--                    random, weighted, failover. Stored on every member row;
--                    all rows of a rule must agree (enforced on write).
--   model_override = optional per-member model id the request is rewritten to
--                    before the upstream hop ("" = use the rule's model name).
--   weight         = relative traffic share for the weighted strategy (1..100).
ALTER TABLE lb_rules ADD COLUMN strategy TEXT NOT NULL DEFAULT 'round_robin';
ALTER TABLE lb_rules ADD COLUMN model_override TEXT NOT NULL DEFAULT '';
ALTER TABLE lb_rules ADD COLUMN weight INTEGER NOT NULL DEFAULT 1;
