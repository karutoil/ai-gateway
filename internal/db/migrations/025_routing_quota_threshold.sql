-- 025: quota-aware routing. Values are percentages of quota consumed.
-- Existing rules receive the safe 90% default. A nullable column leaves room
-- for an explicit future disable mode without changing the schema again.
ALTER TABLE lb_rules ADD COLUMN quota_threshold_pct INTEGER;
UPDATE lb_rules SET quota_threshold_pct = 90 WHERE quota_threshold_pct IS NULL;
CREATE INDEX IF NOT EXISTS idx_lb_rules_model_position ON lb_rules(model, position);
