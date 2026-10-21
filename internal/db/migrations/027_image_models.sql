-- 027_image_models.sql — image-generation model support.
--
-- Image models (gpt-image-*, dall-e-*, Gemini Nano Banana / Imagen, xAI Grok
-- image) are a distinct model class from chat LLMs and from System One
-- decision models: they are invoked through POST /v1/images/generations (and
-- /v1/images/edits) rather than chat/completions, they output image bytes
-- rather than text, and they bill per generated image rather than (only) per
-- token. Both the catalog and the per-provider model rows carry an explicit
-- `image` flag so image models are listable with the right capability and can
-- never be silently routed chat traffic. Default 0 keeps every existing row
-- classified as a text/decision model.
ALTER TABLE models_catalog ADD COLUMN image INTEGER DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN image INTEGER DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_models_catalog_image ON models_catalog(image);
