-- Rollback for Migration 063: Nova instance key_name read-back
ALTER TABLE instances DROP COLUMN IF EXISTS key_name;
