-- 006_request_error.sql — store upstream error for failed requests
ALTER TABLE request_logs ADD COLUMN error TEXT;
CREATE INDEX IF NOT EXISTS idx_request_logs_error ON request_logs(error) WHERE error IS NOT NULL;
ALTER TABLE request_logs ADD COLUMN request_body TEXT;
ALTER TABLE request_logs ADD COLUMN response_body TEXT;
