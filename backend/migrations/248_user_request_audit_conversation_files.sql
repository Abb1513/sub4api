-- Payloads live exclusively in one atomic JSON document per conversation.
CREATE TABLE IF NOT EXISTS user_request_audit_conversations (
    id BIGINT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    request_count INTEGER NOT NULL,
    relative_path TEXT NOT NULL UNIQUE,
    summary JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS user_request_audit_conversations_updated_idx ON user_request_audit_conversations(updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS user_request_audit_conversations_scope_idx ON user_request_audit_conversations(user_id, api_key_id);
CREATE INDEX IF NOT EXISTS user_request_audit_conversations_expiry_idx ON user_request_audit_conversations(expires_at);
CREATE TABLE IF NOT EXISTS user_request_audit_request_index (
    id BIGINT PRIMARY KEY,
    conversation_id BIGINT NOT NULL REFERENCES user_request_audit_conversations(id) ON DELETE CASCADE,
    logical_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    metadata JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS user_request_audit_request_index_conversation_idx ON user_request_audit_request_index(conversation_id);
CREATE INDEX IF NOT EXISTS user_request_audit_request_index_logical_idx ON user_request_audit_request_index(logical_key);
CREATE INDEX IF NOT EXISTS user_request_audit_request_index_filters_idx ON user_request_audit_request_index((metadata->>'status'),(metadata->>'protocol'));
