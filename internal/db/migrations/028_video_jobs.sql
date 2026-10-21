-- 028_video_jobs.sql — async video-generation jobs.
--
-- Video generation is a job, not a request: it runs for tens of seconds to
-- minutes, far beyond any HTTP budget, so the gateway returns a job handle
-- immediately and the client polls it. The gateway itself does NOT run a
-- background poller — a client poll is translated into a single upstream poll,
-- which keeps the gateway stateless and avoids burning provider quota for
-- clients that stopped caring. This table is the durable mapping between the
-- gateway-issued video id and the provider's own job handle, plus the last
-- observed state (so a finished job is served from here without re-polling,
-- and survives a gateway restart).
CREATE TABLE IF NOT EXISTS video_jobs (
  id            TEXT PRIMARY KEY,           -- gateway-issued id (video_...)
  provider_id   TEXT NOT NULL,              -- providers.id that owns the job
  key_prefix    TEXT,                       -- gateway key that created it (ownership)
  dialect       TEXT NOT NULL,              -- openai | xai | veo
  model         TEXT NOT NULL,              -- model id as requested by the client
  upstream_id   TEXT NOT NULL,              -- provider's job handle (request_id / operation name / video id)
  prompt        TEXT,
  seconds       TEXT,
  size          TEXT,
  status        TEXT NOT NULL DEFAULT 'queued',
  progress      INTEGER DEFAULT 0,
  error_code    TEXT,
  error_message TEXT,
  video_url     TEXT,                       -- provider-hosted URL once ready (when the provider returns one)
  created_at    DATETIME NOT NULL,
  updated_at    DATETIME NOT NULL,
  completed_at  DATETIME,
  expires_at    DATETIME
);
CREATE INDEX IF NOT EXISTS idx_video_jobs_key ON video_jobs(key_prefix);
CREATE INDEX IF NOT EXISTS idx_video_jobs_provider ON video_jobs(provider_id);
CREATE INDEX IF NOT EXISTS idx_video_jobs_created ON video_jobs(created_at);
