-- SPEC-009 (WS-5 / FR-023): per-attachment delete-on-termination flag. When
-- true, deleting the server deletes the attached volume; when false (the
-- default), the volume is merely released (detached, status -> available).
-- Boot-from-volume with an existing volume defaults to false, so the volume
-- survives its server — matching upstream OpenStack semantics.
ALTER TABLE volume_attachments ADD COLUMN IF NOT EXISTS delete_on_termination boolean NOT NULL DEFAULT false;
