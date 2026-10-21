-- 029_video_models.sql — video-generation model flag.
--
-- Video models (Sora, Veo, Grok Imagine) are a distinct model class: they are
-- invoked through the asynchronous POST /v1/videos job API (create -> poll ->
-- download content) rather than chat/images, and they bill per generated
-- second. Both model tables carry an explicit `video` flag so video models are
-- listable with the right capability and can never be routed chat or image
-- traffic. Default 0 keeps every existing row classified as text/image/decision.
ALTER TABLE models_catalog ADD COLUMN video INTEGER DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN video INTEGER DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_models_catalog_video ON models_catalog(video);
