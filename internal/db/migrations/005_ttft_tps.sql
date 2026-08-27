-- 005_ttft_tps.sql — add TTFT and TPS support
ALTER TABLE request_logs ADD COLUMN ttft_ms INTEGER DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN response_ms INTEGER DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_request_logs_ttft ON request_logs(ttft_ms);
CREATE INDEX IF NOT EXISTS idx_request_logs_status ON request_logs(status);
