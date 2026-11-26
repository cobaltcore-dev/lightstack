-- Migration 063: persist the keypair name an instance was booted with, so Nova
-- server-detail (show + list-detail) can read it back and a repeated `terraform
-- apply` is a no-op (SPEC-017 / cloud-storage#524, finding F1).
-- Nullable: pre-existing rows read back as NULL (Nova's wire value for "no keypair").
ALTER TABLE instances ADD COLUMN IF NOT EXISTS key_name VARCHAR(255);
