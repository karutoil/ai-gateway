-- 010_usage_logging.sql — richer per-request usage metadata + assembled stream bodies
-- All columns are additive and nullable/zero-default so existing rows and
-- every legacy query keep working unchanged.
--   finish_reason       normalized terminal reason: stop|length|tool_calls|content_filter
--                       (anthropic end_turn/max_tokens/tool_use mapped to these)
--   fallback_chain      JSON array of attempted providers before the final one:
--                       [{"provider_id","name","status"}] — NULL when none failed over
--   cache_read_tokens   anthropic usage.cache_read_input_tokens / openai prompt_tokens_details.cached_tokens
--   cache_write_tokens  anthropic usage.cache_creation_input_tokens
--   reasoning_tokens    openai completion_tokens_details.reasoning_tokens / anthropic thinking tokens
ALTER TABLE request_logs ADD COLUMN finish_reason TEXT;
ALTER TABLE request_logs ADD COLUMN fallback_chain TEXT;
ALTER TABLE request_logs ADD COLUMN cache_read_tokens INTEGER DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN cache_write_tokens INTEGER DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN reasoning_tokens INTEGER DEFAULT 0;
