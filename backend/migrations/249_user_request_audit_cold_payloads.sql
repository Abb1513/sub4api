-- Intentional clean cutover: old audit payloads are not imported or re-indexed.
-- Preserve the request high-water mark while making PostgreSQL allocate all IDs.
CREATE SEQUENCE IF NOT EXISTS user_request_audit_request_id_seq;
SELECT setval('user_request_audit_request_id_seq', GREATEST(
    COALESCE((SELECT MAX(id) FROM user_request_audit_request_index), 0),
    COALESCE((SELECT MAX(id) FROM user_request_audits), 0),
    (SELECT last_value FROM user_request_audit_request_id_seq),
    (SELECT last_value FROM user_request_audits_id_seq)
), true);
DELETE FROM user_request_audit_request_index;
DELETE FROM user_request_audit_conversations;
DROP TABLE user_request_audits;
ALTER TABLE user_request_audit_conversations DROP COLUMN relative_path;
ALTER TABLE user_request_audit_request_index ALTER COLUMN id SET DEFAULT nextval('user_request_audit_request_id_seq');
ALTER SEQUENCE user_request_audit_request_id_seq OWNED BY user_request_audit_request_index.id;
ALTER TABLE user_request_audit_request_index ALTER COLUMN conversation_id DROP NOT NULL;
ALTER TABLE user_request_audit_request_index ADD COLUMN request_path TEXT NOT NULL DEFAULT '';
ALTER TABLE user_request_audit_request_index ADD COLUMN response_path TEXT NOT NULL DEFAULT '';
ALTER TABLE user_request_audit_request_index ADD COLUMN payload_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE user_request_audit_request_index ADD COLUMN proof JSONB NOT NULL DEFAULT '{}';
CREATE UNIQUE INDEX user_request_audit_request_index_logical_unique ON user_request_audit_request_index(logical_key) WHERE logical_key <> '';
CREATE INDEX user_request_audit_request_index_scope_idx ON user_request_audit_request_index ((CAST(metadata->>'user_id' AS BIGINT)), (CAST(metadata->>'api_key_id' AS BIGINT)));
CREATE TABLE user_request_audit_payloads (
    request_id BIGINT NOT NULL REFERENCES user_request_audit_request_index(id) ON DELETE CASCADE,
    relative_path TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('request', 'response')),
    ordinal INTEGER NOT NULL,
    byte_size BIGINT NOT NULL,
    UNIQUE(request_id, ordinal)
);
CREATE TABLE user_request_audit_writer_lock (id INTEGER PRIMARY KEY, version INTEGER NOT NULL);
INSERT INTO user_request_audit_writer_lock(id, version) VALUES (1, 0);
CREATE TABLE user_request_audit_payload_gc (relative_path TEXT PRIMARY KEY);
