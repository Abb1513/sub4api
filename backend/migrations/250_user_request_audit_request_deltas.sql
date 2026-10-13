-- Existing cold snapshots remain readable; no payload is opened or rewritten.
ALTER TABLE user_request_audit_request_index ADD COLUMN request_parent_id BIGINT REFERENCES user_request_audit_request_index(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE user_request_audit_request_index ADD COLUMN request_prefix_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE user_request_audit_request_index ADD COLUMN request_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE user_request_audit_request_index ADD COLUMN request_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE user_request_audit_request_index ADD CONSTRAINT user_request_audit_request_delta_bounds CHECK (
    request_bytes >= 0 AND request_prefix_bytes >= 0 AND request_prefix_bytes <= request_bytes
    AND ((request_parent_id IS NULL AND request_prefix_bytes = 0)
      OR (request_parent_id < id AND request_prefix_bytes > 0))
);
CREATE INDEX user_request_audit_request_parent_idx ON user_request_audit_request_index(request_parent_id) WHERE request_parent_id IS NOT NULL;
