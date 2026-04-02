-- PostgreSQL Schema for Audit Records

-- Records table (no sessions or idempotency tables needed)
CREATE TABLE IF NOT EXISTS records (
    id TEXT PRIMARY KEY,
    message_id TEXT UNIQUE NOT NULL,
    session_id TEXT NOT NULL,
    seq_num BIGINT NOT NULL,
    timestamp TIMESTAMPTZ NOT NULL,
    type INTEGER NOT NULL,
    is_input BOOLEAN NOT NULL DEFAULT FALSE,
    data TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for queries
CREATE INDEX IF NOT EXISTS idx_records_session_id ON records(session_id);
CREATE INDEX IF NOT EXISTS idx_records_message_id ON records(message_id);
CREATE INDEX IF NOT EXISTS idx_records_seq_num ON records(session_id, seq_num);
