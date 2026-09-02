-- 016_webhook_format.sql
-- Per-webhook payload format. "json" (default) sends the full gateway event
-- envelope; "discord" and "slack" wrap it into those platforms' message
-- shape (Discord requires {"content": ...}, Slack {"text": ...}).
ALTER TABLE webhooks ADD COLUMN format TEXT NOT NULL DEFAULT 'json';
