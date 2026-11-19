CREATE TABLE token_revocations (
    token_hash TEXT PRIMARY KEY,
    expires_at_unix BIGINT NOT NULL
);

CREATE INDEX idx_token_revocations_expires_at
    ON token_revocations(expires_at_unix);
