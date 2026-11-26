-- Rollback for Migration 065: real console tokens (SPEC-021).
--
-- Restores the two dropped columns with their original migration-005 types so a
-- downgraded binary still starts. Their CONTENTS are not restored — they were a
-- hash-derived port and an unused password, so there is nothing worth keeping.
DROP TABLE IF EXISTS console_auth_tokens;

ALTER TABLE instances ADD COLUMN IF NOT EXISTS console_vnc_port INTEGER;
ALTER TABLE instances ADD COLUMN IF NOT EXISTS console_vnc_password VARCHAR(255);
