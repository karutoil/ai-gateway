-- 023_model_recycling_bin.sql — snapshot of a removed provider model.
-- The exclusion table only remembers (provider, model) so discovery skips it.
-- The recycling bin keeps the row the operator removed, so restoring puts the
-- same model back (display name, pricing, capabilities, source) instead of
-- waiting for the next discovery to re-enrich it. Rows written before this
-- migration have a NULL snapshot and restore as a plain discovered model.
ALTER TABLE provider_model_exclusions ADD COLUMN snapshot TEXT;
