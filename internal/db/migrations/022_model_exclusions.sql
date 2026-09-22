-- 022_model_exclusions.sql — models an operator removed from a provider.
-- Discovery upserts every model the upstream lists, so a plain DELETE from
-- provider_models is undone by the next Discover (including the background
-- discover that runs when a provider is created). A row here is the durable
-- "do not auto-discover" mark for (provider, model). It is written when the
-- operator deletes a discovered model and cleared only when they add that
-- exact model back by hand. Provider deletion cascades the marks away.
CREATE TABLE IF NOT EXISTS provider_model_exclusions (
  id TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL,
  model_id TEXT NOT NULL,
  created_at DATETIME NOT NULL,
  FOREIGN KEY(provider_id) REFERENCES providers(id) ON DELETE CASCADE,
  UNIQUE(provider_id, model_id)
);
CREATE INDEX IF NOT EXISTS idx_provider_model_exclusions_provider ON provider_model_exclusions(provider_id);
