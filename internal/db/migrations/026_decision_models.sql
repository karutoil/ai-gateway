-- 026_decision_models.sql — System One / decision-model support.
--
-- Decision models (TypeSafe Jev, Cloudflare Clef, open-weight Kev, llama.cpp
-- /v1/systemone) are a distinct model class: they answer typed questions with
-- calibrated probabilities instead of generating text. They need to be
-- listable (GET /v1/models), routable (POST /v1/systemone) and billable
-- (input-only pricing), so both the catalog and the per-provider model rows
-- carry an explicit `decision` flag. Default 0 keeps every existing row
-- classified as a text model.
ALTER TABLE models_catalog ADD COLUMN decision INTEGER DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN decision INTEGER DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_models_catalog_decision ON models_catalog(decision);
